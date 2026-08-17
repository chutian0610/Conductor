package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// claudeBackend implements Backend by spawning the Claude Code CLI in
// headless mode (claude -p). The wire protocol is Claude's
// stream-json: line-delimited JSON on stdout, one event per line.
//
// Event types:
//
//   {"type":"system",...}              — session banner (carries session_id)
//   {"type":"assistant",...}           — text / thinking / tool_use blocks
//   {"type":"user",...}                — tool_result blocks
//   {"type":"log",...}                 — structured log line
//   {"type":"control_request",...}     — permission / hook prompt (auto-allow)
//   {"type":"result",...}              — terminal: result text + usage
//
// The session id comes from the "system" event's session_id field.
// Approval policy: --permission-mode bypassPermissions makes claude
// auto-allow tools. The control_request handshake still fires for some
// flows; we answer with allow via stdin (handled in handleControlRequest).
type claudeBackend struct {
	cfg Config
}

var claudeTerminateGrace = 5 * time.Second

// compile-time interface check.
var _ Backend = (*claudeBackend)(nil)

// ── Execute ───────────────────────────────────────────────────────────────

func (b *claudeBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "claude"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("claude executable not found at %q: %w", execPath, err)
	}

	// ── Preflight (one-time, shared across fallback attempts) ──

	// If the caller provided an MCP config, write it to a temp file and
	// pass --mcp-config <path> so the agent uses a controlled set of MCP
	// servers instead of inheriting from the outer session.
	var mcpPath string
	if hasManagedMcpConfig(opts.McpConfig) {
		path, err := writeMcpConfigToTemp(opts.McpConfig)
		if err != nil {
			return nil, fmt.Errorf("write mcp config: %w", err)
		}
		mcpPath = path
		defer cleanupMcpConfigTemp(mcpPath)
	}

	// Persist the brief into CLAUDE.md so claude picks it up natively.
	if err := InjectRuntimeConfig(opts.Cwd, "claude", opts.SystemPrompt); err != nil {
		return nil, fmt.Errorf("inject claude runtime config: %w", err)
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
		// also re-writes CLAUDE.md so the LLM sees the new context.
		currentOpts := opts
		currentPrompt := prompt
		var lastResult Result

		for attempt := 1; attempt <= 2; attempt++ {
			lastResult = b.runOneAttempt(ctx, currentPrompt, currentOpts, mcpPath, msgCh)
			if !shouldFallbackToFreshSession(lastResult, currentOpts) {
				break
			}
			currentOpts.ResumeSessionID = ""
			currentOpts.ResumeExpected = false
			currentPrompt = resumeWithContinuityNotice(prompt, opts.ResumeContinuityNotice)
			if err := InjectRuntimeConfig(opts.Cwd, "claude", currentPrompt); err != nil {
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

// runOneAttempt is a single claude -p invocation. It returns a
// Result and uses callback functions to stream events on the caller's
// msgCh. The function is synchronous: it blocks until the subprocess
// exits (or is cancelled), then returns.
//
// The retry loop in Execute owns the channel lifetime; runOneAttempt
// just produces events + final result for a single attempt.
func (b *claudeBackend) runOneAttempt(
	ctx context.Context,
	prompt string,
	opts ExecOptions,
	mcpPath string,
	msgCh chan<- Message,
) Result {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "claude"
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)
	defer cancel()

	args := buildClaudeArgs(opts)
	if mcpPath != "" {
		args = append(args, "--mcp-config", mcpPath)
	}

	cmd := exec.CommandContext(runCtx, execPath, args...)
	hideAgentWindow(cmd)
	// Run claude in its own process group so cancellation reaches the
	// whole tree (the CLI + MCP servers + tool subprocesses), not just
	// the direct child — otherwise descendants keep running after a
	// cancel and burn model budget.
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
		return Result{Status: "failed", Error: fmt.Sprintf("claude stdout pipe: %v", err), DurationMs: 0}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{Status: "failed", Error: fmt.Sprintf("claude stdin pipe: %v", err), DurationMs: 0}
	}
	var closeStdinOnce sync.Once
	closeStdin := func() { closeStdinOnce.Do(func() { _ = stdin.Close() }) }

	stderrLineWriter := newSlogLineWriter(b.cfg.Logger, "[claude:stderr] ")
	stderrBuf := newStderrTail(stderrLineWriter)
	cmd.Stderr = stderrBuf

	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		return Result{Status: "failed", Error: fmt.Sprintf("start claude: %v", err), DurationMs: 0}
	}
	b.cfg.Logger.Info("claude started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	procDone := make(chan struct{})

	defer func() { releaseProcessGroup(cmd) }()
	defer closeStdin()

	startTime := time.Now()
	var lastAssistantText string
	var finalResultText string
	sawResult := false
	resultIsError := false
	var sessionID string
	usage := make(map[string]TokenUsage)
	eventCount := 0
	invalidEventCount := 0
	assistantEventCount := 0
	toolUseCount := 0

	// On cancellation / timeout, terminate the whole group BEFORE
	// unblocking the scanner.
	go func() {
		select {
		case <-procDone:
			return
		case <-runCtx.Done():
		}
		closeStdin()
		if cmd.Process != nil {
			signalProcessGroup(cmd, syscall.SIGTERM)
			if !waitProcessGroupGone(cmd, claudeTerminateGrace) {
				signalProcessGroup(cmd, syscall.SIGKILL)
			}
		}
		_ = stdout.Close()
	}()

	// writeClaudeInput runs in its own goroutine so it cannot deadlock
	// against the stdout reader.
	writeDone := make(chan error, 1)
	go func() {
		err := writeClaudeInput(stdin, prompt)
		if err != nil {
			closeStdin()
		}
		writeDone <- err
	}()

	scanner := newAgentStreamScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg claudeSDKMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			invalidEventCount++
			continue
		}
		eventCount++

		switch msg.Type {
		case "system":
			if msg.SessionID != "" {
				sessionID = msg.SessionID
			}
			trySend(msgCh, Message{Type: MessageStatus, Status: "running", SessionID: sessionID})
		case "assistant":
			assistantEventCount++
			turn := handleClaudeAssistant(msg, msgCh, usage)
			toolUseCount += turn.toolUses
			if !turn.understood {
				invalidEventCount++
			}
			lastAssistantText = turn.resolveFallback(lastAssistantText)
		case "user":
			if handleClaudeUser(msg, msgCh) {
				// Detected async background launch — refused.
				b.cfg.Logger.Warn("claude launched an async tool; not allowed in conductor mode",
					"tool_use_id", msg.RequestID)
			}
		case "result":
			sawResult = true
			finalResultText = msg.ResultText
			resultIsError = msg.IsError
			sessionID = msg.SessionID
			if u := claudeResultUsage(msg, opts.Model); len(u) > 0 {
				usage = u
			}
			closeStdin()
		case "log":
			if msg.Log != nil {
				trySend(msgCh, Message{Type: MessageLog, Level: msg.Log.Level, Content: msg.Log.Message})
			}
		case "control_request":
			handleClaudeControlRequest(msg, stdin, b.cfg.Logger)
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = stdout.Close()
	}
	closeStdin()

	exitErr := cmd.Wait()
	close(procDone)
	duration := time.Since(startTime)
	writeErr := <-writeDone

	status, output, errMsg := finalizeStreamResult(
		"claude", timeout, runCtx.Err(), writeErr, exitErr, sessionID,
		streamTerminalState{
			lastAssistantText: lastAssistantText,
			finalResultText:   finalResultText,
			sawResult:         sawResult,
			resultIsError:     resultIsError,
		},
	)
	if errMsg != "" {
		errMsg = withStderrTail(errMsg, stderrBuf.Tail())
	}
	logProtocolSummary(b.cfg.Logger, "claude", opts.Model,
		streamProcessExitCode(exitErr), eventCount, invalidEventCount,
		assistantEventCount, toolUseCount, sawResult, resultIsError,
		scanErr != nil, len(finalResultText), len(lastAssistantText),
	)
	b.cfg.Logger.Info("claude finished", "pid", cmd.Process.Pid, "status", status,
		"duration", duration.Round(time.Millisecond).String())

	return Result{
		Status:     status,
		Output:     output,
		Error:      errMsg,
		DurationMs: duration.Milliseconds(),
		SessionID:  sessionID,
		Usage:      usage,
	}
}



// ── Claude CLI argument construction ───────────────────────────────────────

// buildClaudeArgs assembles the argv passed to the Claude CLI. Backends
// own a small blocklist of flags the user cannot override (--model,
// --output-format, --mcp-config) so conductor stays in control of the
// protocol regardless of what the user puts in agent.yaml.
func buildClaudeArgs(opts ExecOptions) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		// bypassPermissions makes the CLI auto-allow tools without an
		// interactive prompt.
		"--permission-mode", "bypassPermissions",
		// AskUserQuestion is Claude Code's interactive question tool.
		"--disallowedTools", "AskUserQuestion",
	}
	if hasManagedMcpConfig(opts.McpConfig) {
		args = append(args, "--strict-mcp-config")
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.ThinkingLevel != "" {
		args = append(args, "--effort", opts.ThinkingLevel)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opts.MaxTurns))
	}
	if opts.ResumeSessionID != "" {
		// --resume goes BEFORE user CustomArgs so user input cannot
		// shadow it; the blocklist below also covers --resume.
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	args = append(args, filterCustomArgs(opts.CustomArgs)...)
	return args
}

var claudeBlockedArgs = map[string]struct{}{
	"-p":                                {},
	"--output-format":                   {},
	"--input-format":                    {},
	"--permission-mode":                 {},
	"--strict-mcp-config":               {},
	"--model":                           {},
	"--max-turns":                       {},
	"--mcp-config":                      {},
	"--session-name":                    {},
	"--resume":                          {},
	"--continue":                        {},
	"--dangerously-skip-permissions":    {},
	"-r":                                {},
}

func filterCustomArgs(userArgs []string) []string {
	out := make([]string, 0, len(userArgs))
	skipNext := false
	for _, a := range userArgs {
		if skipNext {
			skipNext = false
			continue
		}
		if _, blocked := claudeBlockedArgs[a]; blocked {
			switch a {
			case "--model", "--max-turns", "--mcp-config",
				"--output-format", "--input-format",
				"--permission-mode", "--session-name", "-r", "--resume":
				skipNext = true
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	return out
}

// ── Claude wire types ─────────────────────────────────────────────────────

type claudeSDKMessage struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message,omitempty"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Model     string          `json:"model,omitempty"`

	// result fields
	ResultText      string                            `json:"result,omitempty"`
	IsError         bool                              `json:"is_error,omitempty"`
	TerminalReason  string                            `json:"terminal_reason,omitempty"`
	DurationMs      float64                           `json:"duration_ms,omitempty"`
	NumTurns        int                               `json:"num_turns,omitempty"`
	Usage           *claudeUsage                      `json:"usage,omitempty"`
	ModelUsage      map[string]claudeResultModelUsage `json:"modelUsage,omitempty"`

	// log fields
	Log *claudeLogEntry `json:"log,omitempty"`

	// control request fields
	RequestID string          `json:"request_id,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
}

type claudeLogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

type claudeMessageContent struct {
	Role    string               `json:"role"`
	Model   string               `json:"model"`
	Content []claudeContentBlock `json:"content"`
	Usage   *claudeUsage         `json:"usage,omitempty"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type claudeResultModelUsage struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
}

type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type claudeControlRequestPayload struct {
	Subtype  string          `json:"subtype"`
	ToolName string          `json:"tool_name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
}

// ── Handlers ─────────────────────────────────────────────────────────────

type assistantTurn struct {
	text       string
	toolUses   int
	understood bool
}

func (t assistantTurn) resolveFallback(prior string) string {
	switch {
	case t.toolUses > 0 || !t.understood:
		return ""
	case t.text != "":
		return t.text
	default:
		return prior
	}
}

func handleClaudeAssistant(msg claudeSDKMessage, ch chan<- Message, usage map[string]TokenUsage) assistantTurn {
	var content claudeMessageContent
	if err := json.Unmarshal(msg.Message, &content); err != nil {
		return assistantTurn{}
	}
	turn := assistantTurn{understood: true}
	var assistantText strings.Builder
	toolUseCount := 0

	if content.Usage != nil && content.Model != "" {
		u := usage[content.Model]
		u.InputTokens += content.Usage.InputTokens
		u.OutputTokens += content.Usage.OutputTokens
		u.CacheReadTokens += content.Usage.CacheReadInputTokens
		u.CacheWriteTokens += content.Usage.CacheCreationInputTokens
		usage[content.Model] = u
	}

	for _, block := range content.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				assistantText.WriteString(block.Text)
				trySend(ch, Message{Type: MessageText, Content: block.Text})
			}
		case "thinking":
			if block.Text != "" {
				trySend(ch, Message{Type: MessageThinking, Content: block.Text})
			}
		case "tool_use":
			toolUseCount++
			var input map[string]any
			if block.Input != nil {
				_ = json.Unmarshal(block.Input, &input)
			}
			trySend(ch, Message{
				Type:   MessageToolUse,
				Tool:   block.Name,
				CallID: block.ID,
				Input:  input,
			})
		default:
			turn.understood = false
		}
	}
	turn.text = assistantText.String()
	turn.toolUses = toolUseCount
	return turn
}

func handleClaudeUser(msg claudeSDKMessage, ch chan<- Message) bool {
	var content claudeMessageContent
	if err := json.Unmarshal(msg.Message, &content); err != nil {
		return false
	}
	sawAsyncLaunch := false
	for _, block := range content.Content {
		if block.Type == "tool_result" {
			resultStr := ""
			if block.Content != nil {
				resultStr = string(block.Content)
				if claudeToolResultHasAsyncLaunch(block.Content) {
					sawAsyncLaunch = true
				}
			}
			trySend(ch, Message{
				Type:   MessageToolResult,
				CallID: block.ToolUseID,
				Output: resultStr,
			})
		}
	}
	return sawAsyncLaunch
}

func handleClaudeControlRequest(msg claudeSDKMessage, stdin io.Writer, logger *slog.Logger) {
	var req claudeControlRequestPayload
	if err := json.Unmarshal(msg.Request, &req); err != nil {
		return
	}
	var inputMap map[string]any
	if req.Input != nil {
		_ = json.Unmarshal(req.Input, &inputMap)
	}
	if inputMap == nil {
		inputMap = map[string]any{}
	}
	if v, ok := inputMap["run_in_background"].(bool); ok && v {
		inputMap["run_in_background"] = false
		logger.Info("claude: forced foreground tool execution",
			"request_id", msg.RequestID,
			"tool", req.ToolName,
		)
	}

	response := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": msg.RequestID,
			"response": map[string]any{
				"behavior":     "allow",
				"updatedInput": inputMap,
			},
		},
	}
	data, err := json.Marshal(response)
	if err != nil {
		logger.Warn("claude: failed to marshal control response", "error", err)
		return
	}
	data = append(data, '\n')
	if _, err := stdin.Write(data); err != nil {
		logger.Warn("claude: failed to write control response", "error", err)
	}
}

func trySend(ch chan<- Message, msg Message) {
	defer func() { _ = recover() }()
	select {
	case ch <- msg:
	default:
	}
}

func hasManagedMcpConfig(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

func claudeResultUsage(msg claudeSDKMessage, fallbackModel string) map[string]TokenUsage {
	if len(msg.ModelUsage) > 0 {
		usage := make(map[string]TokenUsage, len(msg.ModelUsage))
		for model, u := range msg.ModelUsage {
			if model == "" || !claudeUsageHasTokens(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens) {
				continue
			}
			usage[model] = TokenUsage{
				InputTokens:      u.InputTokens,
				OutputTokens:     u.OutputTokens,
				CacheReadTokens:  u.CacheReadInputTokens,
				CacheWriteTokens: u.CacheCreationInputTokens,
			}
		}
		if len(usage) > 0 {
			return usage
		}
	}
	model := msg.Model
	if model == "" {
		model = fallbackModel
	}
	if msg.Usage == nil || model == "" || !claudeUsageHasTokens(
		msg.Usage.InputTokens,
		msg.Usage.OutputTokens,
		msg.Usage.CacheReadInputTokens,
		msg.Usage.CacheCreationInputTokens,
	) {
		return nil
	}
	return map[string]TokenUsage{
		model: {
			InputTokens:      msg.Usage.InputTokens,
			OutputTokens:     msg.Usage.OutputTokens,
			CacheReadTokens:  msg.Usage.CacheReadInputTokens,
			CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
		},
	}
}

func claudeUsageHasTokens(input, output, cacheRead, cacheWrite int64) bool {
	return input > 0 || output > 0 || cacheRead > 0 || cacheWrite > 0
}

// ── Shared helpers (also used by codex) ─────────────────────────────────

func claudeTerminalReasonFailure(terminalReason, resultText string) string {
	if strings.TrimSpace(terminalReason) != taskfailureTerminalReasonPromptTooLong {
		return ""
	}
	msg := "claude ended the turn with terminal_reason=" + taskfailureTerminalReasonPromptTooLong +
		": the session's context window is exhausted and compaction could not recover it"
	if detail := strings.TrimSpace(resultText); detail != "" {
		msg += " (" + detail + ")"
	}
	return msg
}

func writeMcpConfigToTemp(raw json.RawMessage) (string, error) {
	dir, err := os.MkdirTemp("", "conductor-mcp-*")
	if err != nil {
		return "", fmt.Errorf("create mcp config temp dir: %w", err)
	}
	path := dir + string(os.PathSeparator) + "mcp-config.json"
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("write mcp config temp file: %w", err)
	}
	return path, nil
}

func cleanupMcpConfigTemp(path string) {
	if dir := pathSepParent(path); dir != "" {
		_ = os.RemoveAll(dir)
	}
}

func pathSepParent(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == os.PathSeparator || path[i] == '/' {
			return path[:i]
		}
	}
	return ""
}

func writeClaudeInput(w io.Writer, prompt string) error {
	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]string{
				{"type": "text", "text": prompt},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal claude input: %w", err)
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func claudeToolResultHasAsyncLaunch(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch x := v.(type) {
	case map[string]any:
		if claudeMapHasAsyncLaunchStatus(x) {
			return true
		}
		if content, ok := x["content"].([]any); ok {
			return claudeArrayHasAsyncLaunchStatus(content)
		}
	case []any:
		return claudeArrayHasAsyncLaunchStatus(x)
	}
	return false
}

func claudeArrayHasAsyncLaunchStatus(values []any) bool {
	for _, v := range values {
		if item, ok := v.(map[string]any); ok && claudeMapHasAsyncLaunchStatus(item) {
			return true
		}
	}
	return false
}

func claudeMapHasAsyncLaunchStatus(v map[string]any) bool {
	status, ok := v["status"].(string)
	return ok && status == "async_launched"
}

// ── Helpers shared with codex (and any future JSON-stream backend) ────

// mergeEnv merges base (os.Environ form), processEnv (Config.Env) and
// execEnv (ExecOptions.Env) into a single []string. execEnv wins over
// processEnv, which wins over base. Nil entries are dropped.
func mergeEnv(base []string, processEnv, execEnv map[string]string) []string {
	merged := make(map[string]string, len(base))
	for _, kv := range base {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		merged[kv[:i]] = kv[i+1:]
	}
	for k, v := range processEnv {
		merged[k] = v
	}
	for k, v := range execEnv {
		merged[k] = v
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}

// taskfailureTerminalReasonPromptTooLong is the value of the
// `terminal_reason` field on Claude's `result` event when the session
// ran out of context window.
const taskfailureTerminalReasonPromptTooLong = "prompt_too_long"

// claudeTerminalReasonFailure turns Claude's structured terminal_reason
// into an error string when the reason means the turn did not actually
// produce an answer.
