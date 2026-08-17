package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── JSON → TOML inline ──────────────────────────────────────────────────

func TestJsonValueToCodexTOMLInline_Basic(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{true, "true"},
		{false, "false"},
		{float64(42), "42"},
		{float64(1.5), "1.5"},
		{"hi", `"hi"`},
		{[]any{"a", "b"}, `["a", "b"]`},
	}
	for _, c := range cases {
		got, err := jsonValueToCodexTOMLInline(c.in)
		if err != nil {
			t.Errorf("jsonValueToCodexTOMLInline(%v) err = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("jsonValueToCodexTOMLInline(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJsonValueToCodexTOMLInline_RejectsNull(t *testing.T) {
	if _, err := jsonValueToCodexTOMLInline(nil); err == nil {
		t.Errorf("expected error for nil (TOML has no null)")
	}
}

func TestJsonValueToCodexTOMLInline_StringEscaping(t *testing.T) {
	// Test each special character independently to keep the test
	// readable. codexTOMLBasicString must:
	//   - escape backslash as \\
	//   - escape " as \"
	//   - escape newline as \n
	//   - leave other characters alone
	cases := []struct {
		in, want string
	}{
		{`a\b`, `"a\\b"`},   // raw backslash
		{`a"b`,  `"a\"b"`},  // raw double-quote
		{"a\nb", `"a\nb"`},   // real newline
		{`a	b`,  `"a\tb"`},   // raw tab
		{"plain", `"plain"`},   // no specials
	}
	for _, c := range cases {
		got, err := jsonValueToCodexTOMLInline(c.in)
		if err != nil {
			t.Errorf("err for %q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("escape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJsonValueToCodexTOMLInline_InlineTable(t *testing.T) {
	got, err := jsonValueToCodexTOMLInline(map[string]any{"k": "v", "n": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	// Keys sorted: k, n
	want := `{k = "v", n = 1}`
	if got != want {
		t.Errorf("inline table = %q, want %q", got, want)
	}
}

// ── Bare-key validation ─────────────────────────────────────────────────

func TestIsCodexBareTomlKey(t *testing.T) {
	good := []string{"fetch", "my-server", "server1", "a-b_c"}
	for _, k := range good {
		if !isCodexBareTomlKey(k) {
			t.Errorf("isCodexBareTomlKey(%q) = false, want true", k)
		}
	}
	bad := []string{"", "with space", "with.dot", "with/slash", "with:colon"}
	for _, k := range bad {
		if isCodexBareTomlKey(k) {
			t.Errorf("isCodexBareTomlKey(%q) = true, want false", k)
		}
	}
}

// ── Normalise server (stdio vs http) ────────────────────────────────────

func TestNormalizeCodexMcpServer_Stdio(t *testing.T) {
	in := map[string]any{
		"command": "npx",
		"args":    []any{"-y", "server"},
		"env":     map[string]any{"KEY": "value"},
		// Selector keys must be dropped.
		"tools":     []any{"a", "b"},
		"prompts":   []any{"p"},
		"resources": []any{"r"},
	}
	out := normalizeCodexMcpServer(in)
	if _, ok := out["tools"]; ok {
		t.Errorf("selector key 'tools' not dropped: %+v", out)
	}
	if _, ok := out["command"]; !ok {
		t.Errorf("stdio command dropped: %+v", out)
	}
}

func TestNormalizeCodexMcpServer_Http(t *testing.T) {
	in := map[string]any{
		"type":    "http",
		"url":     "https://example.com/mcp",
		"headers": map[string]any{"Authorization": "Bearer xyz"},
		"tools":   []any{"a"},
	}
	out := normalizeCodexMcpServer(in)
	if _, ok := out["type"]; ok {
		t.Errorf("http 'type' should be dropped: %+v", out)
	}
	if _, ok := out["headers"]; ok {
		t.Errorf("http 'headers' should be renamed to http_headers: %+v", out)
	}
	hdr, ok := out["http_headers"]
	if !ok {
		t.Fatalf("http_headers missing: %+v", out)
	}
	if _, ok := hdr.(map[string]any)["Authorization"]; !ok {
		t.Errorf("http_headers.Authorization missing: %+v", hdr)
	}
	if got, ok := out["experimental_use_rmcp_client"]; !ok || got != true {
		t.Errorf("experimental_use_rmcp_client should be true: %+v", out)
	}
	if _, ok := out["tools"]; ok {
		t.Errorf("selector key 'tools' not dropped: %+v", out)
	}
}

// ── Render the managed block ───────────────────────────────────────────

func TestRenderCodexMcpServersBlock_Stdio(t *testing.T) {
	in := json.RawMessage(`{"mcpServers":{"fetch":{"command":"npx","args":["-y","fs"],"env":{"K":"v"}}}}`)
	block, has, err := renderCodexMcpServersBlock(in)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Errorf("hasServers should be true")
	}
	if !strings.Contains(block, codexMcpBeginMarker) {
		t.Errorf("missing BEGIN marker: %s", block)
	}
	if !strings.Contains(block, codexMcpEndMarker) {
		t.Errorf("missing END marker: %s", block)
	}
	if !strings.Contains(block, "[mcp_servers.fetch]") {
		t.Errorf("missing fetch table: %s", block)
	}
	if !strings.Contains(block, `command = "npx"`) {
		t.Errorf("missing command: %s", block)
	}
	if !strings.Contains(block, `args = ["-y", "fs"]`) {
		t.Errorf("missing args: %s", block)
	}
	// env is an inline table; key sorted before value
	if !strings.Contains(block, `env = {K = "v"}`) {
		t.Errorf("missing env inline table: %s", block)
	}
}

func TestRenderCodexMcpServersBlock_EmptyManaged(t *testing.T) {
	// `{}` is "managed but empty" — must produce a (zero-table) block
	// wrapped in markers so the next run can find and strip it.
	in := json.RawMessage(`{}`)
	block, has, err := renderCodexMcpServersBlock(in)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Errorf("hasServers should be false for empty config")
	}
	if block != "" {
		t.Errorf("empty managed config should render no block, got %q", block)
	}
}

func TestRenderCodexMcpServersBlock_RejectsBadKey(t *testing.T) {
	in := json.RawMessage(`{"mcpServers":{"with space":{"command":"x"}}}`)
	if _, _, err := renderCodexMcpServersBlock(in); err == nil {
		t.Errorf("expected error for non-bare server name")
	}
}

func TestRenderCodexMcpServersBlock_Http(t *testing.T) {
	in := json.RawMessage(`{"mcpServers":{"remote":{"type":"http","url":"https://x/mcp","headers":{"Auth":"Bearer y"}}}}`)
	block, _, err := renderCodexMcpServersBlock(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "[mcp_servers.remote]") {
		t.Errorf("missing remote table: %s", block)
	}
	if strings.Contains(block, `type = "http"`) {
		t.Errorf("http type should be stripped (must NOT contain `type = \"http\"`), got: %s", block)
	}
	if !strings.Contains(block, `url = "https://x/mcp"`) {
		t.Errorf("missing url: %s", block)
	}
	if !strings.Contains(block, `http_headers = {Auth = "Bearer y"}`) {
		t.Errorf("missing http_headers: %s", block)
	}
	if !strings.Contains(block, `experimental_use_rmcp_client = true`) {
		t.Errorf("missing experimental_use_rmcp_client: %s", block)
	}
}

// ── Strip user tables ──────────────────────────────────────────────────

func TestStripCodexUserMcpServerTables(t *testing.T) {
	content := `# user comment
api_key = "sk-x"

[mcp_servers.user_one]
command = "x"

[mcp_servers.user_one.env]
TOKEN = "y"

[other_section]
foo = "bar"
`
	out := stripCodexUserMcpServerTables(content)
	// user table + sub-table removed
	if strings.Contains(out, "mcp_servers.user_one") {
		t.Errorf("user table not stripped: %s", out)
	}
	// non-mcp table preserved
	if !strings.Contains(out, "[other_section]") || !strings.Contains(out, `foo = "bar"`) {
		t.Errorf("non-mcp section stripped: %s", out)
	}
	if !strings.Contains(out, `api_key = "sk-x"`) {
		t.Errorf("non-mcp value stripped: %s", out)
	}
}

// ── Full file rewrite ──────────────────────────────────────────────────

func TestEnsureCodexMcpConfig_WritesManagedBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	// Seed with a user table that should be stripped on rewrite.
	if err := os.WriteFile(configPath, []byte(`api_key = "sk-x"

[mcp_servers.user_one]
command = "user-cmd"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	mcp := json.RawMessage(`{"mcpServers":{"fetch":{"command":"npx","args":["-y","fs"]}}}`)
	if err := ensureCodexMcpConfig(configPath, mcp); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)

	// User table should be stripped.
	if strings.Contains(s, "user-cmd") {
		t.Errorf("user table not stripped: %s", s)
	}
	// Managed block present.
	if !strings.Contains(s, codexMcpBeginMarker) || !strings.Contains(s, codexMcpEndMarker) {
		t.Errorf("managed block missing: %s", s)
	}
	if !strings.Contains(s, "[mcp_servers.fetch]") {
		t.Errorf("fetch table missing: %s", s)
	}
	// Mode is 0o600.
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 0600", got)
	}
	// Non-mcp content preserved.
	if !strings.Contains(s, `api_key = "sk-x"`) {
		t.Errorf("non-mcp content stripped: %s", s)
	}
}

func TestEnsureCodexMcpConfig_NilManaged_StripsPriorBlock(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	prior := `# BEGIN conductor-managed mcp_servers (do not edit; regenerated by conductor)
[mcp_servers.prior]
command = "old"
# END conductor-managed mcp_servers

[other]
keep = "yes"
`
	if err := os.WriteFile(configPath, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	// nil → "no managed config" → strip prior block, keep other.
	if err := ensureCodexMcpConfig(configPath, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if strings.Contains(s, "prior") {
		t.Errorf("prior block not stripped: %s", s)
	}
	if !strings.Contains(s, "[other]") {
		t.Errorf("other section stripped: %s", s)
	}
}

func TestEnsureCodexMcpConfig_Idempotent(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	mcp := json.RawMessage(`{"mcpServers":{"fetch":{"command":"x"}}}`)
	// Run twice — second call must produce identical bytes.
	if err := ensureCodexMcpConfig(configPath, mcp); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(configPath)
	if err := ensureCodexMcpConfig(configPath, mcp); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(configPath)
	if string(first) != string(second) {
		t.Errorf("idempotent run diverged:\nbefore:\n%s\nafter:\n%s", first, second)
	}
}

func TestEnsureCodexMcpConfig_MissingFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml") // does not exist
	mcp := json.RawMessage(`{"mcpServers":{"fetch":{"command":"x"}}}`)
	if err := ensureCodexMcpConfig(configPath, mcp); err != nil {
		t.Fatalf("expected no error on missing file, got %v", err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Errorf("config file not created: %v", err)
	}
}
