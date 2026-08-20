package spec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"conductor/server/internal/home"
	"conductor/server/internal/protocol"
)

// ErrNotFound is returned when a spec doesn't exist.
var ErrNotFound = errors.New("spec: not found")

// ErrAlreadyExists is returned by Create when the derived SpecId
// already has a spec.json on disk. Because SpecId is content-
// deterministic, this means the same spec content was already
// registered (idempotent re-run).
var ErrAlreadyExists = errors.New("spec: already exists")

// ErrProviderRequired is returned by Create when the spec lacks a
// provider. Phase 1 only knows about codex, but the package stays
// provider-agnostic — it just refuses to register a spec that
// couldn't possibly create a working HOME.
var ErrProviderRequired = errors.New("spec: provider required")

// CreateInput is the user-supplied shape for Create. The caller
// (typically the CLI layer) is responsible for resolving BaseURL
// and EnvKey from the user's ~/.codex/config.toml; this package
// doesn't read that file directly (it's a codex-specific concern
// that should not leak into the storage layer).
type CreateInput struct {
	// Spec is the user-given AgentSpec (without metadata fields).
	Spec protocol.AgentSpec

	// BaseURL is the resolved base URL for the chosen provider
	// (e.g. "https://api.openai.com/v1"). Empty is allowed for
	// providers that don't need a remote endpoint (Phase 2+).
	BaseURL string

	// EnvKey is the name of the env var that holds the API key
	// (e.g. "OPENAI_API_KEY"). Empty for local providers (Ollama).
	EnvKey string
}

// CreateResult is what Create returns on success.
type CreateResult struct {
	// SpecId is the derived id (same as input.Spec.SpecId, for
	// convenience).
	SpecId string

	// Record is the on-disk SpecRecord (Spec + HomePath + ConfigToml).
	Record protocol.SpecRecord
}

// Create registers a new spec: derives a content-deterministic
// SpecId, allocates the per-spec HOME, writes config.toml + auth
// symlink via home.IsolatedHome.Setup, then writes spec.json with
// the resulting SpecRecord.
//
// Idempotent: re-running with the same input returns ErrAlreadyExists
// instead of creating a duplicate. Atomicity: spec.json is written
// last, so a failure before that step leaves no spec record behind.
// The HOME is rolled back if the spec.json write fails.
//
// Errors:
//   - ErrProviderRequired: spec.Provider == ""
//   - ErrAlreadyExists: a spec.json already exists at the derived
//     SpecId's path (caller can decide whether to overwrite, but
//     we don't auto-overwrite — content-equivalence implies
//     identity, so silent overwrite is dangerous).
//   - any underlying filesystem / JSON error.
func Create(ctx context.Context, in CreateInput) (CreateResult, error) {
	// Bail out before touching the filesystem if the caller has
	// already cancelled (saves a HOME we have to roll back).
	if err := ctx.Err(); err != nil {
		return CreateResult{}, err
	}
	if in.Spec.Provider == "" {
		return CreateResult{}, ErrProviderRequired
	}

	specId, err := DeriveSpecId(in.Spec)
	if err != nil {
		return CreateResult{}, fmt.Errorf("derive SpecId: %w", err)
	}

	if err := home.EnsureBaseDirs(); err != nil {
		return CreateResult{}, fmt.Errorf("ensure base dirs: %w", err)
	}

	// Check for collision BEFORE we set up the HOME — that way a
	// failure here costs us nothing.
	metaPath := home.SpecMetaPath(specId)
	switch _, err := os.Stat(metaPath); {
	case err == nil:
		return CreateResult{}, ErrAlreadyExists
	case !errors.Is(err, os.ErrNotExist):
		return CreateResult{}, fmt.Errorf("stat spec.json: %w", err)
	}

	// Ensure auth file slot exists (may already be populated by
	// `conductor auth init`); the path is where we'll symlink from.
	authSourcePath, err := home.EnsureAuthDirFile(string(in.Spec.Provider))
	if err != nil {
		return CreateResult{}, fmt.Errorf("ensure auth dir: %w", err)
	}

	// Set up the per-spec HOME.
	iso := home.New(specId, string(in.Spec.Provider))
	if err := iso.Setup(in.BaseURL, in.EnvKey, authSourcePath); err != nil {
		return CreateResult{}, fmt.Errorf("setup HOME: %w", err)
	}

	// Fill in metadata fields now that the id is known.
	now := time.Now().UTC()
	spec := in.Spec
	spec.SpecId = specId
	spec.CreatedAt = now
	spec.UpdatedAt = now

	record := protocol.SpecRecord{
		Spec:       spec,
		HomePath:   iso.HomeDir(),
		ConfigToml: iso.ConfigTomlPath(),
	}

	// Write spec.json last (atomicity: HOME rollback on failure).
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		_ = iso.Remove()
		return CreateResult{}, fmt.Errorf("marshal spec.json: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o700); err != nil {
		_ = iso.Remove()
		return CreateResult{}, fmt.Errorf("mkdir specs dir: %w", err)
	}
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		_ = iso.Remove()
		return CreateResult{}, fmt.Errorf("write spec.json: %w", err)
	}

	// Surface ctx cancellation that happened during the work above.
	if err := ctx.Err(); err != nil {
		_ = Remove(context.Background(), specId) // never cancelled: rollback always runs to completion
		return CreateResult{}, err
	}

	return CreateResult{SpecId: specId, Record: record}, nil
}

// Get reads the SpecRecord for the given SpecId. Returns ErrNotFound
// when spec.json is missing.
func Get(ctx context.Context, specId string) (*protocol.SpecRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(home.SpecMetaPath(specId))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read spec.json: %w", err)
	}
	var record protocol.SpecRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("parse spec.json: %w", err)
	}
	return &record, nil
}

// List returns every registered spec, sorted by SpecId. Missing or
// malformed spec.json entries are skipped silently (logged in Phase
// 2 once we have a logger hook).
func List(ctx context.Context) ([]protocol.SpecRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	specsDir := home.SpecsDir()
	entries, err := os.ReadDir(specsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read specs dir: %w", err)
	}
	records := make([]protocol.SpecRecord, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := Get(ctx, e.Name())
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("load spec %q: %w", e.Name(), err)
		}
		records = append(records, *rec)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].Spec.SpecId < records[j].Spec.SpecId
	})
	return records, nil
}

// Remove deletes both the per-spec HOME and spec.json. Returns
// ErrNotFound if spec.json is missing. The HOME is removed even if
// spec.json was already absent (so the inverse — HOME exists but
// metadata is gone — cleans up correctly).
//
// Idempotent at the file level: a missing HOME is not an error,
// but a missing spec.json IS, so callers can detect double-removes.
func Remove(ctx context.Context, specId string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// We need spec.json to be present to consider the spec
	// "registered" — otherwise Remove would silently nuke an
	// already-deleted spec on retry, hiding bugs.
	metaPath := home.SpecMetaPath(specId)
	if _, err := os.Stat(metaPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("stat spec.json: %w", err)
	}

	// Read the record first so we know which provider's HOME to
	// look for (defensive: even though the dir layout is fixed by
	// specId, future migrations may need this).
	record, err := Get(ctx, specId)
	if err != nil {
		return err
	}

	// Remove the HOME (missing is fine).
	if err := os.RemoveAll(record.HomePath); err != nil {
		return fmt.Errorf("remove HOME: %w", err)
	}

	// Remove spec.json.
	if err := os.Remove(metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove spec.json: %w", err)
	}

	// Best-effort: remove the empty specs/<specId>/ parent dir.
	_ = os.Remove(filepath.Join(home.SpecsDir(), specId))

	return nil
}
