package codexconfig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestParseHappyPath covers a representative real-world config:
// multiple providers, comments, blank lines, mixed quote styles.
func TestParseHappyPath(t *testing.T) {
	src := []byte(`
# top-level comment
[model_providers.openai]
name = "OpenAI"
base_url = "https://api.openai.com/v1"
env_key = "OPENAI_API_KEY"

[model_providers.openrouter]
name = "OpenRouter"
base_url = "https://openrouter.ai/api/v1"
env_key = "OPENROUTER_API_KEY"

[unrelated.section]
foo = "bar"  # should be ignored

# trailing comment
`)
	got, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d providers, want 2: %+v", len(got), got)
	}
	if pc := got["openai"]; pc.ID != "openai" || pc.BaseURL != "https://api.openai.com/v1" ||
		pc.EnvKey != "OPENAI_API_KEY" || pc.Name != "OpenAI" {
		t.Errorf("openai = %+v", pc)
	}
	if pc := got["openrouter"]; pc.BaseURL != "https://openrouter.ai/api/v1" ||
		pc.EnvKey != "OPENROUTER_API_KEY" {
		t.Errorf("openrouter = %+v", pc)
	}
}

// TestParseBareStrings verifies values can be unquoted identifiers
// (Codex itself uses this style for env_key in some configs).
func TestParseBareStrings(t *testing.T) {
	src := []byte(`[model_providers.openai]
env_key = OPENAI_API_KEY
base_url = https://api.openai.com/v1
`)
	got, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pc := got["openai"]
	if pc.EnvKey != "OPENAI_API_KEY" {
		t.Errorf("EnvKey = %q, want OPENAI_API_KEY", pc.EnvKey)
	}
	if pc.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("BaseURL = %q", pc.BaseURL)
	}
}

// TestParseUnknownKeysIgnored ensures we don't choke on extra
// fields the user (or a newer Codex) added.
func TestParseUnknownKeysIgnored(t *testing.T) {
	src := []byte(`[model_providers.openai]
name = "OpenAI"
base_url = "https://api.openai.com/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
some_future_field = ["a", "b"]
`)
	got, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got["openai"].BaseURL != "https://api.openai.com/v1" {
		t.Errorf("known fields lost when unknown keys present: %+v", got["openai"])
	}
}

// TestParseEscapes verifies basic TOML string escapes work.
func TestParseEscapes(t *testing.T) {
 src := []byte("[model_providers.test]\nname = \"a\\nb\\tc\\\"d\"\n")
	got, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pc := got["test"]
	want := "a\nb\tc\"d"
	if pc.Name != want {
		t.Errorf("Name = %q, want %q", pc.Name, want)
	}
}

// TestParseSectionHeaderMissingBracket covers the error path for
// malformed section headers.
func TestParseSectionHeaderMissingBracket(t *testing.T) {
	_, err := Parse([]byte("[model_providers.openai\nfoo = \"bar\"\n"))
	if err == nil {
		t.Errorf("expected error for missing ']', got nil")
	}
}

// TestParseEmptyIsOK — empty file is valid (zero providers).
func TestParseEmptyIsOK(t *testing.T) {
	got, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d providers, want 0", len(got))
	}
}

// TestResolveBuiltinOpenAI covers the special-case fallback:
// "codex" provider works even without any config.toml.
func TestResolveBuiltinOpenAI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

	pc, err := Resolve("", "codex")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pc.BaseURL != "https://api.openai.com/v1" || pc.EnvKey != "OPENAI_API_KEY" {
		t.Errorf("builtin = %+v", pc)
	}
}

// TestResolveBuiltinOverride covers the case where the user does
// have a config.toml with an explicit "codex" provider — their
// values win.
func TestResolveBuiltinOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
[model_providers.codex]
base_url = "https://my-proxy.example.com/v1"
env_key = "MY_PROXY_KEY"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	pc, err := Resolve("", "codex")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pc.BaseURL != "https://my-proxy.example.com/v1" || pc.EnvKey != "MY_PROXY_KEY" {
		t.Errorf("override = %+v", pc)
	}
}

// TestResolveConfigNotFound covers a non-codex provider with no
// config.toml: should return ErrConfigNotFound.
func TestResolveConfigNotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

	_, err := Resolve("", "openrouter")
	if !errors.Is(err, ErrConfigNotFound) {
		t.Errorf("err = %v, want ErrConfigNotFound", err)
	}
}

// TestResolveProviderNotFound covers a config.toml that exists but
// doesn't declare the requested provider.
func TestResolveProviderNotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
[model_providers.openai]
base_url = "https://api.openai.com/v1"
env_key = "OPENAI_API_KEY"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Resolve("", "openrouter")
	if !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("err = %v, want ErrProviderNotFound", err)
	}
}

// TestResolveRoundTrip verifies the full file→Resolve path with
// a realistic config.
func TestResolveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
[model_providers.openrouter]
base_url = "https://openrouter.ai/api/v1"
env_key = "OPENROUTER_API_KEY"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	pc, err := Resolve("", "openrouter")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pc.BaseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("BaseURL = %q", pc.BaseURL)
	}
	if pc.EnvKey != "OPENROUTER_API_KEY" {
		t.Errorf("EnvKey = %q", pc.EnvKey)
	}
}

// TestResolveUsesCODEXHOME verifies the env var override works.
func TestResolveUsesCODEXHOME(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`
[model_providers.openrouter]
base_url = "https://example.com/v1"
env_key = "EXAMPLE_KEY"
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	pc, err := Resolve("", "openrouter")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pc.EnvKey != "EXAMPLE_KEY" {
		t.Errorf("EnvKey = %q, want EXAMPLE_KEY (from $CODEX_HOME)", pc.EnvKey)
	}
}

// TestParseWireAPIAndRequiresOpenAIAuth covers the MiniMax-style
// custom-provider block (no OpenAI account, custom base URL +
// wire protocol). These two fields are the gate for whether
// codex app-server attempts ChatGPT OAuth at all.
func TestParseWireAPIAndRequiresOpenAIAuth(t *testing.T) {
	src := []byte(`
[model_providers.minimax]
name = "MiniMax"
base_url = "http://127.0.0.1:8000/v1"
env_key = "MINIMAX_API_KEY"
wire_api = "responses"
requires_openai_auth = false
`)
	got, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pc := got["minimax"]
	if pc.WireAPI != "responses" {
		t.Errorf("WireAPI = %q, want responses", pc.WireAPI)
	}
	if pc.RequiresOpenAIAuth == nil {
		t.Fatalf("RequiresOpenAIAuth = nil, want &false")
	}
	if *pc.RequiresOpenAIAuth {
		t.Errorf("*RequiresOpenAIAuth = true, want false")
	}
}

// TestParseRequiresOpenAITrue covers the inverse case where the
// provider DOES need OpenAI OAuth.
func TestParseRequiresOpenAITrue(t *testing.T) {
	src := []byte(`
[model_providers.openai]
base_url = "https://api.openai.com/v1"
env_key = "OPENAI_API_KEY"
requires_openai_auth = true
`)
	got, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pc := got["openai"]
	if pc.RequiresOpenAIAuth == nil {
		t.Fatalf("RequiresOpenAIAuth = nil, want &true")
	}
	if !*pc.RequiresOpenAIAuth {
		t.Errorf("*RequiresOpenAIAuth = false, want true")
	}
}

// TestParseRequiresOpenAIAuthUnset covers the case where the
// field is absent — should remain nil so we don't write a
// false default into the per-spec config.
func TestParseRequiresOpenAIAuthUnset(t *testing.T) {
	src := []byte(`
[model_providers.openai]
base_url = "https://api.openai.com/v1"
env_key = "OPENAI_API_KEY"
`)
	got, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got["openai"].RequiresOpenAIAuth != nil {
		t.Errorf("RequiresOpenAIAuth = %v, want nil (unset)", *got["openai"].RequiresOpenAIAuth)
	}
}
