package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsolatedHomeSetupWritesConfigToml verifies the on-disk shape of
// a freshly-created per-spec HOME: config.toml exists with the
// expected content, and the .codex subdirectory was created.
func TestIsolatedHomeSetupWritesConfigToml(t *testing.T) {
	// Isolate from the user's real HOME.
	t.Setenv("CONDUCTOR_HOME", t.TempDir())

	h := New("claude-opus-planner", "openrouter")
	if err := h.Setup(SetupConfig{BaseURL: "https://openrouter.ai/api/v1", EnvKey: "OPENROUTER_API_KEY"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// config.toml exists with the expected content.
	data, err := os.ReadFile(h.ConfigTomlPath())
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `model_provider = "openrouter"`) {
		t.Errorf("config.toml missing model_provider:\n%s", body)
	}
	if !strings.Contains(body, `[model_providers.openrouter]`) {
		t.Errorf("config.toml missing [model_providers.openrouter]:\n%s", body)
	}
	if !strings.Contains(body, `base_url = "https://openrouter.ai/api/v1"`) {
		t.Errorf("config.toml missing base_url:\n%s", body)
	}
	if !strings.Contains(body, `env_key = "OPENROUTER_API_KEY"`) {
		t.Errorf("config.toml missing env_key:\n%s", body)
	}

	// .codex subdirectory was created with 0700 perms.
	info, err := os.Stat(filepath.Join(h.HomeDir(), ".codex"))
	if err != nil {
		t.Fatalf("stat .codex: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf(".codex perms = %o, want 0700", perm)
	}
}

// TestIsolatedHomeSetupWithoutAuth verifies that empty authSourcePath
// is fine (for local providers like Ollama that don't need an API key).
func TestIsolatedHomeSetupWithoutAuth(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())

	h := New("local-llama", "ollama")
	if err := h.Setup(SetupConfig{BaseURL: "http://localhost:11434/v1"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	data, err := os.ReadFile(h.ConfigTomlPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "env_key") {
		t.Errorf("config.toml should not have env_key when not provided:\n%s", data)
	}
	// No auth symlink should have been attempted.
	if _, err := os.Lstat(filepath.Join(h.HomeDir(), ".codex.json")); !os.IsNotExist(err) {
		t.Errorf(".codex.json should not exist when authSourcePath is empty: %v", err)
	}
}

// TestIsolatedHomeRemove verifies Remove() actually deletes the HOME.
func TestIsolatedHomeRemove(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())

	h := New("delete-me", "openai")
	if err := h.Setup(SetupConfig{BaseURL: "https://api.openai.com/v1", EnvKey: "OPENAI_API_KEY"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := os.Stat(h.HomeDir()); err != nil {
		t.Fatalf("HOME not created: %v", err)
	}
	if err := h.Remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(h.HomeDir()); !os.IsNotExist(err) {
		t.Errorf("HOME should be gone after Remove, got err = %v", err)
	}
}

// TestIsolatedHomeSetupWritesWireAPIAndAuth verifies the per-spec
// HOME config.toml reflects the MiniMax-style custom provider
// fields: wire_api = "responses" + requires_openai_auth = false.
// These two together tell codex app-server "skip ChatGPT OAuth,
// use the Responses protocol against this non-OpenAI endpoint".
func TestIsolatedHomeSetupWritesWireAPIAndAuth(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	h := New("minimax-planner", "minimax")
	noAuth := false
	if err := h.Setup(SetupConfig{
		BaseURL:            "http://127.0.0.1:8000/v1",
		EnvKey:             "MINIMAX_API_KEY",
		WireAPI:            "responses",
		RequiresOpenAIAuth: &noAuth,
	}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	body, err := os.ReadFile(h.ConfigTomlPath())
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	for _, want := range []string{
		`wire_api = "responses"`,
		`requires_openai_auth = false`,
		`env_key = "MINIMAX_API_KEY"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("config.toml missing %q:\n%s", want, body)
		}
	}
}

// TestIsolatedHomeSetupOmitsRequiresOpenAIAuthWhenNil verifies
// that nil RequiresOpenAIAuth means "do not write the field".
// This matters because Codex's default is requires_openai_auth=true
// (ChatGPT OAuth), and we don't want to silently flip providers
// like openrouter into "no OAuth" mode just because the user
// didn't set the field.
func TestIsolatedHomeSetupOmitsRequiresOpenAIAuthWhenNil(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	h := New("openrouter-planner", "openrouter")
	if err := h.Setup(SetupConfig{
		BaseURL: "https://openrouter.ai/api/v1",
		EnvKey:  "OPENROUTER_API_KEY",
		// RequiresOpenAIAuth deliberately left nil.
	}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	body, err := os.ReadFile(h.ConfigTomlPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), "requires_openai_auth") {
		t.Errorf("config.toml should not write requires_openai_auth when nil:\n%s", body)
	}
}

// TestIsolatedHomeSetupOmitsWireAPIWhenEmpty verifies wire_api is
// not written when empty (Codex picks its default).
func TestIsolatedHomeSetupOmitsWireAPIWhenEmpty(t *testing.T) {
	t.Setenv("CONDUCTOR_HOME", t.TempDir())
	h := New("openrouter-planner", "openrouter")
	if err := h.Setup(SetupConfig{
		BaseURL: "https://openrouter.ai/api/v1",
		EnvKey:  "OPENROUTER_API_KEY",
	}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	body, err := os.ReadFile(h.ConfigTomlPath())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), "wire_api") {
		t.Errorf("config.toml should not write wire_api when empty:\n%s", body)
	}
}
