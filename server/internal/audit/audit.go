// Package audit implements the adversarial audit loop (ADR-0009):
// given a recorded run, spawn a fresh LLM subprocess that reads
// the events transcript and emits a single-line JSON verdict
// (`pass | fail | unverifiable` + one-sentence evidence). The result
// is persisted to the `run_audits` table.
//
// Trust model: judgment, never enforcement. This package does not
// change the run's `status`, gate any future conductor call, or
// affect CI exit codes. Operators read the verdict and decide.
//
// Design references:
//   - ADR-0009 (this repo) — design source of truth.
//   - multica server/pkg/agent/goal_manager.py:run_audit — upstream
//     reference; we mirror the "fresh subprocess, no in-process
//     bias" structure but tighten the verdict contract and persist
//     in our own SQLite table.
package audit

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"conductor/server/internal/agentregistry"
	"conductor/server/internal/backend"
)

// auditorPrompt is the system prompt we give the auditor LLM. It is
// intentionally short so the auditor's "what to do" fits in a few
// hundred tokens and the auditor LLM spends most of its context on
// the transcript itself.
//
// Output contract (intentional, do not change without an ADR amend):
//
//	{"verdict": "<pass|fail|unverifiable>", "evidence": "<1-3 sentences>"}
//
// The auditor must emit exactly one JSON object on its first non-blank
// line. Tools are disallowed on the auditor subprocess.
const auditorPrompt = `You are an adversarial reviewer. A run of an AI agent
is provided below in plain text. Your job is to judge whether the run
went as expected — not whether the answer was correct in any deeper
sense, but whether the run was coherent: prompts were answered,
tool calls that errored were either retried or surfaced, the agent
did not loop, and the terminal Result is consistent with the events.

Verdict values:

  pass            every event is consistent with the result; no
                  errors, no obvious failures, no contradictions.
  fail            something went clearly wrong — a tool errored and
                  the agent silently ignored it; the agent overran
                  its brief; or the final result contradicts an
                  earlier event.
  unverifiable    insufficient information to judge (the transcript
                  was truncated, parse errors were reported without
                  resolution, or key context was missing).

Output exactly one JSON object on the first non-blank line of your
response. Do not call any tools. Do not emit any prose before or
after the JSON object. Shape:

  {"verdict": "<pass|fail|unverifiable>", "evidence": "<1-3 sentences>"}

The evidence string should mention the specific event id(s) and the
specific claim from the transcript that drove your verdict.`

// transcriptMaxChars caps how big an event transcript we'll feed to
// the auditor. 500 KB of text is roughly 125k tokens at the high end
// (claude reads it as input); larger transcripts need summarising
// first, which is a followup if real runs hit the cap.
const transcriptMaxChars = 500_000

// auditTimeout is the default wall-clock bound on a single audit
// subprocess. Zero in Options uses this.
const auditTimeout = 5 * time.Minute

// Options configures one audit invocation.
type Options struct {
	// Force re-audits an already-audited run. When false (default),
	// an audit on an already-audited run returns ErrAlreadyAudited.
	Force bool

	// Model overrides the auditor's model. Empty string means "use
	// the backend's default" (claude will pick its default model).
	// Honoured by `conductor audit --model <name>`.
	Model string

	// AuditorBackend forces the backend; empty means "claude". V2
	// supports claude only; codex would need a different prompt
	// contract and is a future ADR.
	AuditorBackend string

	// Logger receives the auditor subprocess's protocol metadata.
	// Nil falls back to slog.Default().
	Logger *slog.Logger

	// Timeout caps the auditor subprocess wall-clock. Zero means
	// auditTimeout.
	Timeout time.Duration
}

// Result is the audit outcome the operator sees. The same row is
// persisted to `run_audits`; this struct is the in-memory view.
type Result struct {
	AuditID      int64  `json:"audit_id"`
	RunID        int64  `json:"run_id"`
	Verdict      string `json:"verdict"` // pass | fail | unverifiable
	Evidence     string `json:"evidence"`
	AuditorModel string `json:"auditor_model"`
	AuditedAt    int64  `json:"audited_at"`
}

// ErrAlreadyAudited is returned by Run when the run has a non-pending
// audit row and Options.Force is false. The caller (CLI) maps this
// to a non-zero exit + a readable error.
var ErrAlreadyAudited = fmt.Errorf("audit: run already audited (use --force to re-audit)")

// Run executes one audit invocation against the given run. The
// auditor LLM is spawned fresh via the existing backend driver; the
// caller does not need to know about backends.
func Run(ctx context.Context, reg *agentregistry.Store, runID int64, opts Options) (Result, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Timeout == 0 {
		opts.Timeout = auditTimeout
	}
	backendName := opts.AuditorBackend
	if backendName == "" {
		backendName = "claude"
	}
	if backendName != "claude" {
		return Result{}, fmt.Errorf("audit: backend %q not supported in V2 (claude only)", backendName)
	}

	// 1. Load run + events.
	run, err := reg.GetRun(ctx, runID)
	if err != nil {
		return Result{}, fmt.Errorf("audit: load run: %w", err)
	}
	events, err := reg.Events(ctx, runID)
	if err != nil {
		return Result{}, fmt.Errorf("audit: load events: %w", err)
	}
	if !isAuditableStatus(run.Status) {
		return Result{}, fmt.Errorf("audit: run status %q is not auditable yet", run.Status)
	}

	// 2. Refuse re-audit unless --force.
	if !opts.Force {
		existing, ok, err := reg.GetLatestAudit(ctx, runID)
		if err != nil {
			return Result{}, err
		}
		if ok && existing.Verdict != "pending" {
			return Result{}, ErrAlreadyAudited
		}
	}

	// 3. Build the auditor's input and the hashes that pin them.
	transcript := renderTranscript(run, events)
	inputHash := sha256Hex(transcript)
	promptHash := sha256Hex(auditorPrompt)
	now := time.Now().UnixMilli()

	// 4. Insert a pending audit row (recoverable if the subprocess
	//    crashes mid-flight).
	auditID, err := reg.StartAudit(ctx, agentregistry.RunAudit{
		RunID:        runID,
		AuditorModel: opts.Model, // may be "" if operator passed none
		AuditedAt:    now,
		InputSHA:     inputHash,
		PromptSHA:    promptHash,
	})
	if err != nil {
		return Result{}, fmt.Errorf("audit: start row: %w", err)
	}

	// 5. Spawn the auditor subprocess. The transcript is the user
	//    prompt; the auditor instructions live in SystemPrompt
	//    (persisted to CLAUDE.md by the backend's preflight).
	auditorBackend, err := backend.New("claude", backend.Config{
		Logger: opts.Logger,
	})
	if err != nil {
		_ = reg.FinishAudit(ctx, auditID, "unverifiable",
			fmt.Sprintf("auditor backend construction failed: %v", err))
		return Result{}, fmt.Errorf("audit: backend: %w", err)
	}

	customArgs := []string{"--disallowed-tools", ""}
	if opts.Model != "" {
		customArgs = append(customArgs, "--model", opts.Model)
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	session, err := auditorBackend.Execute(runCtx, transcript, backend.ExecOptions{
		MaxTurns:     1,
		Timeout:      opts.Timeout,
		SystemPrompt: auditorPrompt,
		CustomArgs:   customArgs,
	})
	if err != nil {
		_ = reg.FinishAudit(ctx, auditID, "unverifiable",
			fmt.Sprintf("auditor subprocess failed to start: %v", err))
		return Result{}, fmt.Errorf("audit: spawn: %w", err)
	}
	// Drain events into the void — auditor messages are not surfaced.
	for range session.Messages {
	}
	res := <-session.Result
	if res.Status != "completed" {
		_ = reg.FinishAudit(ctx, auditID, "unverifiable",
			fmt.Sprintf("auditor exited with status %q (err: %s)", res.Status, res.Error))
		return Result{}, fmt.Errorf("audit: auditor exit status %s: %s", res.Status, res.Error)
	}

	// 6. Parse the verdict from the auditor's final output.
	verdict, evidence := parseVerdict(res.Output)
	if err := reg.FinishAudit(ctx, auditID, verdict, evidence); err != nil {
		return Result{}, fmt.Errorf("audit: persist verdict: %w", err)
	}
	opts.Logger.Info("audit: completed",
		"run_id", runID, "audit_id", auditID, "verdict", verdict)
	return Result{
		AuditID:      auditID,
		RunID:        runID,
		Verdict:      verdict,
		Evidence:     evidence,
		AuditorModel: opts.Model,
		AuditedAt:    time.Now().UnixMilli(),
	}, nil
}

// isAuditableStatus gates audit to terminal states — a still-running
// run will keep emitting events after we've snapshotted, so the
// transcript we'd build would be incomplete and verdict noise.
func isAuditableStatus(status string) bool {
	switch status {
	case "completed", "failed", "timeout", "cancelled", "aborted":
		return true
	}
	return false
}

// renderTranscript joins the run's metadata + events into a single
// text block for the auditor. The format is intentionally
// human-ish (not JSON) — auditor models are widely tuned on
// Markdown-shaped review prompts.
func renderTranscript(run agentregistry.Run, events []agentregistry.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run #%d\n", run.ID)
	fmt.Fprintf(&b, "  agent_id      : %d\n", run.AgentID)
	fmt.Fprintf(&b, "  status        : %s\n", run.Status)
	fmt.Fprintf(&b, "  started_at    : %d\n", run.StartedAt.UnixMilli())
	if run.EndedAt != nil {
		fmt.Fprintf(&b, "  ended_at      : %d\n", run.EndedAt.UnixMilli())
	}
	fmt.Fprintf(&b, "  duration_ms   : %d\n", run.DurationMs)
	fmt.Fprintf(&b, "  session_id    : %s\n", run.SessionID)
	if run.Error != "" {
		fmt.Fprintf(&b, "  error         : %s\n", run.Error)
	}
	if len(run.Usage) > 0 {
		ub, _ := json.Marshal(run.Usage)
		fmt.Fprintf(&b, "  usage         : %s\n", ub)
	}
	fmt.Fprintf(&b, "  prompt_sha    : %s\n", run.PromptSHA)
	fmt.Fprintf(&b, "  events        : %d rows\n\n", len(events))
	for _, e := range events {
		fmt.Fprintf(&b, "  [event %d kind=%s]\n", e.ID, e.Kind)
		if e.Payload != nil {
			var pretty any
			if err := json.Unmarshal(e.Payload, &pretty); err == nil {
				if pp, err := json.MarshalIndent(pretty, "    ", "  "); err == nil {
					b.Write(pp)
				} else {
					b.Write(e.Payload)
				}
			} else {
				b.Write(e.Payload)
			}
		}
		b.WriteByte('\n')
	}
	out := b.String()
	if len(out) > transcriptMaxChars {
		out = out[:transcriptMaxChars] +
			fmt.Sprintf("\n...[transcript truncated; %d bytes dropped]...\n",
				len(out)-transcriptMaxChars)
	}
	return out
}

// ParseVerdict reads the auditor's terminal Output and produces the
// (verdict, evidence) pair. Exported for direct unit testing without
// spawning a subprocess.
//
// Failure modes (all converge on `unverifiable`):
//
//   - empty output
//   - output that doesn't start with `{`
//   - JSON that doesn't unmarshal to {verdict, evidence}
//   - JSON that parses but `verdict` is not in the allowed set
//
// In every case the evidence string carries enough context for an
// operator to re-run manually.
func ParseVerdict(stdout string) (verdict, evidence string) {
	return parseVerdict(stdout)
}

func parseVerdict(stdout string) (verdict, evidence string) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return "unverifiable", "auditor returned empty output"
	}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var firstLine string
	if scanner.Scan() {
		firstLine = strings.TrimSpace(scanner.Text())
	}
	if firstLine == "" {
		return "unverifiable", "auditor returned empty first line"
	}
	if !strings.HasPrefix(firstLine, "{") {
		return "unverifiable", "auditor returned non-JSON output: " + truncateForEvidence(firstLine)
	}
	type response struct {
		Verdict  string `json:"verdict"`
		Evidence string `json:"evidence"`
	}
	var r response
	if err := json.Unmarshal([]byte(firstLine), &r); err != nil {
		return "unverifiable",
			fmt.Sprintf("auditor returned malformed JSON: %s (%v)",
				truncateForEvidence(firstLine), err)
	}
	if r.Verdict == "" || r.Evidence == "" {
		return "unverifiable",
			fmt.Sprintf("auditor JSON missing field(s): %s", truncateForEvidence(firstLine))
	}
	switch r.Verdict {
	case "pass", "fail", "unverifiable":
		return r.Verdict, r.Evidence
	default:
		return "unverifiable",
			fmt.Sprintf("auditor emitted unknown verdict %q: %s",
				r.Verdict, truncateForEvidence(r.Evidence))
	}
}

// truncateForEvidence caps the auditor's output string when it's
// carrying it back in our evidence fallback (don't pollute the run
// audit row with hundreds of KB of stdout).
func truncateForEvidence(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// sha256Hex returns the lowercase-hex sha256 of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
