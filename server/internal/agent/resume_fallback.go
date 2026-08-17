package agent

import "strings"

// isResumeSessionGone returns true if the result indicates the prior
// session is permanently unavailable. Permanent failures trigger the
// fallback retry; transient failures (transport, rate limit, auth)
// surface as the run's error so the caller can retry later.
//
// The heuristic is intentionally string-based: both backends report
// resume failures with free-text errors, and the list of "permanent"
// signatures is small and stable enough to be matched by substring.
//
// V1.2 covers: "no rollout found" (codex), "session not found" (claude).
// V1.3 will add: "image too large" (multica GH#5975), schema drift.
func isResumeSessionGone(res Result) bool {
	if res.Status != "failed" {
		return false
	}
	errStr := strings.ToLower(res.Error)
	return strings.Contains(errStr, "no rollout found") ||
		strings.Contains(errStr, "no conversation found") ||
		strings.Contains(errStr, "session not found") ||
		strings.Contains(errStr, "thread not found") ||
		strings.Contains(errStr, "no such conversation")
}

// shouldFallbackToFreshSession returns true if a retry without the
// resume pointer is appropriate. Conservative: only kicks in when the
// caller set ResumeExpected AND the prior session is permanently gone
// AND we have a continuity notice to give the LLM some context.
//
// Returning false here means: surface the failure to the operator and
// stop. This is the safe default — we'd rather have a noisy failure
// than a silent wrong answer.
func shouldFallbackToFreshSession(res Result, opts ExecOptions) bool {
	if !opts.ResumeExpected {
		return false
	}
	if opts.ResumeSessionID == "" {
		// Already on a fresh session; nothing to fall back to.
		return false
	}
	if opts.ResumeContinuityNotice == "" {
		// No continuity notice means the LLM would silently lose
		// all context. Refuse to fall back rather than mis-attribute.
		return false
	}
	return isResumeSessionGone(res)
}

// resumeWithContinuityNotice is the prompt transformation applied
// during fallback. The original prompt stays intact; the notice is
// prepended with a clear separator so the LLM sees the difference
// between "task" and "context about this task".
func resumeWithContinuityNotice(prompt, notice string) string {
	if notice == "" {
		return prompt
	}
	if prompt == "" {
		return notice
	}
	return notice + "\n\n---\n\n" + prompt
}
