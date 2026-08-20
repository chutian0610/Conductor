package codexconfig

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProviderConfig is one [model_providers.<id>] block from the
// user's codex config.toml.
type ProviderConfig struct {
	// ID is the suffix of [model_providers.<id>], e.g. "openai" or
	// "openrouter". This is what users pass as --provider.
	ID string

	// Name is an optional display name (Codex allows any string).
	// Conductor currently doesn't render it but keeps it for
	// future spec-list pretty printing.
	Name string

	// BaseURL is the API endpoint for this provider. Empty when
	// the user's config.toml omits it (we don't fail here so the
	// caller can decide — Phase 1's spec.create requires a URL).
	BaseURL string

	// EnvKey is the name of the env var that holds the API key
	// (e.g. "OPENROUTER_API_KEY"). Empty for local providers
	// (Ollama, embedded proxies) that don't need an API key.
	EnvKey string
}

// ErrProviderNotFound is returned by Resolve when the requested
// provider is missing from the user's config.toml AND it isn't the
// built-in "codex" provider.
var ErrProviderNotFound = errors.New("codexconfig: provider not found")

// ErrConfigNotFound is returned when the user's config.toml itself
// doesn't exist (distinct from "file exists but provider missing").
// Callers may want to surface this differently (e.g. "run `codex
// login` first") vs. ErrProviderNotFound.
var ErrConfigNotFound = errors.New("codexconfig: config.toml not found")

// Resolve looks up the named provider in the user's codex config
// at <home>/config.toml. home defaults to $CODEX_HOME, falling
// back to ~/.codex.
//
// Special case: provider == "codex" returns a synthetic built-in
// OpenAI config (https://api.openai.com/v1, env_key=OPENAI_API_KEY)
// even when the user has no config.toml at all — matches Codex
// CLI's own default behaviour, so users without a custom config
// can still `spec create --provider codex`.
func Resolve(home, provider string) (ProviderConfig, error) {
	if home == "" {
		home = defaultCodexHome()
	}
	path := filepath.Join(home, "config.toml")
	providers, err := ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if provider == "codex" {
				return builtInOpenAI(), nil
			}
			return ProviderConfig{}, fmt.Errorf("%w (path: %s)", ErrConfigNotFound, path)
		}
		return ProviderConfig{}, err
	}
	pc, ok := providers[provider]
	if !ok {
		if provider == "codex" {
			return builtInOpenAI(), nil
		}
		return ProviderConfig{}, fmt.Errorf("%w: %q (path: %s)", ErrProviderNotFound, provider, path)
	}
	return pc, nil
}

// ReadFile parses a codex config TOML file and returns every
// [model_providers.<id>] block keyed by id. Other sections are
// dropped silently.
func ReadFile(path string) (map[string]ProviderConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses TOML bytes for the [model_providers.*] subset. See
// package doc for the supported syntax.
//
// Errors are returned for malformed lines (unterminated string,
// section header without closing bracket) — these are real bugs
// the user should see. Missing fields are not errors; an empty
// ProviderConfig is fine if the user just wants the id registered.
func Parse(data []byte) (map[string]ProviderConfig, error) {
	providers := make(map[string]ProviderConfig)
	// current holds the in-progress ProviderConfig for the active
	// [model_providers.<id>] section. We can't take &providers[id]
	// directly (map elements aren't addressable in Go), so we keep
	// a local copy, mutate it, and re-store into the map on every
	// key=value update.
	var current ProviderConfig
	var inProviderSection bool

	for lineNo, rawLine := range bytes.Split(data, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Section header.
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("line %d: section header missing ']': %q", lineNo+1, line)
			}
			header := strings.TrimSpace(line[1 : len(line)-1])
			if strings.HasPrefix(header, "model_providers.") {
				id := strings.TrimPrefix(header, "model_providers.")
				current = ProviderConfig{ID: id}
				inProviderSection = true
				providers[id] = current
			} else {
				inProviderSection = false
			}
			continue
		}

		// Key = value (only inside [model_providers.*]).
		if !inProviderSection {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			// Malformed line inside a providers section — skip
			// silently. We don't want one weird user line to
			// break every conductor command.
			continue
		}
		key := strings.TrimSpace(line[:eq])
		valStr := strings.TrimSpace(line[eq+1:])
		value, err := parseValue(valStr)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w: %q", lineNo+1, err, line)
		}
		s, ok := value.(string)
		if !ok {
			continue
		}
		switch key {
		case "name":
			current.Name = s
		case "base_url":
			current.BaseURL = s
		case "env_key":
			current.EnvKey = s
		}
		// Re-store the local copy into the map so external readers
		// see the updated fields.
		providers[current.ID] = current
	}
	return providers, nil
}

// parseValue handles a single TOML value: a quoted string or a
// bare identifier. We don't need numbers / arrays / inline tables
// for [model_providers.*] (Codex uses only strings there).
func parseValue(s string) (any, error) {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return unquoteTOML(s)
	}
	// Bare identifier (e.g. `env_key = OPENAI_API_KEY`).
	return s, nil
}

// unquoteTOML strips surrounding quotes and applies the common
// escape sequences (\n, \t, \", \\). Phase 1 doesn't need full
// TOML escape coverage — only what's in real provider configs.
func unquoteTOML(s string) (string, error) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("unterminated string: %q", s)
	}
	inner := s[1 : len(s)-1]
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			switch inner[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				// Unknown \x — keep the character after \ literally
				// rather than failing the whole parse. This is more
				// permissive than TOML spec but harmless for our
				// subset (Codex never emits exotic escapes here).
				b.WriteByte(inner[i+1])
			}
			i++
			continue
		}
		b.WriteByte(inner[i])
	}
	return b.String(), nil
}

// defaultCodexHome returns $CODEX_HOME, or $HOME/.codex. Mirrors
// the Codex CLI's own resolution so users get the same file we'd
// expect them to edit.
func defaultCodexHome() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	uh, _ := os.UserHomeDir()
	return filepath.Join(uh, ".codex")
}

// builtInOpenAI is the synthetic default for provider "codex"
// when no config.toml exists or no entry for "codex" is found.
// Matches Codex CLI's default (https://api.openai.com/v1 +
// OPENAI_API_KEY).
func builtInOpenAI() ProviderConfig {
	return ProviderConfig{
		ID:      "codex",
		Name:    "OpenAI",
		BaseURL: "https://api.openai.com/v1",
		EnvKey:  "OPENAI_API_KEY",
	}
}
