package daemonlock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// hostname returns the best-effort short hostname of the current
// machine. Falls back to "unknown" if the OS call fails; holder
// metadata is for diagnostics, not auth, so it must always be
// writable.
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// writeHolder serializes the Holder as indented JSON to the given
// path, creating the file mode 0600 if absent. Atomic write via
// rename so a concurrent reader never sees a partial file.
func writeHolder(path string, h Holder) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readHolder parses the sidecar JSON. If the file does not exist,
// returns (nil, nil) — "no holder" is valid during the brief
// window between lock acquisition and JSON write, and after
// [Lock.Release] has cleaned up.
func readHolder(path string) (*Holder, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read holder %s: %w", path, err)
	}
	var h Holder
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("parse holder %s: %w", path, err)
	}
	return &h, nil
}
