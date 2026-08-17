package agentregistry

import (
	"strings"
)

// Conductor identity env vars. The naming follows multica's
// `goal_manager.py`: a primary var plus a fallback that lets a parent
// runtime forward identity to its child without re-implementing the
// precedence logic in every spawn site.
//
// Operators spawning subagents should set:
//
//	CONDUCTOR_PARENT_AGENT_ID=<id-of-spawner>
//	CONDUCTOR_PARENT_RUN_ID  =<id-of-spawner-run>
//	CONDUCTOR_PARENT_SESSION_ID=<backend-session-id>
//
// inside the child process env so the registry can stitch the parent
// link without the operator touching the YAML.
const (
	EnvAgentID        = "CONDUCTOR_AGENT_ID"
	EnvParentAgentID  = "CONDUCTOR_PARENT_AGENT_ID"
	EnvParentRunID    = "CONDUCTOR_PARENT_RUN_ID"
	EnvParentSessID   = "CONDUCTOR_PARENT_SESSION_ID"
	EnvSessionID      = "CONDUCTOR_SESSION_ID"
	EnvClaudeSessID   = "CLAUDE_CODE_SESSION_ID" // Claude backend writes this
	EnvCodexThreadID  = "CODEX_THREAD_ID"       // Codex backend writes this
)

// CurrentAgentID reads the agent id the calling process is operating
// on, honouring the same fallback chain multica uses:
//
//	CONDUCTOR_AGENT_ID > CONDUCTOR_PARENT_AGENT_ID > ""
//
// Empty result is reported as "" (not an error) so the caller can
// decide whether identity is required.
func CurrentAgentID(env []string) string {
	return firstEnv(env, EnvAgentID, EnvParentAgentID)
}

// CurrentSessionID returns the backend-stable session id of the current
// run. The fallback chain matches multica + the two backends Conductor
// supports:
//
//	CLAUDE_CODE_SESSION_ID > CODEX_THREAD_ID > CONDUCTOR_SESSION_ID > CONDUCTOR_PARENT_SESSION_ID > ""
func CurrentSessionID(env []string) string {
	return firstEnv(env, EnvClaudeSessID, EnvCodexThreadID, EnvSessionID, EnvParentSessID)
}

// firstEnv returns the first non-empty env entry among keys.
func firstEnv(env []string, keys ...string) string {
	for _, k := range keys {
		for _, e := range env {
			eq := strings.IndexByte(e, '=')
			if eq < 0 {
				continue
			}
			if e[:eq] != k {
				continue
			}
			v := strings.TrimSpace(e[eq+1:])
			if v != "" {
				return v
			}
		}
	}
	return ""
}

// IdentityEnv returns the env fragment a child process should inherit
// so the registry can stitch the parent/child relationship without the
// operator prop drilling. Pass through the result of
// [os.Environ] to a CLI spawn:
//
//	env = append(env, agentregistry.IdentityEnv(parentID, runID, sid)...)
func IdentityEnv(agentID string, runID int64, sessionID string) []string {
	out := make([]string, 0, 3)
	if agentID != "" {
		out = append(out, EnvAgentID+"="+agentID)
	}
	if runID > 0 {
		out = append(out, EnvParentRunID+"="+itoa(runID))
	}
	if sessionID != "" {
		out = append(out, EnvParentSessID+"="+sessionID)
	}
	return out
}

// itoa without importing strconv twice in callers (kept private —
// callers should use fmt.Sprintf).
func itoa(n int64) string {
	// small, no allocation through fmt package for the common case
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
