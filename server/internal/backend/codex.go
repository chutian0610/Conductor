package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// codexBackend implements Backend by spawning the Codex CLI in one-shot
// non-interactive mode (codex exec --json).
//
// Wire protocol (line-delimited JSON on stdout):
//
//	{"type":"thread.started","thread_id":"..."}            — session banner
//	{"type":"turn.started"}                                — turn begins
//	{"type":"item.started","item":{...}}                   — item starts
//	  item.type = "agent_message"      → MessageText
//
// //	  item.type = "command_execution"  → MessageToolUse {Tool: "Bash", CallID}
//
//	  item.type = "error"              → MessageError
//	{"type":"item.completed","item":{...}}                 — item finished
//	  item.type = "command_execution"  → MessageToolResult {CallID, Output}
//	{"type":"turn.completed","usage":{...}}                — terminal
//
// Approval policy: --approve-for-me routes any tool permission requests
// through automatic review (workspace-write sandbox). Combined with
// --dangerously-bypass-approvals-and-sandbox this becomes fully
// unattended; we leave the sandbox policy to the user's config so the
// CLI's defaults are respected.
//
// V1.1 scope: single attempt, no resume, no MCP, no retries.
// V1.2 added:   resume subcommand + model override (-m) + auto-fallback
//
// //
// The protocol is structurally identical to Claude's stream-json, so
// codex.go mirrors claude.go's structure almost line-for-line. The big
// differences are:
//   - no stdin prompt write (the prompt is a positional argv argument)
//   - no control_request handshake (approval is a CLI flag, not a frame)
//   - the model is passed via -m, not a stream frame
type codexBackend struct {
	cfg Config
}

var codexTerminateGrace = 5 * time.Second

// compile-time interface check.
var _ Backend = (*codexBackend)(nil)

// ── Execute ───────────────────────────────────────────────────────────────

func (b *codexBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "codex"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("codex executable not found at %q: %w", execPath, err)
	}

	// ── Preflight (one-time, shared across fallback attempts) ──

	// Persist the brief into AGENTS.md so codex picks it up natively.
	if err := InjectRuntimeConfig(opts.Cwd, "codex", opts.SystemPrompt); err != nil {
		return nil, fmt.Errorf("inject codex runtime config: %w", err)
	}

	// Materialise the agent's MCP config into $CODEX_HOME/config.toml
	// when present. Mirrors multica codex.go:941-970. argv would echo
	// env-bearing secrets via ps/logs, so we go through a per-task
	// config.toml at 0o600 instead. CODEX_HOME falls back to ~/.codex
	// when unset (codex's documented default).
	if hasManagedCodexMcpConfig(opts.McpConfig) {
		codexHome := strings.TrimSpace(opts.Env["CODEX_HOME"])
		if codexHome == "" {
			if home, herr := os.UserHomeDir(); herr == nil {
				codexHome = filepath.Join(home, ".codex")
			}
		}
		if codexHome == "" {
			return nil, fmt.Errorf("codex: mcp_config is set but CODEX_HOME is not configured and home dir is unknown; cannot apply managed MCP")
		}
		if err := ensureCodexMcpConfig(filepath.Join(codexHome, "config.toml"), opts.McpConfig); err != nil {
			return nil, fmt.Errorf("apply codex mcp_config: %w", err)
		}
	}

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	go func() {
		defer close(msgCh)
		defer close(resCh)

		// ── Retry loop ──
		//
		// First attempt uses the caller's opts verbatim. If the prior
		// session is permanently unavailable AND the caller opted into
		// fallback (ResumeExpected + notice), retry with a fresh session
		// and the continuity notice prepended to the prompt. The retry
		// also re-writes AGENTS.md so the LLM sees the new context.
		currentOpts := opts
		currentPrompt := prompt
		var lastResult Result

		for attempt := 1; attempt <= 2; attempt++ {
			innerResCh := make(chan Result, 1)
			b.runOneAttempt(ctx, currentPrompt, currentOpts, msgCh, innerResCh)
			lastResult = <-innerResCh

			if !shouldFallbackToFreshSession(lastResult, currentOpts) {
				break
			}
			// Fall through to retry.
			currentOpts.ResumeSessionID = ""
			currentOpts.ResumeExpected = false // prevent infinite loop
			currentPrompt = resumeWithContinuityNotice(prompt, opts.ResumeContinuityNotice)
			if err := InjectRuntimeConfig(opts.Cwd, "codex", currentPrompt); err != nil {
				lastResult = Result{
					Status:     "failed",
					Error:      fmt.Sprintf("fallback: re-inject runtime config: %v", err),
					DurationMs: lastResult.DurationMs,
				}
				break
			}
		}
		resCh <- lastResult
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// runOneAttempt is a single codex exec invocation. It spawns the
// subprocess, streams events on msgCh, and sends exactly one Result on
// resCh before returning. The caller drives the retry loop so that the
// same channels (msgCh, resCh) are observed externally while multiple
// internal attempts may happen.
func (b *codexBackend) runOneAttempt(
	ctx context.Context,
	prompt string,
	opts ExecOptions,
	msgCh chan<- Message,
	resCh chan<- Result,
) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "codex"
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)
	defer cancel()

	args, dropped := buildCodexArgs(opts)
	for _, d := range dropped {
		b.cfg.Logger.Warn("codex: blocked user arg dropped by conductor", "flag", d.Flag, "took_value_with_it", d.TakesValue)
	}
	// The prompt is a positional argv entry. We always pass it last; any
	// user-supplied text that arrived via opts.SystemPrompt was already
	// persisted to AGENTS.md by InjectRuntimeConfig above.
	args = append(args, prompt)

	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	// Run codex in its own process group so a cancel reaches the whole
	// tree — the CLI plus any tool subprocesses it spawns.
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return nil }
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = mergeEnv(os.Environ(), b.cfg.Env, opts.Env)

	b.cfg.Logger.Info("agent command", "exec", execPath, "args", redactArgs(args))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		resCh <- Result{Status: "failed", Error: fmt.Sprintf("codex stdout pipe: %v", err), DurationMs: 0}
		return
	}
	// We never write to stdin — the prompt is positional argv — but
	// exec.CommandContext requires the pipe to exist before Start. Close
	// it immediately so the CLI's read returns EOF.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		resCh <- Result{Status: "failed", Error: fmt.Sprintf("codex stdin pipe: %v", err), DurationMs: 0}
		return
	}
	_ = stdin.Close()

	stderrBuf := newStderrTail(newSlogLineWriter(b.cfg.Logger, "[codex:stderr] "))
	cmd.Stderr = stderrBuf

	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		resCh <- Result{Status: "failed", Error: fmt.Sprintf("start codex: %v", err), DurationMs: 0}
		return
	}
	b.cfg.Logger.Info("codex started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	procDone := make(chan struct{})
	startTime := time.Now()

	defer func() { releaseProcessGroup(cmd) }()

	var sessionID, finalAnswer, lastAgentText string
	sawTurnCompleted := false
	usage := make(map[string]TokenUsage)
	eventCount := 0
	invalidEventCount := 0
	itemCount := 0
	commandCount := 0
	pendingCommands := map[string]string{} // callID → command (for MessageToolUse.Input)

	// Cancellation: SIGTERM the process group, escalate after grace.
	go func() {
		select {
		case <-procDone:
			return
		case <-runCtx.Done():
		}
		if cmd.Process != nil {
			signalProcessGroup(cmd, syscall.SIGTERM)
			if !waitProcessGroupGone(cmd, codexTerminateGrace) {
				signalProcessGroup(cmd, syscall.SIGKILL)
			}
		}
		_ = stdout.Close()
	}()

	scanner := newAgentStreamScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg codexEvent
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			invalidEventCount++
			continue
		}
		eventCount++

		switch msg.Type {
		case "thread.started":
			if msg.ThreadID != "" {
				sessionID = msg.ThreadID
			}
			trySend(msgCh, Message{Type: MessageStatus, Status: "running", SessionID: sessionID})
		case "turn.started":
			trySend(msgCh, Message{Type: MessageStatus, Status: "running"})
		case "item.started":
			itemCount++
			handleCodexItem(msg.Item, msgCh, &lastAgentText, pendingCommands, &commandCount, false)
		case "item.completed":
			itemCount++
			handleCodexItem(msg.Item, msgCh, &lastAgentText, pendingCommands, &commandCount, true)
		case "turn.completed":
			sawTurnCompleted = true
			if u := codexTurnUsage(msg.Usage); u != nil {
				usage = u
			}
		default:
			// Unknown top-level event — count but don't drop the
			// run. The CLI may add new event types in future versions.
			invalidEventCount++
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = stdout.Close()
	}

	exitErr := cmd.Wait()
	close(procDone)
	duration := time.Since(startTime)

	status, output, errMsg := finalizeCodexResult(
		timeout, runCtx.Err(), exitErr, sawTurnCompleted,
		scanErr, lastAgentText, finalAnswer, sessionID,
	)
	if errMsg != "" {
		errMsg = withStderrTail(errMsg, stderrBuf.Tail())
	}
	logProtocolSummary(b.cfg.Logger, "codex", opts.Model,
		streamProcessExitCode(exitErr), eventCount, invalidEventCount,
		itemCount, commandCount, sawTurnCompleted, false,
		scanErr != nil, len(lastAgentText), len(lastAgentText),
	)
	b.cfg.Logger.Info("codex finished", "pid", cmd.Process.Pid, "status", status,
		"duration", duration.Round(time.Millisecond).String())

	resCh <- Result{
		Status:     status,
		Output:     output,
		Error:      errMsg,
		DurationMs: duration.Milliseconds(),
		SessionID:  sessionID,
		Usage:      usage,
	}
}

// ── Codex CLI argument construction ───────────────────────────────────────

// buildCodexArgs assembles the argv passed to `codex exec --json`
// (or `codex exec resume --json` when ResumeSessionID is set).
//
// Fresh-exec flag surface:
//
//	--json                       : line-delimited JSON on stdout
//	--approve-for-me             : auto-approve via workspace-write sandbox
//	-C <dir>                     : working directory
//	-m <model>                   : model selection
//	-c model_reasoning_effort=X  : thinking effort (when ThinkingLevel set)
//
// Resume subcommand (`codex exec resume --json <id> "prompt"`):
//
//	-m <model>                   : allowed, overrides session's model
//	-c model_reasoning_effort=X  : allowed, per-turn config override
//	-C <dir>                     : REJECTED ("unexpected argument") — uses original session's cwd
//	--approve-for-me             : REJECTED — uses original session's approval policy
//
// V1.2 candidates (not implemented): sandbox override, --oss, --profile.
// buildCodexArgs assembles the argv passed to `codex exec --json`
// (or `codex exec resume --json` when ResumeSessionID is set). The
// second return value lists user args the blocklist dropped; the
// caller logs each entry so silently-ignored config doesn't slip by.
func buildCodexArgs(opts ExecOptions) (args []string, dropped []BlockedArg) {
	args = []string{"exec"}
	if opts.ResumeSessionID != "" {
		// codex exec resume accepts -m and -c but rejects -C and
		// --approve-for-me as "unexpected argument" (verified against
		// codex-cli 0.147.0). The resume subcommand uses the original
		// session's cwd and approval policy; only the model and
		// per-turn config overrides can change.
		args = append(args, "resume")
		if opts.Model != "" {
			args = append(args, "-m", opts.Model)
		}
		if opts.ThinkingLevel != "" {
			args = append(args, "-c", "model_reasoning_effort="+opts.ThinkingLevel)
		}
		args = append(args, "--json", opts.ResumeSessionID)
		return args, dropped
	}
	args = append(args, "--json", "--approve-for-me")
	if opts.Cwd != "" {
		args = append(args, "-C", opts.Cwd)
	}
	if opts.Model != "" {
		args = append(args, "-m", opts.Model)
	}
	if opts.ThinkingLevel != "" {
		args = append(args, "-c", "model_reasoning_effort="+opts.ThinkingLevel)
	}
	kept, d := codexFilterCustomArgs(opts.CustomArgs)
	args = append(args, kept...)
	dropped = d
	return args, dropped
}

var codexBlockedArgs = map[string]struct{}{
	"exec":             {},
	"resume":           {},
	"--json":           {},
	"--approve-for-me": {},
	"--dangerously-bypass-approvals-and-sandbox": {},
	"-m":                              {},
	"--model":                         {},
	"-C":                              {},
	"--cd":                            {},
	"-s":                              {},
	"--sandbox":                       {},
	"-c":                              {},
	"--config":                        {},
	"--oss":                           {},
	"--local-provider":                {},
	"-p":                              {},
	"--profile":                       {},
	"--dangerously-bypass-hook-trust": {},
	"--skip-git-repo-check":           {},
	"--ephemeral":                     {},
	"--ignore-user-config":            {},
	"--ignore-rules":                  {},
}

func codexFilterCustomArgs(userArgs []string) (kept []string, dropped []BlockedArg) {
	out := make([]string, 0, len(userArgs))
	skipNext := false
	for _, a := range userArgs {
		if skipNext {
			skipNext = false
			continue
		}
		if _, hit := codexBlockedArgs[a]; hit {
			takesValue := a == "-c" || a == "--model"
			if takesValue {
				skipNext = true
			}
			dropped = append(dropped, BlockedArg{Flag: a, TakesValue: takesValue})
			continue
		}
		out = append(out, a)
	}
	return out, dropped
}

// ── Codex wire types ─────────────────────────────────────────────────────

type codexEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id,omitempty"`
	Item     json.RawMessage `json:"item,omitempty"`
	Usage    *codexUsage     `json:"usage,omitempty"`
}

type codexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens,omitempty"`
}

type codexItem struct {
	ID               string `json:"id,omitempty"`
	Type             string `json:"type,omitempty"`
	Text             string `json:"text,omitempty"`
	Message          string `json:"message,omitempty"`
	Command          string `json:"command,omitempty"`
	AggregatedOutput string `json:"aggregated_output,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	Status           string `json:"status,omitempty"`
}

// ── Handlers ─────────────────────────────────────────────────────────────

// handleCodexItem dispatches an item.started or item.completed event.
//
// For tool events we capture the command on item.started (codex does
// not echo it back on item.completed) and emit the matching
// MessageToolUse / MessageToolResult pair. For agent_message we
// overwrite lastAgentText so the terminal Result has the right value.
// Reasoning (V1.2 — deferred; see followups row 9).
func handleCodexItem(
	raw json.RawMessage,
	ch chan<- Message,
	lastAgentText *string,
	pending map[string]string,
	commandCount *int,
	completed bool,
) {
	var item codexItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return
	}
	switch item.Type {
	case "agent_message":
		if completed && item.Text != "" {
			*lastAgentText = item.Text
			trySend(ch, Message{Type: MessageText, Content: item.Text})
		}
	case "command_execution":
		if item.ID == "" {
			return
		}
		if !completed {
			*commandCount++
			pending[item.ID] = item.Command
			trySend(ch, Message{
				Type:   MessageToolUse,
				Tool:   "Bash",
				CallID: item.ID,
				Input:  map[string]any{"command": item.Command, "status": item.Status},
			})
			return
		}
		command := pending[item.ID]
		delete(pending, item.ID)
		trySend(ch, Message{
			Type:   MessageToolResult,
			Tool:   "Bash",
			CallID: item.ID,
			Output: item.AggregatedOutput,
		})
		// Surface a non-zero exit code as an error message — the rest of
		// the protocol does not signal failure cleanly when a tool exits
		// non-zero and the agent may continue anyway.
		if item.ExitCode != nil && *item.ExitCode != 0 {
			trySend(ch, Message{
				Type:    MessageError,
				Content: fmt.Sprintf("command exited %d: %s", *item.ExitCode, command),
			})
		}
	case "file_change":
		// codex's file_change items don't carry a "tool name" the way the
		// app-server protocol did. Emit a generic patch event so the
		// transcript records the change; downstream consumers can map
		// "patch" back to whatever UI affordance they want.
		if item.ID == "" {
			return
		}
		msgType := MessageToolUse
		output := ""
		if completed {
			msgType = MessageToolResult
			output = item.Status
		}
		trySend(ch, Message{Type: msgType, Tool: "patch", CallID: item.ID, Output: output})
	case "mcp_tool_call":
		if item.ID == "" {
			return
		}
		// We don't currently know the tool name without parsing args;
		// surface whatever we can. V1.2 can lift the args map out of
		// the item payload.
		if completed {
			trySend(ch, Message{
				Type:   MessageToolResult,
				Tool:   "mcp",
				CallID: item.ID,
				Output: item.Status,
			})
		} else {
			trySend(ch, Message{
				Type:   MessageToolUse,
				Tool:   "mcp",
				CallID: item.ID,
			})
		}
	case "error":
		if item.Message != "" {
			trySend(ch, Message{Type: MessageError, Content: item.Message})
		}
	}
}

func codexTurnUsage(u *codexUsage) map[string]TokenUsage {
	if u == nil {
		return nil
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedInputTokens == 0 && u.CacheWriteInputTokens == 0 {
		return nil
	}
	return map[string]TokenUsage{
		// codex exec --json doesn't surface a model name in the usage
		// block. Use a stable key so downstream consumers don't have to
		// special-case the absence of a model identifier.
		"codex": {
			InputTokens:      u.InputTokens,
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  u.CachedInputTokens,
			CacheWriteTokens: u.CacheWriteInputTokens,
		},
	}
}

// finalizeCodexResult applies the fail-closed terminal contract. The
// flow mirrors claude's: structured terminal wins first, then
// infrastructure failures (timeout / cancel / scanner / exit / missing
// turn.completed).
func finalizeCodexResult(
	timeout time.Duration,
	runCtxErr error,
	exitErr error,
	sawTurnCompleted bool,
	scanErr error,
	lastAgentText string,
	finalAnswer string,
	sessionID string,
) (status string, output string, errMsg string) {
	status = "completed"

	// 1. Infrastructure failures first — they win even when the CLI also
	// emitted a structured error, because the structured frame is just
	// the CLI confirming what we already initiated.
	switch {
	case status == "completed" && errors.Is(runCtxErr, context.DeadlineExceeded):
		status = "timeout"
		errMsg = fmt.Sprintf("codex timed out after %s", timeout)
	case status == "completed" && errors.Is(runCtxErr, context.Canceled):
		status = "aborted"
		errMsg = "execution cancelled"
	case status == "completed" && scanErr != nil:
		status = "failed"
		errMsg = fmt.Sprintf("codex stdout read error: %v", scanErr)
	case status == "completed" && exitErr != nil:
		status = "failed"
		errMsg = fmt.Sprintf("codex exited with error: %v", exitErr)
	case status == "completed" && !sawTurnCompleted:
		status = "failed"
		errMsg = "codex stream ended without terminal turn.completed event"
	}

	if status != "completed" {
		return status, "", errMsg
	}
	if finalAnswer != "" {
		return status, finalAnswer, ""
	}
	return status, lastAgentText, ""
}

// Compile-time guard so accidental import cleanup doesn't drop io.
var _ = io.EOF
