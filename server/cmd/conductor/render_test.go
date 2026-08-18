package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"conductor/server/internal/backend"
)

// These tests pin the user-facing CLI output to a deterministic shape.
// The renderer layer is the only thing operators see — once --json or
// --quiet lands on stdout, downstream tooling greps for `tokens[`,
// `▸`, `—`, etc. A regression here would silently break scripts
// the team has running without anyone noticing. Golden strings below
// are exact-match assertions; if you change them, you are changing
// the CLI contract.

func TestRenderMessage_Text(t *testing.T) {
	got := renderOne(t, backend.Message{Type: backend.MessageText, Content: "hi"})
	want := "▸ hi\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderMessage_ThinkingTruncates(t *testing.T) {
	long := strings.Repeat("a", thinkingPreviewChars+50)
	got := renderOne(t, backend.Message{Type: backend.MessageThinking, Content: long})
	wantEllipsis := "… " + strings.Repeat("a", thinkingPreviewChars) + "…\n"
	if got != wantEllipsis {
		t.Fatalf("got %q want prefix %q", got, wantEllipsis)
	}
}

func TestRenderMessage_ThinkingShortNoTruncate(t *testing.T) {
	short := strings.Repeat("a", 10) // < thinkingPreviewChars
	got := renderOne(t, backend.Message{Type: backend.MessageThinking, Content: short})
	want := "… " + short + "\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderMessage_ToolUse(t *testing.T) {
	in := map[string]any{"cmd": "ls", "path": "/tmp"}
	got := renderOne(t, backend.Message{
		Type:  backend.MessageToolUse,
		Tool:  "Bash",
		Input: in,
	})
	// compactJSON keeps keys in declaration order. Pin the literal
	// output so any future reformat shows up as a diff.
	want := "🔧 Bash " + `{"cmd":"ls","path":"/tmp"}` + "\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderMessage_ToolUse_NilInput(t *testing.T) {
	// sanitizeMap on nil/empty returns the input unchanged, so
	// compactJSON sees an empty map → empty string → trailing space
	// before the newline. Pin it so any future reformat is intentional.
	got := renderOne(t, backend.Message{
		Type: backend.MessageToolUse,
		Tool: "Bash",
	})
	want := "🔧 Bash \n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderMessage_ToolResultTruncates(t *testing.T) {
	long := strings.Repeat("z", toolOutputPreviewChars+10)
	got := renderOne(t, backend.Message{
		Type:   backend.MessageToolResult,
		Output: long,
	})
	want := "↳ " + strings.Repeat("z", toolOutputPreviewChars) + "…\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderMessage_Status(t *testing.T) {
	got := renderOne(t, backend.Message{Type: backend.MessageStatus, Status: "running"})
	want := "· running\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderMessage_Error(t *testing.T) {
	got := renderOne(t, backend.Message{Type: backend.MessageError, Content: "boom"})
	want := "⚠ boom\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderMessage_Log(t *testing.T) {
	got := renderOne(t, backend.Message{
		Type:    backend.MessageLog,
		Level:   "WARN",
		Content: "rate limited",
	})
	want := "[WARN] rate limited\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderMessage_StripsANSI(t *testing.T) {
	// ESC-prefixed ANSI in any string field is scrubbed by the
	// renderer — brackets (Claude variant identifiers) survive.
	in := "\x1b[1mhi\x1b[0m (variant[1m])"
	got := renderOne(t, backend.Message{Type: backend.MessageText, Content: in})
	want := "▸ hi (variant[1m])\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderMessage_UnknownTypeProducesNothing(t *testing.T) {
	// Future MessageType values fall through the switch; the operator
	// sees nothing rather than a half-rendered line. Pin the contract.
	got := renderOne(t, backend.Message{Type: "future-thing", Content: "x"})
	if got != "" {
		t.Fatalf("got %q want empty", got)
	}
}

// --- renderResult -------------------------------------------------------

func TestRenderResult_BasicCompleted(t *testing.T) {
	var stderr, stdout bytes.Buffer
	renderResult(&stderr, &stdout, backend.Result{
		Status:     "completed",
		DurationMs: 1234,
	})
	wantStderr := "\n— COMPLETED (1234 ms) —\n"
	if stderr.String() != wantStderr {
		t.Fatalf("stderr: got %q want %q", stderr.String(), wantStderr)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout should be empty for empty Output: %q", stdout.String())
	}
}

func TestRenderResult_WithSessionUsageError(t *testing.T) {
	var stderr, stdout bytes.Buffer
	renderResult(&stderr, &stdout, backend.Result{
		Status:     "failed",
		DurationMs: 17,
		SessionID:  "sess-X",
		Usage: map[string]backend.TokenUsage{
			"claude-sonnet-4-5": {InputTokens: 10, OutputTokens: 20, CacheReadTokens: 5},
		},
		Error: "boom",
		Output: "final body",
	})
	stderrWant := "" +
		"\n— FAILED (17 ms) —\n" +
		"session: sess-X\n" +
		"tokens[claude-sonnet-4-5]: in=10 out=20 cache_r=5 cache_w=0\n" +
		"error: boom\n"
	if stderr.String() != stderrWant {
		t.Fatalf("stderr mismatch:\ngot:  %q\nwant: %q", stderr.String(), stderrWant)
	}
	if stdout.String() != "final body\n" {
		t.Fatalf("stdout mismatch: %q", stdout.String())
	}
}

// --- emitUsage ----------------------------------------------------------

func TestEmitUsage_Empty(t *testing.T) {
	var buf bytes.Buffer
	emitUsage(&buf, map[string]backend.TokenUsage{})
	if buf.Len() != 0 {
		t.Fatalf("expected no output, got %q", buf.String())
	}
}

func TestEmitUsage_FullLineShape(t *testing.T) {
	var buf bytes.Buffer
	emitUsage(&buf, map[string]backend.TokenUsage{
		"claude-sonnet-4-5": {InputTokens: 100, OutputTokens: 50, CacheReadTokens: 128, CacheWriteTokens: 0},
	})
	want := "tokens[claude-sonnet-4-5]: in=100 out=50 cache_r=128 cache_w=0\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}

func TestEmitUsage_PreservesClaudeVariant(t *testing.T) {
	// ansiclean.Strip keeps bracket content like MiniMax-M3[1m] intact.
	var buf bytes.Buffer
	emitUsage(&buf, map[string]backend.TokenUsage{
		"MiniMax-M3[1m]": {InputTokens: 1, OutputTokens: 2},
	})
	want := "tokens[MiniMax-M3[1m]]: in=1 out=2 cache_r=0 cache_w=0\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}

func TestEmitUsage_MultipleModels(t *testing.T) {
	// Iteration order is map-iteration dependent; assert both lines
	// are present rather than a specific order.
	var buf bytes.Buffer
	emitUsage(&buf, map[string]backend.TokenUsage{
		"a": {InputTokens: 1, OutputTokens: 1},
		"b": {InputTokens: 2, OutputTokens: 2},
	})
	got := buf.String()
	if !strings.Contains(got, "tokens[a]: in=1 out=1 cache_r=0 cache_w=0\n") {
		t.Fatalf("missing line for a: %q", got)
	}
	if !strings.Contains(got, "tokens[b]: in=2 out=2 cache_r=0 cache_w=0\n") {
		t.Fatalf("missing line for b: %q", got)
	}
}

// --- emitJSON -----------------------------------------------------------

func TestEmitJSON_Shape(t *testing.T) {
	var buf bytes.Buffer
	emitJSON(&buf, "event", backend.Message{Type: backend.MessageText, Content: "hi"})
	got := strings.TrimSpace(buf.String())
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("not valid JSON: %v: %s", err, got)
	}
	if decoded["kind"] != "event" {
		t.Fatalf("kind: %v", decoded["kind"])
	}
	payload, ok := decoded["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload not a map: %T", decoded["payload"])
	}
	if payload["Content"] != "hi" {
		t.Fatalf("payload.Content: %v", payload["Content"])
	}
}

func TestEmitJSON_AlwaysSingleLine(t *testing.T) {
	// Operator greps `{"kind":"event"` per-line. Multi-line payloads
	// must be collapsed — json.Encoder with no indent emits that.
	var buf bytes.Buffer
	emitJSON(&buf, "result", map[string]any{
		"nested": map[string]any{
			"key": "value",
		},
	})
	got := buf.String()
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("expected exactly one newline (terminator): %q", got)
	}
}

// --- compactJSON --------------------------------------------------------

func TestCompactJSON_EmptyMap(t *testing.T) {
	if got := compactJSON(map[string]any{}); got != "" {
		t.Fatalf("got %q want empty", got)
	}
}

func TestCompactJSON_SingleLine(t *testing.T) {
	got := compactJSON(map[string]any{"k": "v"})
	want := `{"k":"v"}`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// --- truncate -----------------------------------------------------------

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"empty", "", 5, ""},
		{"under-max", "abc", 5, "abc"},
		{"at-max", "abcde", 5, "abcde"},
		{"over-max", "abcdef", 5, "abcde…"},
		{"max-zero", "x", 0, "…"}, // edge: empty prefix + ellipsis
		// UTF-8: byte-based, not rune-based. Document the behaviour
		// as a known trade-off so callers know not to rely on it for
		// non-ASCII content without first r/l counting.
		// 中文 = 3 bytes (0xE4 0xB8 0xAD). Byte-truncating at 3
		// keeps the full rune intact; truncating at 1 splits it,
		// yielding a Unicode replacement char on decode. Documented.
		{"utf8-at-rune-boundary", "中abc", 3, "中…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncate(tc.in, tc.max); got != tc.want {
				t.Errorf("truncate(%q, %d) = %q want %q",
					tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// --- sanitizeMap --------------------------------------------------------

func TestSanitizeMap_StripsAllStringValues(t *testing.T) {
	in := map[string]any{
		"plain":     "hello",
		"bold":      "\x1b[1mbold\x1b[0m",
		"nested":    "value \x1b[31mred\x1b[0m end",
		"keep-key":  "\x1b[1monkey-strips-not-keys\x1b[0m",
	}
	out := sanitizeMap(in)
	// All four keys must survive; each string value must be ANSI-free.
	for k := range in {
		if _, ok := out[k]; !ok {
			t.Errorf("key %q lost", k)
		}
	}
	if out["bold"] != "bold" {
		t.Errorf("bold: %q", out["bold"])
	}
	if out["nested"] != "value red end" {
		t.Errorf("nested: %q", out["nested"])
	}
	// Bracket content stays — Claude variant identifiers must survive.
	extra := sanitizeMap(map[string]any{"m": "MiniMax-M3[1m]"})
	if extra["m"] != "MiniMax-M3[1m]" {
		t.Errorf("variant lost: %q", extra["m"])
	}
}

func TestSanitizeMap_PreservesNonStringValues(t *testing.T) {
	in := map[string]any{
		"n":   42,
		"f":   3.14,
		"b":   true,
		"arr": []any{"\x1b[1ma\x1b[0m", 1, true},
	}
	out := sanitizeMap(in)
	if out["n"].(int) != 42 {
		t.Errorf("int mutation: %v", out["n"])
	}
	if _, ok := out["arr"].([]any); !ok {
		t.Errorf("slice type lost: %T", out["arr"])
	}
}

// --- helpers ------------------------------------------------------------

func renderOne(t *testing.T, msg backend.Message) string {
	t.Helper()
	var buf bytes.Buffer
	renderMessage(&buf, msg)
	return buf.String()
}
