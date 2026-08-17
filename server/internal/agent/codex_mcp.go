package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// codex_mcp.go materialises the daemon-side `mcp_config` payload into
// `$CODEX_HOME/config.toml`. Mirrors multica's
// server/pkg/agent/codex.go:ensureCodexMcpConfig + renderCodexMcpServersBlock
// + jsonValueToCodexTOMLInline + normalizeCodexMcpServerConfig, stripped
// to V1 scope:
//
//   - stdio + http MCP transports
//   - env values that are strings, numbers, bools, arrays, inline tables
//   - strict-mode semantics: when managed mcp_config is present, the
//     user's existing [mcp_servers.*] tables are stripped from the
//     per-task config.toml so the managed set wins (mirrors Claude's
//     `--strict-mcp-config`).
//
// Multica does much more (shell_environment_policy, per-provider config
// layers, daemon-managed MCP block markers for ops greppability). Those
// are V2 concerns; add helpers here when needed.

// ── Public API ──────────────────────────────────────────────────────────

// ensureCodexMcpConfig writes (or clears) the daemon-managed
// `[mcp_servers.*]` block in $CODEX_HOME/config.toml. With a non-empty
// managed mcp_config, the file ends up with the managed block; with
// nil/null mcp_config, any prior managed block is removed and the
// user's inherited tables are preserved (CLI default fallback).
//
// File mode is 0o600 because `mcp_servers.<id>.env` values may carry
// secrets (Anthropic API keys, custom provider tokens, etc.) — argv
// would echo them via ps/logs, so we never inline them.
func ensureCodexMcpConfig(configPath string, mcpConfig json.RawMessage) error {
	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	existing := string(data)

	// Strip any prior managed block so reruns converge on a clean state.
	stripped := codexMcpBlockRe.ReplaceAllString(existing, "")

	managed := hasManagedCodexMcpConfig(mcpConfig)
	block, _, renderErr := renderCodexMcpServersBlock(mcpConfig)
	if renderErr != nil {
		return renderErr
	}

	var updated string
	if managed {
		// Agent has a managed MCP set. Two reasons we strip user tables too:
		//   1. TOML rejects redefining the same table — a user table named
		//      `[mcp_servers.fetch]` would crash codex if the agent also
		//      defined `fetch`.
		//   2. An admin saving an explicit list in the UI would otherwise
		//      see user-global servers silently joined in.
		// Mirrors Claude's `--strict-mcp-config`.
		stripped = stripCodexUserMcpServerTables(stripped)
		stripped = strings.TrimRight(stripped, "\n")
		if block == "" {
			// Managed-but-empty: pin the markers with no tables between so
			// the next run can find and strip them deterministically.
			block = codexMcpBeginMarker + "\n" + codexMcpEndMarker + "\n"
		}
		if stripped == "" {
			updated = block
		} else {
			updated = stripped + "\n\n" + block
		}
	} else {
		// No managed config: just remove any prior managed block.
		updated = stripped
	}

	if updated == existing {
		return nil
	}
	if err := os.WriteFile(configPath, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	// os.WriteFile applies mode only when creating; if the file existed at
	// 0o644 (execenv.seedFile's default), the secret-bearing values we
	// just wrote would inherit that wider mode. Chmod unconditionally.
	if err := os.Chmod(configPath, 0o600); err != nil {
		return fmt.Errorf("chmod %s to 0600: %w", configPath, err)
	}
	return nil
}

// hasManagedCodexMcpConfig reports whether raw carries a non-null, non-empty
// MCP config object. `{}` and `{"mcpServers":{}}` count as "saved an
// empty set" — distinct from nil/null which means "fall back to CLI
// default". The three-state semantics matter for codex's strict mode.
func hasManagedCodexMcpConfig(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

// ── JSON → TOML rendering ───────────────────────────────────────────────

// renderCodexMcpServersBlock renders the mcp_config JSON
// (Claude-style `{"mcpServers": {...}}`) as a TOML block of
// `[mcp_servers.<name>]` tables wrapped in BEGIN/END markers.
//
// Returns (block, hasServers, err); hasServers=false means the input
// had no servers to render and the caller should only strip the prior
// managed block.
func renderCodexMcpServersBlock(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	var parsed struct {
		McpServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", false, fmt.Errorf("parse mcp_config json: %w", err)
	}
	if len(parsed.McpServers) == 0 {
		return "", false, nil
	}

	names := make([]string, 0, len(parsed.McpServers))
	for name := range parsed.McpServers {
		names = append(names, name)
	}
	sort.Strings(names)

	var sb strings.Builder
	sb.WriteString(codexMcpBeginMarker)
	sb.WriteString("\n")
	for i, name := range names {
		if !isCodexBareTomlKey(name) {
			return "", false, fmt.Errorf("mcp server name %q must be ASCII alphanumeric / _ / - to fit Codex's bare-key requirement", name)
		}
		var serverVal map[string]any
		if err := json.Unmarshal(parsed.McpServers[name], &serverVal); err != nil {
			return "", false, fmt.Errorf("mcp_servers.%s: %w", name, err)
		}
		if serverVal == nil {
			return "", false, fmt.Errorf("mcp_servers.%s must be a JSON object", name)
		}
		serverVal = normalizeCodexMcpServer(serverVal)
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("[mcp_servers.")
		sb.WriteString(name)
		sb.WriteString("]\n")
		keys := make([]string, 0, len(serverVal))
		for k := range serverVal {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			tomlValue, err := jsonValueToCodexTOMLInline(serverVal[k])
			if err != nil {
				return "", false, fmt.Errorf("mcp_servers.%s.%s: %w", name, k, err)
			}
			sb.WriteString(codexTOMLKey(k))
			sb.WriteString(" = ")
			sb.WriteString(tomlValue)
			sb.WriteString("\n")
		}
	}
	sb.WriteString(codexMcpEndMarker)
	sb.WriteString("\n")
	return sb.String(), true, nil
}

// normalizeCodexMcpServer rewrites the conductor-side MCP server shape
// into codex's config.toml shape. Two transports today:
//
//   stdio:  {command, args, env, cwd?}          → pass through (drop selector keys)
//   http:   {url, headers?, type?}              → rename headers → http_headers,
//                                                drop `type`, force
//                                                experimental_use_rmcp_client = true
//
// `tools`/`prompts`/`resources` are conductor UI selector keys, not
// protocol-level — they are dropped here so they don't confuse codex when
// it tries to parse the file.
func normalizeCodexMcpServer(server map[string]any) map[string]any {
	if !isCodexRemoteMcpServer(server) {
		normalized := make(map[string]any, len(server))
		for k, v := range server {
			if isConductorMcpSelectorKey(k) {
				continue
			}
			normalized[k] = v
		}
		return normalized
	}
	normalized := make(map[string]any, len(server)+1)
	for k, v := range server {
		switch {
		case isConductorMcpSelectorKey(k):
			continue
		case k == "type":
			continue
		case k == "headers":
			if _, ok := server["http_headers"]; !ok {
				normalized["http_headers"] = v
			}
		default:
			normalized[k] = v
		}
	}
	normalized["experimental_use_rmcp_client"] = true
	return normalized
}

func isConductorMcpSelectorKey(k string) bool {
	switch k {
	case "tools", "prompts", "resources":
		return true
	default:
		return false
	}
}

func isCodexRemoteMcpServer(server map[string]any) bool {
	if typ, ok := server["type"].(string); ok && strings.EqualFold(typ, "http") {
		return true
	}
	_, hasURL := server["url"]
	_, hasCommand := server["command"]
	return hasURL && !hasCommand
}

// stripCodexUserMcpServerTables removes every `[mcp_servers.*]` table
// (header + body lines until the next top-level table header or EOF)
// from a TOML config string. Sub-tables like `[mcp_servers.fetch.env]`
// count as part of the parent table and are dropped along with it.
func stripCodexUserMcpServerTables(content string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		if userCodexMcpTableHeaderRe.MatchString(line) {
			skipping = true
			continue
		}
		if skipping {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "[") {
				// Next table header. If it's still mcp_servers.* (incl.
				// sub-tables), keep skipping; otherwise stop and emit.
				if userCodexMcpTableHeaderRe.MatchString(line) ||
					strings.HasPrefix(trimmed, "[mcp_servers.") ||
					strings.HasPrefix(trimmed, "[ mcp_servers.") {
					continue
				}
				skipping = false
				out = append(out, line)
				continue
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// ── JSON value → TOML inline value ───────────────────────────────────────

// jsonValueToCodexTOMLInline serialises a JSON value as a TOML inline
// value. Only the subset Codex's `-c` accepts is supported: strings,
// numbers, booleans, arrays, and inline tables. JSON nulls are rejected
// because TOML has no null and silently dropping them would be confusing.
func jsonValueToCodexTOMLInline(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", fmt.Errorf("null is not a valid TOML value")
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case string:
		return codexTOMLBasicString(x), nil
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			p, err := jsonValueToCodexTOMLInline(e)
			if err != nil {
				return "", err
			}
			parts[i] = p
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			tv, err := jsonValueToCodexTOMLInline(x[k])
			if err != nil {
				return "", err
			}
			parts[i] = codexTOMLKey(k) + " = " + tv
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	default:
		return "", fmt.Errorf("unsupported TOML value type %T", v)
	}
}

// codexTOMLBasicString renders a Go string as a TOML basic string
// (double-quoted, with backslash and double-quote escaping). Newlines
// become the literal two-character sequence `\n` which TOML parsers
// re-expand — this is the only escape Codex's TOML parser actually
// accepts in basic strings.
func codexTOMLBasicString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// codexTOMLKey renders a Go string as a TOML bare key. Bare keys
// must match `[A-Za-z0-9_-]+`. Non-conforming keys need to be quoted
// (which Codex's config.toml parser may or may not accept — we reject
// them at the validation boundary above).
func codexTOMLKey(k string) string {
	if isCodexBareTomlKey(k) {
		return k
	}
	return codexTOMLBasicString(k)
}

func isCodexBareTomlKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// ── Markers + regexes ───────────────────────────────────────────────────

const (
	codexMcpBeginMarker = "# BEGIN conductor-managed mcp_servers (do not edit; regenerated by conductor)"
	codexMcpEndMarker   = "# END conductor-managed mcp_servers"
)

// codexMcpBlockRe matches a full managed block between BEGIN/END markers,
// including the BEGIN/END lines themselves. Replacement with "" removes
// the block from a prior config.toml.
//
// Note: QuoteMeta is essential — the markers contain parens
// ("(do not edit; regenerated by conductor)") that Go's regexp
// would otherwise interpret as group operators, silently making
// the pattern never match.
var codexMcpBlockRe = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(codexMcpBeginMarker) + `.*?` + regexp.QuoteMeta(codexMcpEndMarker) + `\n?`)

// for integration tests. Do not call from production code.

// for integration tests. Do not call from production code.

// for integration tests. Do not call from production code.

// userCodexMcpTableHeaderRe matches `[mcp_servers.X]` (and its
// quoted-key form `[mcp_servers."X"]`) at the start of a line. Used
// to strip user-provided mcp_servers tables from the per-task config
// when the agent has its own mcp_config — mirrors Claude's
// `--strict-mcp-config`.
var userCodexMcpTableHeaderRe = regexp.MustCompile(`^\s*\[\s*mcp_servers\s*\.\s*(?:"[^"]*"|[^\]\s]+)\s*\]\s*(?:#.*)?$`)
