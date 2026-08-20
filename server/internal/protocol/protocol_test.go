package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestAgentSessionConfigJSONRoundTrip ensures the wire types serialize
// and deserialize losslessly. The protocol package is the canonical
// source of truth; if a struct field can't survive JSON, that's a
// real bug (see §3 / §4 of docs/design.md on wire schema stability).
func TestAgentSessionConfigJSONRoundTrip(t *testing.T) {
	orig := AgentSessionConfig{
		Provider:     ProviderCodex,
		Model:        "claude-opus-4-6",
		Cwd:          "/tmp/proj",
		SystemPrompt: "you are a careful reviewer",
		Thinking:     "high",
		ToolsAllow:   []string{"read", "grep"},
		ToolsExclude: []string{"bash"},
		MCPConfig:    "/etc/mcp.json",
		SessionId:    "sess-abc",
	}

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got AgentSessionConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, orig) {
		t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, orig)
	}
}

// TestRefJSONRoundTrip exercises every RefKind to make sure the
// per-kind fields survive (de)serialization.
func TestRefJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		ref  Ref
	}{
		{
			name: "file",
			ref: Ref{
				Kind:  RefFile,
				Name:  "diff",
				Path:  "/tmp/x.diff",
				Mime:  "text/x-diff",
				Bytes: 24180,
			},
		},
		{
			name: "worktree",
			ref: Ref{
				Kind:       RefWorktree,
				Name:       "wt1",
				WorktreeID: "wt-xyz",
				Branch:     "feat/auth",
			},
		},
		{
			name: "session",
			ref: Ref{
				Kind:      RefSession,
				Name:      "plan",
				Provider:  ProviderCodex,
				SessionID: "sess-1",
				Handle: &AgentPersistenceHandle{
					Provider:   ProviderCodex,
					SessionId:  "sess-1",
					SessionDir: "/home/.codex/sessions",
				},
			},
		},
		{
			name: "blob",
			ref: Ref{
				Kind:    RefBlob,
				Name:    "tarball",
				BlobID:  "blob-1",
				Sha256:  "abc123",
				BlobURL: "/blobs/blob-1",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.ref)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Ref
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, tc.ref) {
				t.Errorf("round-trip mismatch:\n got  %+v\n want %+v", got, tc.ref)
			}
		})
	}
}
