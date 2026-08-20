package protocol

// Ref is a typed reference to a resource owned by the run. Refs are
// the only mechanism by which a stage output can hand a file /
// worktree / session / blob to a downstream stage without copying
// the underlying bytes through WorkflowState.
//
// Refs are always local to the per-spec HOME (or per-run worktree)
// in v0.13. There are no cross-host Ref kinds — see §12.0 of
// docs/design.md.
//
// Each kind has a stable textual representation suitable for
// inclusion in agent prompts. The provider package renders refs as
// "<ref name="X" file="..." />" markers so the agent can decide
// when to fetch.
type Ref struct {
	// Kind discriminates the union.
	Kind RefKind `json:"kind"`

	// Name is the user-facing label for this ref. e.g. "diff",
	// "worktree", "session". Required.
	Name string `json:"name"`

	// File-kind fields.
	Path  string `json:"path,omitempty"`  // absolute filesystem path
	Mime  string `json:"mime,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`

	// Worktree-kind fields.
	WorktreeID string `json:"worktreeId,omitempty"`
	Branch     string `json:"branch,omitempty"`

	// Session-kind fields.
	Provider  AgentProvider          `json:"provider,omitempty"`
	SessionID string                 `json:"sessionId,omitempty"`
	Handle    *AgentPersistenceHandle `json:"handle,omitempty"`

	// Blob-kind fields.
	BlobID  string `json:"blobId,omitempty"`
	Sha256  string `json:"sha256,omitempty"`
	BlobURL string `json:"blobUrl,omitempty"`
}

type RefKind string

const (
	RefFile     RefKind = "file"
	RefWorktree RefKind = "worktree"
	RefSession  RefKind = "session"
	RefBlob     RefKind = "blob"
)

// RefMap is the set of refs produced by a single stage output.
type RefMap map[string]Ref
