package backend

// Pure-Go unit tests for helpers that are too small / pure to
// deserve their own file but deserve to be exercised. They lift
// coverage on the bits of resume_fallback.go and runtime_config.go
// that the integration tests do not hit directly.

import (
	"strings"
	"testing"
	"time"
)

func TestProviderNeedsInlineSystemPrompt_DefaultsFalse(t *testing.T) {
	// V1 has no provider needing inline delivery; both known ones
	// read CLAUDE.md / AGENTS.md natively. This is the gate for
	// future providers — keep it green until a provider lands.
	for _, p := range []string{"claude", "codex", "grok", "kimi", ""} {
		if providerNeedsInlineSystemPrompt(p) {
			t.Errorf("providerNeedsInlineSystemPrompt(%q) = true, want false", p)
		}
	}
}

func TestShouldFallbackToFreshSession_NeverWhenNotExpected(t *testing.T) {
	// ResumeExpected=false is the strict case: any failure is the
	// caller's problem, never a fallback.
	for _, status := range []string{"completed", "failed", "timeout", "aborted"} {
		if shouldFallbackToFreshSession(Result{Status: status}, ExecOptions{}) {
			t.Errorf("fallback fired without ResumeExpected (status=%s)", status)
		}
	}
}

func TestShouldFallbackToFreshSession_OnlyOnPermanentFailures(t *testing.T) {
	yes := ExecOptions{ResumeExpected: true, ResumeSessionID: "thr-prev"}

	for _, status := range []string{"completed", "timeout", "aborted"} {
		if shouldFallbackToFreshSession(Result{Status: status}, yes) {
			t.Errorf("fallback fired for status=%s (not a permanent failure)", status)
		}
	}
}

func TestResumeWithContinuityNotice_PrependsBanner(t *testing.T) {
	got := resumeWithContinuityNotice("the original prompt", "PREVIOUS SESSION WAS LOST")
	if !strings.Contains(got, "PREVIOUS SESSION WAS LOST") {
		t.Errorf("notice not prepended: %q", got)
	}
	if !strings.Contains(got, "the original prompt") {
		t.Errorf("original prompt missing: %q", got)
	}
	if !strings.HasPrefix(got, "PREVIOUS SESSION WAS LOST") {
		t.Errorf("notice is not at the head: %q", got)
	}
}

func TestResumeWithContinuityNotice_EmptyNoticePassesThrough(t *testing.T) {
	got := resumeWithContinuityNotice("the prompt", "")
	if got != "the prompt" {
		t.Errorf("empty notice should leave prompt untouched, got %q", got)
	}
}

func TestRunContext_NoTimeoutReturnsCancellableContext(t *testing.T) {
	ctx, cancel := runContext(t.Context(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Errorf("zero timeout should not set a deadline")
	}
}

func TestRunContext_PositiveTimeoutAppliesDeadline(t *testing.T) {
	ctx, cancel := runContext(t.Context(), 100*time.Millisecond)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("positive timeout did not set a deadline")
	}
	if time.Until(deadline) > 200*time.Millisecond {
		t.Errorf("deadline far in the future; got %v", time.Until(deadline))
	}
}
