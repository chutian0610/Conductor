package home

import (
	"fmt"
	"os"
	"path/filepath"
)

// ConductorHome returns the absolute path to the Conductor data
// directory, honoring $CONDUCTOR_HOME if set.
//
// Default: $HOME/.conductor (where $HOME is the user's real home).
// Override via env var, e.g.:
//
//	CONDUCTOR_HOME=/var/lib/conductor conductor spec create ...
func ConductorHome() string {
	if v := os.Getenv("CONDUCTOR_HOME"); v != "" {
		return v
	}
	// os.UserHomeDir returns $HOME on Unix, %USERPROFILE% on Windows.
	uh, _ := os.UserHomeDir()
	return filepath.Join(uh, ".conductor")
}

// AuthDir returns $CONDUCTOR_HOME/.auth (shared auth storage).
func AuthDir() string { return filepath.Join(ConductorHome(), ".auth") }

// SpecsDir returns $CONDUCTOR_HOME/specs (per-spec metadata + HOME).
func SpecsDir() string { return filepath.Join(ConductorHome(), "specs") }

// SpecHomeDir returns $CONDUCTOR_HOME/specs/<specId>/home.
func SpecHomeDir(specId string) string {
	return filepath.Join(SpecsDir(), specId, "home")
}

// SpecMetaPath returns the spec.json path (metadata, not HOME).
func SpecMetaPath(specId string) string {
	return filepath.Join(SpecsDir(), specId, "spec.json")
}

// EnsureBaseDirs creates $CONDUCTOR_HOME, .auth, and specs/ with
// 0700 permissions. Idempotent: existing directories are left
// alone (only the perms are tightened if they were looser).
func EnsureBaseDirs() error {
	for _, dir := range []string{
		ConductorHome(),
		AuthDir(),
		SpecsDir(),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return nil
}
