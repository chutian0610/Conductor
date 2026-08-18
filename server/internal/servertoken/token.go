package servertoken

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultPath is the Linux-literal default location for the
// generated bearer token: $HOME/.config/conductor/serve.token.
// It is intentionally NOT resolved via os.UserConfigDir() —
// ADR-0010 §3 + Update log (2026-08-18 (a)) pins this verbatim.
// macOS / Windows operators are expected to override --token-out.
const DefaultRelativePath = ".config/conductor/serve.token"

// DefaultEnvVar is the environment variable name that overrides
// the on-disk token when set and non-empty.
const DefaultEnvVar = "CONDUCTOR_TOKEN"

// LoadOrGenerate resolves the bearer token for `conductor serve`:
//
//  1. If envVar is set and non-empty, that string is returned;
//     no file I/O happens.
//  2. Else, if path points to an existing file, its contents are
//     returned (any trailing whitespace stripped).
//  3. Else, a fresh token is generated, written atomically to
//     path mode 0600, and returned.
//
// If envVar is set but the file path doesn't exist yet (operator
// chose env-only mode), no file is created.
//
// If envVar is unset and the file path is required, the parent
// directory is created mode 0755 if absent; the file itself is
// created mode 0600.
func LoadOrGenerate(path, envVar string) (string, error) {
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}

	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) == 0 {
			return "", fmt.Errorf("servertoken: file %s is empty", path)
		}
		return string(data), nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("servertoken: read %s: %w", path, err)
	}

	tok, err := generateToken(32)
	if err != nil {
		return "", fmt.Errorf("servertoken: generate: %w", err)
	}

	if err := writeAtomic(path, tok); err != nil {
		return "", fmt.Errorf("servertoken: write %s: %w", path, err)
	}
	return tok, nil
}

// generateToken returns a fresh, URL-safe, 32-byte token
// (43 chars of base64-no-padding).
func generateToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// writeAtomic writes content to path mode 0600 via write-then-rename.
// The parent directory is created mode 0755 if absent. The rename
// is atomic on the same filesystem, so a concurrent reader never
// sees a partial file.
func writeAtomic(path, content string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
