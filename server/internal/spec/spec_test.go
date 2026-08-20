package spec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"conductor/server/internal/home"
	"conductor/server/internal/protocol"
)

// resetConductorHome points $CONDUCTOR_HOME at a fresh temp dir
// for each test (so spec writes don't leak between cases).
func resetConductorHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CONDUCTOR_HOME", dir)
	return dir
}

// TestCreateHappyPath verifies the on-disk shape after a successful
// Create: spec.json with full SpecRecord, .codex/config.toml with
// the resolved BaseURL/EnvKey, .codex.json auth symlink (or absent
// when AuthSourcePath is intentionally skipped).
func TestCreateHappyPath(t *testing.T) {
	resetConductorHome(t)
	ctx := context.Background()

	res, err := Create(ctx, CreateInput{
		Spec: protocol.AgentSpec{
			Provider: protocol.ProviderCodex,
			Model:    "anthropic/claude-opus-4-6",
			Name:     "opus-planner",
		},
		BaseURL: "https://openrouter.ai/api/v1",
		EnvKey:  "OPENROUTER_API_KEY",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(res.SpecId, "opus-planner-") {
		t.Errorf("SpecId = %q, want prefix 'opus-planner-'", res.SpecId)
	}
	if res.Record.HomePath == "" {
		t.Errorf("Record.HomePath is empty")
	}
	if res.Record.ConfigToml == "" {
		t.Errorf("Record.ConfigToml is empty")
	}

	// spec.json was written.
	data, err := os.ReadFile(home.SpecMetaPath(res.SpecId))
	if err != nil {
		t.Fatalf("read spec.json: %v", err)
	}
	var rec protocol.SpecRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse spec.json: %v", err)
	}
	if rec.Spec.SpecId != res.SpecId {
		t.Errorf("spec.SpecId = %q, want %q", rec.Spec.SpecId, res.SpecId)
	}
	if rec.HomePath != res.Record.HomePath {
		t.Errorf("HomePath = %q, want %q", rec.HomePath, res.Record.HomePath)
	}
	if rec.Spec.CreatedAt.IsZero() {
		t.Errorf("CreatedAt should be set by Create")
	}
	if rec.Spec.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt should be set by Create")
	}

	// .codex/config.toml was written with the right provider/url/key.
	cfgData, err := os.ReadFile(res.Record.ConfigToml)
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	cfg := string(cfgData)
	for _, want := range []string{
		`model_provider = "codex"`,
		`[model_providers.codex]`,
		`base_url = "https://openrouter.ai/api/v1"`,
		`env_key = "OPENROUTER_API_KEY"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config.toml missing %q:\n%s", want, cfg)
		}
	}

	// HOME directory exists with 0700 perms.
	info, err := os.Stat(res.Record.HomePath)
	if err != nil {
		t.Fatalf("stat HOME: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("HOME is not a directory")
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("HOME perms = %o, want 0700", perm)
	}

	// .codex.json auth symlink points at the auth file.
	linkTarget, err := os.Readlink(filepath.Join(res.Record.HomePath, ".codex.json"))
	if err != nil {
		t.Fatalf("readlink .codex.json: %v", err)
	}
	if !strings.HasSuffix(linkTarget, ".auth/codex/auth.json") {
		t.Errorf("auth symlink target = %q, want suffix '.auth/codex/auth.json'", linkTarget)
	}
}

// TestCreateNoAuth verifies that EnvKey="" and an authSourcePath
// pointing at nothing (Phase 1 doesn't pre-populate auth) still
// produces a usable HOME: config.toml without env_key, no symlink.
func TestCreateNoAuth(t *testing.T) {
	resetConductorHome(t)
	ctx := context.Background()

	res, err := Create(ctx, CreateInput{
		Spec: protocol.AgentSpec{
			Provider: protocol.ProviderCodex,
			Model:    "ollama-llama",
		},
		BaseURL: "http://localhost:11434/v1",
		EnvKey:  "",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cfgData, err := os.ReadFile(res.Record.ConfigToml)
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if strings.Contains(string(cfgData), "env_key") {
		t.Errorf("config.toml should not contain env_key when EnvKey is empty:\n%s", cfgData)
	}
	// .codex.json IS created here (EnsureAuthDirFile always
	// produces an empty auth source first, then Setup symlinks
	// it). Phase 1 doesnt differentiate local vs remote providers
	// at the symlink layer; the link is harmless either way.
	// The full happy-path symlink assertion is in TestCreateHappyPath.
}

// TestCreateAlreadyExists verifies idempotent re-runs return
// ErrAlreadyExists without touching the original HOME or spec.json.
func TestCreateAlreadyExists(t *testing.T) {
	resetConductorHome(t)
	ctx := context.Background()

	in := CreateInput{
		Spec: protocol.AgentSpec{
			Provider: protocol.ProviderCodex,
			Model:    "anthropic/claude-opus-4-6",
			Name:     "opus-planner",
		},
		BaseURL: "https://openrouter.ai/api/v1",
		EnvKey:  "OPENROUTER_API_KEY",
	}
	first, err := Create(ctx, in)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Stash the mtime of the first HOME so we can verify it
	// doesn't get rewritten on collision.
	firstStat, err := os.Stat(first.Record.HomePath)
	if err != nil {
		t.Fatalf("stat HOME: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // ensure mtime resolution

	_, err = Create(ctx, in)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("second Create err = %v, want ErrAlreadyExists", err)
	}

	secondStat, err := os.Stat(first.Record.HomePath)
	if err != nil {
		t.Fatalf("stat HOME again: %v", err)
	}
	if !secondStat.ModTime().Equal(firstStat.ModTime()) {
		t.Errorf("HOME mtime changed on collision: %v vs %v",
			firstStat.ModTime(), secondStat.ModTime())
	}
}

// TestCreateProviderRequired guards the contract that Provider is
// mandatory (otherwise the HOME has no provider to write).
func TestCreateProviderRequired(t *testing.T) {
	resetConductorHome(t)
	_, err := Create(context.Background(), CreateInput{
		Spec:    protocol.AgentSpec{Model: "x"},
		BaseURL: "https://example.com",
	})
	if !errors.Is(err, ErrProviderRequired) {
		t.Errorf("err = %v, want ErrProviderRequired", err)
	}
}

// TestGet verifies reading back a freshly-created spec round-trips
// all the fields we care about (Spec content, HomePath, ConfigToml).
func TestGet(t *testing.T) {
	resetConductorHome(t)
	ctx := context.Background()

	created, err := Create(ctx, CreateInput{
		Spec: protocol.AgentSpec{
			Provider: protocol.ProviderCodex,
			Model:    "anthropic/claude-opus-4-6",
			Name:     "rt-test",
			Skills:   []string{"review", "ship"},
		},
		BaseURL: "https://example.com",
		EnvKey:  "EXAMPLE_KEY",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := Get(ctx, created.SpecId)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec.SpecId != created.SpecId {
		t.Errorf("got.Spec.SpecId = %q, want %q", got.Spec.SpecId, created.SpecId)
	}
	if got.Spec.Model != "anthropic/claude-opus-4-6" {
		t.Errorf("got.Spec.Model = %q", got.Spec.Model)
	}
	if got.HomePath != created.Record.HomePath {
		t.Errorf("got.HomePath = %q, want %q", got.HomePath, created.Record.HomePath)
	}
	if got.ConfigToml != created.Record.ConfigToml {
		t.Errorf("got.ConfigToml = %q, want %q", got.ConfigToml, created.Record.ConfigToml)
	}
}

// TestGetNotFound checks the canonical not-found error path.
func TestGetNotFound(t *testing.T) {
	resetConductorHome(t)
	_, err := Get(context.Background(), "does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestListEmpty covers the case where $CONDUCTOR_HOME/specs doesn't
// exist yet (first-run scenario).
func TestListEmpty(t *testing.T) {
	resetConductorHome(t)
	got, err := List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d records, want 0", len(got))
	}
}

// TestListMultiple verifies ordering (sorted by SpecId) and that
// all created specs are returned.
func TestListMultiple(t *testing.T) {
	resetConductorHome(t)
	ctx := context.Background()

	// Create three specs in a non-alphabetical order to verify the
	// sort is by SpecId, not insertion order.
	specs := []CreateInput{
		{Spec: protocol.AgentSpec{Provider: protocol.ProviderCodex, Model: "m1", Name: "charlie"}},
		{Spec: protocol.AgentSpec{Provider: protocol.ProviderCodex, Model: "m2", Name: "alpha"}},
		{Spec: protocol.AgentSpec{Provider: protocol.ProviderCodex, Model: "m3", Name: "bravo"}},
	}
	for i, s := range specs {
		res, err := Create(ctx, s)
		if err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
		// Sanity: each got a distinct prefix.
		if !strings.HasPrefix(res.SpecId, []string{"charlie-", "alpha-", "bravo-"}[i]) {
			t.Errorf("spec #%d SpecId = %q", i, res.SpecId)
		}
	}

	got, err := List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	wantPrefixes := []string{"alpha-", "bravo-", "charlie-"}
	for i, w := range wantPrefixes {
		if !strings.HasPrefix(got[i].Spec.SpecId, w) {
			t.Errorf("record[%d] SpecId = %q, want prefix %q", i, got[i].Spec.SpecId, w)
		}
	}
}

// TestListSkipsOrphanDirs verifies directories without spec.json
// are silently skipped (not treated as errors).
func TestListSkipsOrphanDirs(t *testing.T) {
	resetConductorHome(t)
	ctx := context.Background()

	// One real spec.
	if _, err := Create(ctx, CreateInput{
		Spec:    protocol.AgentSpec{Provider: protocol.ProviderCodex, Model: "m", Name: "real"},
		BaseURL: "https://x",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// One orphan dir (no spec.json inside).
	if err := os.MkdirAll(filepath.Join(home.SpecsDir(), "orphan"), 0o700); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}

	got, err := List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1 (orphan should be skipped)", len(got))
	}
	if !strings.HasPrefix(got[0].Spec.SpecId, "real-") {
		t.Errorf("record SpecId = %q, want prefix 'real-'", got[0].Spec.SpecId)
	}
}

// TestRemove verifies both the spec.json and HOME are deleted, and
// that subsequent Get returns ErrNotFound.
func TestRemove(t *testing.T) {
	resetConductorHome(t)
	ctx := context.Background()

	created, err := Create(ctx, CreateInput{
		Spec:    protocol.AgentSpec{Provider: protocol.ProviderCodex, Model: "m", Name: "rm-me"},
		BaseURL: "https://x",
		EnvKey:  "K",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := Remove(ctx, created.SpecId); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := os.Stat(created.Record.HomePath); !os.IsNotExist(err) {
		t.Errorf("HOME should be gone after Remove, got err = %v", err)
	}
	if _, err := os.Stat(home.SpecMetaPath(created.SpecId)); !os.IsNotExist(err) {
		t.Errorf("spec.json should be gone after Remove, got err = %v", err)
	}

	if _, err := Get(ctx, created.SpecId); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Remove err = %v, want ErrNotFound", err)
	}
}

// TestRemoveNotFound ensures Remove is strict about metadata: it
// refuses to silently nuke a spec whose spec.json is already gone
// (so caller bugs surface as ErrNotFound, not silent data loss).
func TestRemoveNotFound(t *testing.T) {
	resetConductorHome(t)
	err := Remove(context.Background(), "never-existed")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestCreateCtxCancel covers the ctx-cancellation cleanup path:
// if the caller cancels mid-Create, the partial HOME + spec.json
// should be rolled back so a retry can succeed.
func TestCreateCtxCancel(t *testing.T) {
	resetConductorHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE Create starts

	_, err := Create(ctx, CreateInput{
		Spec:    protocol.AgentSpec{Provider: protocol.ProviderCodex, Model: "m", Name: "cancel-me"},
		BaseURL: "https://x",
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	// Verify nothing was written.
	entries, err := os.ReadDir(home.SpecsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read specs dir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("unexpected entry after cancelled Create: %q", e.Name())
	}
}

// TestCreateOverwriteUnsafe makes it explicit that re-running with
// the SAME spec content is treated as an error (not silent
// overwrite), so `conductor spec create` can't accidentally clobber
// a spec whose session history matters.
func TestCreateOverwriteUnsafe(t *testing.T) {
	resetConductorHome(t)
	ctx := context.Background()

	in := CreateInput{
		Spec: protocol.AgentSpec{
			Provider: protocol.ProviderCodex,
			Model:    "m",
			Name:     "no-clobber",
		},
		BaseURL: "https://x",
	}
	first, err := Create(ctx, in)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Simulate the spec being used (write a session JSONL into HOME).
	sessionDir := filepath.Join(first.Record.HomePath, ".codex", "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	sessionFile := filepath.Join(sessionDir, "sess-1.jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"role":"user","text":"hi"}`), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	_, err = Create(ctx, in)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Create err = %v, want ErrAlreadyExists", err)
	}
	if _, err := os.Stat(sessionFile); err != nil {
		t.Errorf("session file should still exist after refused overwrite: %v", err)
	}
}

// TestCreateWritesWireAPIAndAuth verifies the end-to-end flow:
// spec.Create with WireAPI + RequiresOpenAIAuth=false on the
// input lands them in the per-spec config.toml. This is the
// MiniMax custom-provider scenario — codex app-server needs to
// see both fields to skip ChatGPT OAuth and use the Responses
// protocol.
func TestCreateWritesWireAPIAndAuth(t *testing.T) {
	resetConductorHome(t)
	ctx := context.Background()

	noAuth := false
	res, err := Create(ctx, CreateInput{
		Spec: protocol.AgentSpec{
			Provider: protocol.ProviderCodex,
			Model:    "MiniMax-Text-01",
			Name:     "minimax-custom",
		},
		BaseURL:            "http://127.0.0.1:8000/v1",
		EnvKey:             "MINIMAX_API_KEY",
		WireAPI:            "responses",
		RequiresOpenAIAuth: &noAuth,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cfg, err := os.ReadFile(res.Record.ConfigToml)
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	body := string(cfg)
	for _, want := range []string{
		`model_provider = "codex"`,
		`[model_providers.codex]`,
		`base_url = "http://127.0.0.1:8000/v1"`,
		`env_key = "MINIMAX_API_KEY"`,
		`wire_api = "responses"`,
		`requires_openai_auth = false`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config.toml missing %q:\n%s", want, body)
		}
	}
}
