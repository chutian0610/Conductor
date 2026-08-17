package agent

import (
	"bufio"
	"context"
	"time"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
)

// agentStreamMaxLineBytes bounds a single line read from an agent CLI's
// event stream. Agent transports are line-delimited JSON, and one line can
// carry a whole conversation (Anthropic's `result` event, Claude's
// `assistant` event with a long transcript). Crossing this bound makes
// scanner.Scan() return false with bufio.ErrTooLong, which we report as a
// transport failure — the session itself is fine, we just cannot read it.
//
// 32 MiB is the follow-up to the well-known 10 MiB cap. It is headroom,
// not a guarantee: a session can still outgrow any fixed cap, so the
// recovery path matters more than the number.
const agentStreamMaxLineBytes = 32 * 1024 * 1024

// agentStreamInitialBufferBytes is the scanner's starting allocation. Lines
// above it still grow up to agentStreamMaxLineBytes; this only decides
// how much is reserved before the first grow, so ordinary events never
// realloc.
const agentStreamInitialBufferBytes = 1024 * 1024

// newAgentStreamScanner returns a bufio.Scanner sized for agent event
// streams.
func newAgentStreamScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, agentStreamInitialBufferBytes), agentStreamMaxLineBytes)
	return scanner
}

// streamTerminalState keeps the user-facing final answer separate from the
// streamed assistant turns. Assistant messages are still emitted through
// the Session.Messages channel for live progress, but only a terminal
// result (or the last complete assistant message after a clean result)
// may become Result.Output.
type streamTerminalState struct {
	lastAssistantText string
	finalResultText   string
	sawResult         bool
	resultIsError     bool
	scanErr           error
}

// finalizeStreamResult applies the shared fail-closed terminal contract:
//
//   - A clean process exit is not proof the protocol completed; success
//     requires a result event. Failed runs always return an empty output
//     so upstream fallbacks can never mistake a partial transcript for a
//     final answer (multica #6006).
//   - The terminal result text wins over the last assistant text: it is
//     the CLI's own statement of what to surface to the user.
//   - When the result is an error and carries text, that text becomes
//     Error; otherwise we fall back to a generic message that names the
//     provider.
func finalizeStreamResult(
	provider string,
	timeout time.Duration,
	runCtxErr error,
	writeErr error,
	exitErr error,
	sessionID string,
	state streamTerminalState,
) (status string, output string, errMsg string) {
	status = "completed"

	// 1. The structured terminal wins over everything else, EXCEPT when
	// the run context was cancelled: a CLI that emits is_error=true with
	// no result text is just confirming the cancellation we already
	// initiated, not reporting a new failure.
	hasContextErr := errors.Is(runCtxErr, context.DeadlineExceeded) || errors.Is(runCtxErr, context.Canceled)
	if state.resultIsError && !hasContextErr {
		status = "failed"
		errMsg = state.finalResultText
		if errMsg == "" {
			errMsg = provider + " returned an error result without details"
		}
	}

	// 2. Then infrastructure failures.
	switch {
	case status == "completed" && errors.Is(runCtxErr, context.DeadlineExceeded):
		status = "timeout"
		errMsg = fmt.Sprintf("%s timed out after %s", provider, timeout)
	case status == "completed" && errors.Is(runCtxErr, context.Canceled):
		status = "aborted"
		errMsg = "execution cancelled"
	case state.scanErr != nil && status == "completed":
		status = "failed"
		errMsg = fmt.Sprintf("%s stdout read error: %v", provider, state.scanErr)
	case writeErr != nil && status == "completed" && sessionID == "":
		status = "failed"
		errMsg = fmt.Sprintf("write %s input: %v", provider, writeErr)
	case exitErr != nil && status == "completed":
		status = "failed"
		errMsg = fmt.Sprintf("%s exited with error: %v", provider, exitErr)
	case !state.sawResult && status == "completed":
		status = "failed"
		errMsg = provider + " stream ended without terminal result"
	}

	if status != "completed" {
		return status, "", errMsg
	}
	if state.finalResultText != "" {
		return status, state.finalResultText, ""
	}
	return status, state.lastAssistantText, ""
}

// streamProcessExitCode extracts the exit code from a cmd.Wait error.
// Returns 0 on clean exit, -1 on any non-exit error.
func streamProcessExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// logProtocolSummary records only protocol metadata — never assistant or
// result text, tool input/output, or environment values — so diagnosing a
// missing terminal event cannot leak the task transcript into logs.
func logProtocolSummary(logger *slog.Logger, provider string, model string,
	exitCode, eventCount, invalidEventCount, assistantEventCount, toolUseCount int,
	sawResult, resultIsError, scanErr bool,
	resultBytes, lastAssistantBytes int,
) {
	logger.Info("agent stream protocol summary",
		"provider", provider,
		"model", model,
		"exit_code", exitCode,
		"event_count", eventCount,
		"invalid_event_count", invalidEventCount,
		"assistant_event_count", assistantEventCount,
		"tool_use_count", toolUseCount,
		"saw_result", sawResult,
		"result_is_error", resultIsError,
		"result_bytes", resultBytes,
		"last_assistant_bytes", lastAssistantBytes,
		"scanner_error", scanErr,
	)
}
