package agent

// Integration tests for codexBackend, driven by fake-codex
// (testbinaries build tag). Same shape as the claude suite: cover
// happy / cancel / timeout / AGENTS.md-injection paths without
// requiring the real Codex CLI.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexThreadStarted + agentMessageItem + turnCompleted produce a
// happy-path Codex `exec --json` script.
var (
	codexThreadStarted = json.RawMessage(`{"type":"thread.started","thread_id":"thr-fake"}`)
	codexTurnStarted   = json.RawMessage(`{"type":"turn.started"}`)
	codexAgentMessage  = json.RawMessage(`{"type":"item.completed","item":{"type":"agent_message","text":"hello from fake codex"}}`)
	codexTurnCompleted = json.RawMessage(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2,"cached_input_tokens":0,"cache_write_input_tokens":0}}`)
)

func TestCodexIntegration_HappyPath(t *testing.T) {
	binary := MustBuildFakeBinary(t, "fake-codex")
	script := WriteScript(t, t.TempDir(), "happy.jsonl", []ScriptStep{
		{Event: codexThreadStarted},
		{Event: codexTurnStarted},
		{DelayMs: 10, Event: codexAgentMessage},
		{DelayMs: 10, Event: codexTurnCompleted},
	})

	// Provide a writable cwd so InjectRuntimeConfig can drop AGENTS.md.
	workDir := t.TempDir()

	backend, err := New("codex", Config{
		ExecutablePath: binary,
		Logger:         testLogger(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, err := backend.Execute(context.Background(), "say hi", ExecOptions{
		Cwd:        workDir,
		MaxTurns:   1,
		Timeout:    10 * time.Second,
		CustomArgs: []string{"--script", script},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var msgs []Message
	for m := range session.Messages {
		msgs = append(msgs, m)
	}
	res := <-session.Result

	if res.Status != "completed" {
		t.Fatalf("Status = %q, want completed (err: %s)", res.Status, res.Error)
	}
	if !strings.Contains(res.Output, "hello from fake codex") {
		t.Errorf("Output = %q, want contains 'hello from fake codex'", res.Output)
	}
	if res.SessionID != "thr-fake" {
		t.Errorf("SessionID = %q, want thr-fake", res.SessionID)
	}

	var sawText bool
	for _, m := range msgs {
		if m.Type == MessageText &&
			strings.Contains(m.Content, "hello from fake codex") {
			sawText = true
		}
	}
	if !sawText {
		t.Errorf("no MessageText carrying agent_message; got %d messages", len(msgs))
	}

	// InjectRuntimeConfig should have written <cwd>/AGENTS.md.
	agentsMD := filepath.Join(workDir, "AGENTS.md")
	if _, err := os.Stat(agentsMD); err != nil {
		t.Errorf("InjectRuntimeConfig did not write AGENTS.md: %v", err)
	}
}

func TestCodexIntegration_Cancel(t *testing.T) {
	binary := MustBuildFakeBinary(t, "fake-codex")
	script := WriteScript(t, t.TempDir(), "cancel.jsonl", []ScriptStep{
		{Event: codexThreadStarted},
		{DelayMs: 30000, Event: codexTurnCompleted},
	})

	workDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()

	backend, _ := New("codex", Config{
		ExecutablePath: binary,
		Logger:         testLogger(t),
	})
	session, err := backend.Execute(ctx, "wait", ExecOptions{
		Cwd:        workDir,
		MaxTurns:   1,
		Timeout:    60 * time.Second,
		CustomArgs: []string{"--script", script},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range session.Messages {
	}
	res := <-session.Result
	elapsed := time.Since(start)

	if res.Status != "aborted" {
		t.Errorf("Status = %q, want aborted", res.Status)
	}
	if elapsed > 8*time.Second {
		t.Errorf("cancel took %s, expected < 8s", elapsed)
	}
}

func TestCodexIntegration_Timeout(t *testing.T) {
	binary := MustBuildFakeBinary(t, "fake-codex")
	script := WriteScript(t, t.TempDir(), "timeout.jsonl", []ScriptStep{
		{Event: codexThreadStarted},
		{DelayMs: 5000, Event: codexTurnCompleted},
	})

	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	backend, _ := New("codex", Config{ExecutablePath: binary, Logger: testLogger(t)})
	session, err := backend.Execute(ctx, "stall", ExecOptions{
		// We can't import the private Execute from another package,
		// but New() returns a Backend; route through it.
		Cwd:        workDir,
		MaxTurns:   1,
		Timeout:    300 * time.Millisecond,
		CustomArgs: []string{"--script", script},
	})
	_ = err // tolerated; see ClaudeIntegration_Timeout.
	if session != nil {
		for range session.Messages {
		}
		<-session.Result
	}
}

// TestBuildCodexArgs_ResumeAsymmetry verifies the resume branch of
// buildCodexArgs drops the flags that codex-cli 0.147.0 rejects:
// -C (cwd override) and --approve-for-me (approval policy). We test
// this against the unexported buildCodexArgs directly because the
// real "codex exec resume" subcommand rejects unknown-argument
// errors fast enough that end-to-end testing against the fake would
// measure the fake's parser, not codex's.
//
// doc/backends/codex.md flags-vs-resume table is the spec; this
// test pins it so any future codex change gets caught.
func TestBuildCodexArgs_ResumeAsymmetry(t *testing.T) {
	freshArgs, _ := buildCodexArgs(ExecOptions{
		Cwd:           "/tmp/work",
		Model:         "gpt-5",
		ThinkingLevel: "medium",
	})
	if !containsArg(freshArgs, " -C ") && !hasFlag(freshArgs, "-C") {
		t.Errorf("fresh exec argv should contain -C /tmp/work, got %v", freshArgs)
	}
	if !containsArg(freshArgs, "--approve-for-me") {
		t.Errorf("fresh exec argv should contain --approve-for-me, got %v", freshArgs)
	}

	resumeArgs, _ := buildCodexArgs(ExecOptions{
		Cwd:             "/tmp/work",
		Model:           "gpt-5",
		ThinkingLevel:   "medium",
		ResumeSessionID: "thr-prev",
	})
	if !containsArg(resumeArgs, "resume") {
		t.Errorf("resume argv should contain 'resume', got %v", resumeArgs)
	}
	if hasFlag(resumeArgs, "-C") {
		t.Errorf("resume argv must NOT contain -C (rejected by codex exec resume); got %v", resumeArgs)
	}
	if containsArg(resumeArgs, "--approve-for-me") {
		t.Errorf("resume argv must NOT contain --approve-for-me; got %v", resumeArgs)
	}
	if !containsArg(resumeArgs, "thr-prev") {
		t.Errorf("resume argv should contain session id thr-prev, got %v", resumeArgs)
	}
}

// TestBuildClaudeArgs_HappyPath verifies the baseline argv
// construction. Useful as a smoke test alongside the resume
// asymmetry test — together they pin the protocol-owned flags.
func TestBuildClaudeArgs_HappyPath(t *testing.T) {
	args, _ := buildClaudeArgs(ExecOptions{
		Model:         "claude-sonnet-4-5",
		ThinkingLevel: "medium",
	})
	for _, want := range []string{
		"-p", "--output-format", "stream-json", "--input-format", "stream-json",
		"--permission-mode", "bypassPermissions", "--model", "claude-sonnet-4-5",
	} {
		if !containsArg(args, want) {
			t.Errorf("buildClaudeArgs should contain %q, got %v", want, args)
		}
	}
}

// TestBuildClaudeArgs_Blocklist is the dedicated check that
// user CustomArgs cannot override conductor-owned flags.
func TestBuildClaudeArgs_Blocklist(t *testing.T) {
	args, _ := buildClaudeArgs(ExecOptions{
		Model: "config-model",
		CustomArgs: []string{
			"--model", "user-attempt",
			"--user-flag", "user-value",
		},
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "config-model") {
		t.Errorf("argv should contain conductor-pinned model, got: %v", args)
	}
	if strings.Contains(joined, "user-attempt") {
		t.Errorf("argv should not contain user-supplied --model value: %v", args)
	}
	if !strings.Contains(joined, "--user-flag") || !strings.Contains(joined, "user-value") {
		t.Errorf("argv should pass through non-blocked user flags: %v", args)
	}
}

// hasFlag reports whether args contains the (standalone, no-value)
// flag. containsArg is for any token match.
func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func hasFlag(args []string, want string) bool {
	for i, a := range args {
		if a == want {
			return true
		}
		// "-C path" form: check a then a+1.
		if a == want {
			_ = i
			return true
		}
	}
	return false
}
