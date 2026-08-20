package spec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"conductor/server/internal/protocol"
)

// TestDeriveSpecIdDeterministicSameInput verifies that re-running
// DeriveSpecId on the same spec yields the same id — the foundation
// for ErrAlreadyExists detection and idempotent re-creates.
func TestDeriveSpecIdDeterministicSameInput(t *testing.T) {
	spec := protocol.AgentSpec{
		Provider:     protocol.ProviderCodex,
		Model:        "anthropic/claude-opus-4-6",
		Name:         "opus-planner",
		SystemPrompt: "stay terse",
	}
	id1, err := DeriveSpecId(spec)
	if err != nil {
		t.Fatalf("DeriveSpecId: %v", err)
	}
	id2, err := DeriveSpecId(spec)
	if err != nil {
		t.Fatalf("DeriveSpecId: %v", err)
	}
	if id1 != id2 {
		t.Errorf("DeriveSpecId not deterministic: %q vs %q", id1, id2)
	}
	if !strings.HasPrefix(id1, "opus-planner-") {
		t.Errorf("SpecId = %q, want prefix 'opus-planner-'", id1)
	}
}

// TestDeriveSpecIdIgnoresMetadata verifies the metadata fields
// (SpecId, CreatedAt, UpdatedAt) are stripped before hashing — so
// a freshly-loaded spec with timestamps hashes to the same id as
// the user's input before Create fills them in.
func TestDeriveSpecIdIgnoresMetadata(t *testing.T) {
	user := protocol.AgentSpec{
		Provider: protocol.ProviderCodex,
		Model:    "anthropic/claude-opus-4-6",
		Name:     "opus-planner",
	}
	userId, err := DeriveSpecId(user)
	if err != nil {
		t.Fatalf("DeriveSpecId: %v", err)
	}

	// Same content but with metadata populated (as if just read back).
	loaded := user
	loaded.SpecId = "should-be-ignored"
	loaded.CreatedAt = time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)
	loaded.UpdatedAt = time.Date(2026, 8, 20, 13, 0, 0, 0, time.UTC)
	loadedId, err := DeriveSpecId(loaded)
	if err != nil {
		t.Fatalf("DeriveSpecId: %v", err)
	}
	if userId != loadedId {
		t.Errorf("SpecId changed when metadata added: %q vs %q", userId, loadedId)
	}
}

// TestDeriveSpecIdDifferentContentDifferentId covers the negative
// case: any meaningful difference (Model, Skills, ...) must change
// the id, otherwise ErrAlreadyExists would silently alias specs.
func TestDeriveSpecIdDifferentContentDifferentId(t *testing.T) {
	base := protocol.AgentSpec{
		Provider: protocol.ProviderCodex,
		Model:    "anthropic/claude-opus-4-6",
		Name:     "spec",
	}
	baseId, err := DeriveSpecId(base)
	if err != nil {
		t.Fatalf("DeriveSpecId: %v", err)
	}

	cases := []struct {
		name string
		mut  func(s *protocol.AgentSpec)
	}{
		{"model", func(s *protocol.AgentSpec) { s.Model = "openai/gpt-5" }},
		{"systemPrompt", func(s *protocol.AgentSpec) { s.SystemPrompt = "stay terse" }},
		{"thinking", func(s *protocol.AgentSpec) { s.Thinking = "high" }},
		{"toolsAllow", func(s *protocol.AgentSpec) { s.ToolsAllow = []string{"bash"} }},
		{"toolsExclude", func(s *protocol.AgentSpec) { s.ToolsExclude = []string{"web"} }},
		{"mcpConfig", func(s *protocol.AgentSpec) { s.MCPConfig = "mcp.json" }},
		{"cwd", func(s *protocol.AgentSpec) { s.Cwd = "/tmp" }},
		{"skills", func(s *protocol.AgentSpec) { s.Skills = []string{"review"} }},
		{"worktree", func(s *protocol.AgentSpec) {
			s.Worktree = &protocol.WorktreeSpec{Branch: "feat"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mut(&s)
			id, err := DeriveSpecId(s)
			if err != nil {
				t.Fatalf("DeriveSpecId: %v", err)
			}
			if id == baseId {
				t.Errorf("SpecId unchanged after %s mutation: %q", tc.name, id)
			}
		})
	}
}

// TestDeriveSpecIdHashOnlyWhenNoName verifies a spec without Name
// gets a pure 16-char hash, no dangling '-' separator.
func TestDeriveSpecIdHashOnlyWhenNoName(t *testing.T) {
	spec := protocol.AgentSpec{
		Provider: protocol.ProviderCodex,
		Model:    "openai/gpt-5",
	}
	id, err := DeriveSpecId(spec)
	if err != nil {
		t.Fatalf("DeriveSpecId: %v", err)
	}
	if strings.Contains(id, "-") {
		t.Errorf("SpecId %q should have no dashes (no name)", id)
	}
	if len(id) != 16 {
		t.Errorf("SpecId len = %d, want 16", len(id))
	}
}

// TestDeriveSpecIdEmptyNameFallsBack verifies an empty Name field
// is treated like no Name (no empty "-" prefix in the id).
func TestDeriveSpecIdEmptyNameFallsBack(t *testing.T) {
	withEmpty := protocol.AgentSpec{Provider: protocol.ProviderCodex, Model: "x"}
	withNone := protocol.AgentSpec{Provider: protocol.ProviderCodex, Model: "x"}
	id1, _ := DeriveSpecId(withEmpty)
	id2, _ := DeriveSpecId(withNone)
	if id1 != id2 {
		t.Errorf("empty Name vs no Name should hash identically: %q vs %q", id1, id2)
	}
}

// TestSanitizeName covers every branch of the normalization rules.
func TestSanitizeName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"Hello World", "hello-world"},
		{"  hello  ", "hello"},
		{"foo___bar", "foo-bar"},
		{"foo---bar", "foo-bar"},
		{"--foo--", "foo"},
		{"UPPER", "upper"},
		{"snake_case", "snake-case"},
		{"path/to/spec", "path-to-spec"},
		{"spec.with.dots", "spec-with-dots"},
		{"special!@#chars", "special-chars"},
		{"123-numbers-ok", "123-numbers-ok"},
		{"", ""},
		{"   ", ""},
		{"---", ""},
		// 40-char cap
		{strings.Repeat("a", 50), strings.Repeat("a", 40)},
		{strings.Repeat("a", 39) + "-", strings.Repeat("a", 39)},
		{strings.Repeat("a", 40) + "-bbbb", strings.Repeat("a", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := SanitizeName(tc.in)
			if got != tc.want {
				t.Errorf("SanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCanonicalJSONKeyOrder verifies map keys are sorted at every
// depth (matters for stable SpecId hashing across runs).
func TestCanonicalJSONKeyOrder(t *testing.T) {
	// Same logical content, different map-insertion order.
	a := map[string]any{
		"z": 1,
		"a": map[string]any{"y": 2, "x": 3},
		"m": []any{map[string]any{"q": 4, "p": 5}},
	}
	b := map[string]any{
		"a": map[string]any{"x": 3, "y": 2},
		"m": []any{map[string]any{"p": 5, "q": 4}},
		"z": 1,
	}
	ca, err := canonicalJSON(a)
	if err != nil {
		t.Fatalf("canonicalJSON(a): %v", err)
	}
	cb, err := canonicalJSON(b)
	if err != nil {
		t.Fatalf("canonicalJSON(b): %v", err)
	}
	if string(ca) != string(cb) {
		t.Errorf("canonicalJSON order-dependent:\n  a=%s\n  b=%s", ca, cb)
	}
	// Sanity-check the actual order.
	if !strings.Contains(string(ca), `"a":{"x":3,"y":2}`) {
		t.Errorf("nested key order not sorted: %s", ca)
	}
	if !strings.Contains(string(ca), `"m":[{"p":5,"q":4}]`) {
		t.Errorf("slice-of-map order not sorted: %s", ca)
	}
}

// TestCanonicalJSONFromStruct covers the round-trip from a typed
// AgentSpec (which is what DeriveSpecId actually hashes).
func TestCanonicalJSONFromStruct(t *testing.T) {
	spec := protocol.AgentSpec{
		Provider: protocol.ProviderCodex,
		Model:    "anthropic/claude-opus-4-6",
		Name:     "test",
		Skills:   []string{"review", "ship"},
	}
	first, err := canonicalJSON(spec)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	second, err := canonicalJSON(spec)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("canonicalJSON(struct) not deterministic")
	}
	// Round-trip through Unmarshal to make sure the JSON is valid.
	var parsed any
	if err := json.Unmarshal(first, &parsed); err != nil {
		t.Errorf("canonicalJSON produced invalid JSON: %v", err)
	}
}
