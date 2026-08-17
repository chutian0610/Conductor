package agentregistry_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"conductor/server/internal/agentregistry"
)

func newStore(t *testing.T) *agentregistry.Store {
	t.Helper()
	s, err := agentregistry.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRegisterAndGet(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	id, err := s.RegisterAgent(ctx, agentregistry.Agent{
		Name:        "code-reviewer",
		Description: "Reads a git diff and posts a code review.",
		Backend:     "claude",
	})
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	got, err := s.GetAgent(ctx, "code-reviewer")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Name != "code-reviewer" || got.Backend != "claude" {
		t.Fatalf("got %+v", got)
	}
	if got.ID != id {
		t.Fatalf("id mismatch: got %d want %d", got.ID, id)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", got)
	}
}

func TestRegisterRequiresKnownBackend(t *testing.T) {
	s := newStore(t)
	_, err := s.RegisterAgent(context.Background(), agentregistry.Agent{
		Name: "x", Backend: "made-up",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown backend") {
		t.Fatalf("want backend validation, got %v", err)
	}
}

func TestRegisterRejectsEmptyName(t *testing.T) {
	s := newStore(t)
	_, err := s.RegisterAgent(context.Background(), agentregistry.Agent{
		Name: "  ", Backend: "claude",
	})
	if err == nil {
		t.Fatal("expected validation error for empty name")
	}
}

func TestRegisterUniqueName(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.RegisterAgent(ctx, agentregistry.Agent{Name: "dup", Backend: "claude"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.RegisterAgent(ctx, agentregistry.Agent{Name: "dup", Backend: "claude"})
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestUpdateAgent_Patch(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	id, _ := s.RegisterAgent(ctx, agentregistry.Agent{Name: "u", Backend: "claude", Description: "old"})

	desc := "new desc"
	be := "codex"
	if err := s.UpdateAgent(ctx, id, agentregistry.AgentPatch{
		Description: &desc,
		Backend:     &be,
	}); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	got, _ := s.GetAgent(ctx, "@"+itoa(id))
	if got.Description != "new desc" || got.Backend != "codex" {
		t.Fatalf("patch didn't apply: %+v", got)
	}
}

func TestArchiveHidesFromList(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	id, _ := s.RegisterAgent(ctx, agentregistry.Agent{Name: "to-archive", Backend: "claude"})
	if err := s.ArchiveAgent(ctx, id); err != nil {
		t.Fatal(err)
	}
	active, _ := s.ListAgents(ctx, agentregistry.ListAgentOpts{})
	if len(active) != 0 {
		t.Fatalf("archived agent should be hidden by default: got %d", len(active))
	}
	all, _ := s.ListAgents(ctx, agentregistry.ListAgentOpts{IncludeArchived: true})
	if len(all) != 1 || all[0].ArchivedAt == nil {
		t.Fatalf("include-archived should reveal it: %+v", all)
	}
}

func TestArchiveNotFound(t *testing.T) {
	s := newStore(t)
	err := s.ArchiveAgent(context.Background(), 999)
	if !errors.Is(err, agentregistry.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGetAgent_RefForms(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	id, _ := s.RegisterAgent(ctx, agentregistry.Agent{Name: "by-name", Backend: "claude"})

	cases := []struct {
		ref     string
		wantID  int64
		wantErr error
	}{
		{"by-name", id, nil},
		{"@1", id, nil},
		{"42", 42 /* nonexistent id */, agentregistry.ErrNotFound},
		{"", 0, nil}, // validation, not ErrNotFound
		{"@xyz", 0, nil}, // parse error
	}
	for _, tc := range cases {
		_, err := s.GetAgent(ctx, tc.ref)
		switch {
		case tc.wantErr == nil && tc.ref != "by-name" && tc.ref != "@1":
			if err == nil {
				t.Errorf("ref %q: expected error", tc.ref)
			}
		case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
			t.Errorf("ref %q: want %v, got %v", tc.ref, tc.wantErr, err)
		}
	}
}

func TestIdentityEnvPrecedence(t *testing.T) {
	env := []string{
		"CONDUCTOR_AGENT_ID=primary",
		"CLAUDE_CODE_SESSION_ID=claude-sess",
		"CODEX_THREAD_ID=codex-sess",
	}
	if got := agentregistry.CurrentAgentID(env); got != "primary" {
		t.Fatalf("agent id: %q", got)
	}
	// Session id precedence: CLAUDE > CODEX > CONDUCTOR > parent.
	env = []string{"CONDUCTOR_PARENT_SESSION_ID=p-sess", "CONDUCTOR_SESSION_ID=c-sess"}
	if got := agentregistry.CurrentSessionID(env); got != "c-sess" {
		t.Fatalf("session id (no backend vars): %q", got)
	}
	env = []string{"CODEX_THREAD_ID=k-sess"}
	if got := agentregistry.CurrentSessionID(env); got != "k-sess" {
		t.Fatalf("session id (codex only): %q", got)
	}
	env = []string{"CLAUDE_CODE_SESSION_ID=c-sess", "CODEX_THREAD_ID=k-sess"}
	if got := agentregistry.CurrentSessionID(env); got != "c-sess" {
		t.Fatalf("session id (claude > codex): %q", got)
	}
}

func TestIdentityEnv_Empty(t *testing.T) {
	if got := agentregistry.CurrentAgentID(nil); got != "" {
		t.Errorf("want empty, got %q", got)
	}
	if got := agentregistry.CurrentSessionID([]string{"FOO=bar"}); got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

func TestIdentityEnvBuilder(t *testing.T) {
	got := agentregistry.IdentityEnv("a", 7, "sess-X")
	joined := strings.Join(got, ",")
	for _, want := range []string{"CONDUCTOR_AGENT_ID=a", "CONDUCTOR_PARENT_RUN_ID=7", "CONDUCTOR_PARENT_SESSION_ID=sess-X"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}

	// Empty fields are dropped so callers can hand us 0 values freely.
	short := agentregistry.IdentityEnv("", 0, "")
	if len(short) != 0 {
		t.Errorf("expected empty, got %v", short)
	}
}

// itoa is reused inline (no strconv import) to mirror the package's
// other zero-dep helpers — kept as a local in tests.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [20]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestEnsureAgent_CreatesAndReturns(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	got, err := s.EnsureAgent(ctx, agentregistry.Agent{
		Name: "auto", Backend: "claude", Description: "first",
	})
	if err != nil {
		t.Fatalf("first EnsureAgent: %v", err)
	}
	if got.ID == 0 || got.Description != "first" {
		t.Fatalf("first EnsureAgent unexpected: %+v", got)
	}

	got2, err := s.EnsureAgent(ctx, agentregistry.Agent{
		Name: "auto", Backend: "claude", Description: "second",
	})
	if err != nil {
		t.Fatalf("second EnsureAgent: %v", err)
	}
	if got2.ID != got.ID {
		t.Fatalf("EnsureAgent must reuse existing row: %d vs %d", got2.ID, got.ID)
	}
	if got2.Description != "first" {
		t.Fatalf("EnsureAgent must not overwrite existing description: %q", got2.Description)
	}
}

func TestEnsureAgent_RejectsEmptyName(t *testing.T) {
	s := newStore(t)
	_, err := s.EnsureAgent(context.Background(), agentregistry.Agent{Backend: "claude"})
	if err == nil {
		t.Fatal("expected validation error")
	}
}
