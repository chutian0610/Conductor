package audit

import (
	"strings"
	"testing"
	"time"

	"conductor/server/internal/agentregistry"
)

func TestParseVerdict_HappyPath(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantVerd   string
		wantEvid   string
	}{
		{
			"pass",
			`{"verdict":"pass","evidence":"all good"}`,
			"pass", "all good",
		},
		{
			"fail",
			`{"verdict":"fail","evidence":"tool Bash failed twice"}`,
			"fail", "tool Bash failed twice",
		},
		{
			"unverifiable",
			`{"verdict":"unverifiable","evidence":"truncated mid-run"}`,
			"unverifiable", "truncated mid-run",
		},
		{
			"verdict-with-padded-whitespace",
			"   \n\n  " + `{"verdict":"pass","evidence":"ok"}` + "\n\n",
			"pass", "ok",
		},
		{
			"verdict-then-trailing-prose",
			`{"verdict":"pass","evidence":"ok"}` + "\n\nSome trailing commentary\n",
			"pass", "ok",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, e := ParseVerdict(tc.in)
			if v != tc.wantVerd {
				t.Errorf("verdict: got %q want %q", v, tc.wantVerd)
			}
			if e != tc.wantEvid {
				t.Errorf("evidence: got %q want %q", e, tc.wantEvid)
			}
		})
	}
}

func TestParseVerdict_FailureModes_AllConvergeOnUnverifiable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// All cases converge to verdict="unverifiable".
		sub string // substring the evidence must contain
	}{
		{"empty", "", "auditor returned empty output"},
		// Whitespace-only collapses to empty under TrimSpace, hitting the same
		// "empty output" branch as a truly empty string.
		{"whitespace-only", "   \n\n\t  ", "empty output"},
		{"non-json-prefix", "The agent looks fine.", "non-JSON output"},
		// The non-JSON prefix consumes the first line; the contract fires the
		// "non-JSON output" branch instead of "empty first line".
		{"plain-text-then-json", "OK here's my answer:\n{\"verdict\":\"pass\"}", "non-JSON output"},
		{"malformed-json", "{not valid json", "malformed JSON"},
		{"empty-verdict", `{"verdict":"","evidence":"x"}`, "missing field"},
		{"empty-evidence", `{"verdict":"pass","evidence":""}`, "missing field"},
		{"unknown-verdict", `{"verdict":"maybe","evidence":"unsure"}`, "unknown verdict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, e := ParseVerdict(tc.in)
			if v != "unverifiable" {
				t.Errorf("verdict: got %q want unverifiable (case %q)", v, tc.name)
			}
			if !strings.Contains(e, tc.sub) {
				t.Errorf("evidence %q must contain %q", e, tc.sub)
			}
		})
	}
}

func TestTruncateForEvidence(t *testing.T) {
	if got := truncateForEvidence("short"); got != "short" {
		t.Errorf("short: %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := truncateForEvidence(long); len(got) > 205 { // 200 + "…"
		t.Errorf("long should be capped; got len=%d", len(got))
	}
	if !strings.HasSuffix(truncateForEvidence(long), "…") {
		t.Errorf("long must end with ellipsis")
	}
}

func TestIsAuditableStatus(t *testing.T) {
	for _, s := range []string{"completed", "failed", "timeout", "cancelled", "aborted"} {
		if !isAuditableStatus(s) {
			t.Errorf("%q should be auditable", s)
		}
	}
	for _, s := range []string{"", "running", "pending", "garbage"} {
		if isAuditableStatus(s) {
			t.Errorf("%q must NOT be auditable", s)
		}
	}
}

func TestSHA256Hex_DeterministicAndLongEnough(t *testing.T) {
	empty := sha256Hex("")
	if len(empty) != 64 {
		t.Errorf("sha256 hex length: got %d want 64", len(empty))
	}
	a := sha256Hex("hello")
	b := sha256Hex("hello")
	if a != b {
		t.Errorf("sha256 not deterministic: %q vs %q", a, b)
	}
	if sha256Hex("hello") == sha256Hex("world") {
		t.Errorf("sha256 collision between hello and world")
	}
}

// renderTranscript is exercised end-to-end by Run's integration test
// below; we pin its shape via a sanity check here.
func TestRenderTranscript_IncludesRunAndEvents(t *testing.T) {
	run := agentregistryRunFixture()
	got := renderTranscript(run, []agentregistry.Event{
		{ID: 1, Kind: "text"},
		{ID: 2, Kind: "tool_use"},
	})
	for _, want := range []string{
		"Run #42",
		"agent_id      : 7",
		"status        : completed",
		"events        : 2 rows",
		"[event 1 kind=text]",
		"[event 2 kind=tool_use]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q\n---\n%s", want, got)
		}
	}
}

func TestRenderTranscript_TruncatesAtCap(t *testing.T) {
	huge := agentregistryRunFixture()
	events := make([]agentregistry.Event, 1)
	events[0] = agentregistry.Event{ID: 1, Kind: "text",
		Payload: []byte(strings.Repeat("z", transcriptMaxChars))}
	got := renderTranscript(huge, events)
	if len(got) > transcriptMaxChars+200 { // +200 for trailing wrapper
		t.Errorf("transcript exceeded cap; got %d (cap %d)", len(got), transcriptMaxChars)
	}
	if !strings.Contains(got, "...[transcript truncated;") {
		t.Errorf("truncation marker missing in capped output")
	}
}


func agentregistryRunFixture() agentregistry.Run {
	return agentregistry.Run{
		ID:        42,
		AgentID:   7,
		Status:    "completed",
		StartedAt: time.UnixMilli(1700000000000),
		DurationMs: 1234,
		SessionID: "sess-fixture",
		PromptSHA: "abcd",
	}
}
