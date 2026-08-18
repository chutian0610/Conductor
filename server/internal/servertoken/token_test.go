package servertoken_test

import (
	"os"
	"path/filepath"
	"testing"

	"conductor/server/internal/servertoken"
)

func TestEnvWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONDUCTOR_TOKEN", "from-env")

	got, err := servertoken.LoadOrGenerate(path, "CONDUCTOR_TOKEN")
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if got != "from-env" {
		t.Errorf("got %q, want %q", got, "from-env")
	}
}

func TestEnvWinsOverGenerate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok")
	t.Setenv("CONDUCTOR_TOKEN", "from-env")

	got, err := servertoken.LoadOrGenerate(path, "CONDUCTOR_TOKEN")
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if got != "from-env" {
		t.Errorf("got %q, want %q (env should win even if file path missing)", got, "from-env")
	}
}

func TestGenerateNewAndFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "tok")
	t.Setenv("CONDUCTOR_TOKEN", "")

	got, err := servertoken.LoadOrGenerate(path, "CONDUCTOR_TOKEN")
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if len(got) < 32 {
		t.Errorf("generated token too short: %q (%d chars)", got, len(got))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat %s: %v", path, err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("token file mode = %o, want 0600", mode)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != got {
		t.Errorf("file content = %q, got %q", data, got)
	}
}

func TestReadExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok")
	if err := os.WriteFile(path, []byte("from-file-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONDUCTOR_TOKEN", "")

	got, err := servertoken.LoadOrGenerate(path, "CONDUCTOR_TOKEN")
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if got != "from-file-2" {
		t.Errorf("got %q, want %q", got, "from-file-2")
	}
}

func TestEmptyFileIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONDUCTOR_TOKEN", "")

	if _, err := servertoken.LoadOrGenerate(path, "CONDUCTOR_TOKEN"); err == nil {
		t.Fatal("expected error on empty token file")
	}
}
