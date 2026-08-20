package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
	"strings"

	"conductor/server/internal/protocol"
)

// DeriveSpecId returns a deterministic id for a spec. Format:
//
//   - "<sanitized-name>-<6-char-hash>" when spec.Name is non-empty
//   - "<16-char-hash>" otherwise
//
// The hash covers the spec CONTENT only — metadata fields
// (SpecId, CreatedAt, UpdatedAt) are stripped before hashing,
// so re-running Create with the same spec input always yields
// the same id. Name sanitization: lowercase, non-alphanumeric
// runs become single '-', trimmed at both ends, capped at 40 chars.
func DeriveSpecId(spec protocol.AgentSpec) (string, error) {
	// Strip metadata so the hash is content-only.
	spec.SpecId = ""
	spec.CreatedAt = zeroTime
	spec.UpdatedAt = zeroTime

	canonical, err := canonicalJSON(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	shortHash := hex.EncodeToString(sum[:3]) // 6 hex chars
	longHash := hex.EncodeToString(sum[:8])  // 16 hex chars

	name := SanitizeName(spec.Name)
	if name == "" {
		return longHash, nil
	}
	return name + "-" + shortHash, nil
}

// SanitizeName normalizes a user-given spec name into the prefix
// portion of a SpecId. The rules are deliberately conservative —
// filesystem-portable, CLI-friendly, no surprises.
//
//   - lowercase
//   - any run of characters outside [a-z0-9] becomes a single '-'
//   - leading and trailing '-' are trimmed
//   - truncated to 40 characters (and re-trimmed at the boundary
//     so we never end on a dangling '-')
//
// Returns "" if nothing alphanumeric survives.
func SanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	lastDash := false
	for _, r := range s {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlnum {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	const maxLen = 40
	if len(out) > maxLen {
		out = out[:maxLen]
		out = strings.TrimRight(out, "-")
	}
	return out
}

// canonicalJSON returns a deterministic JSON encoding of v with
// map keys sorted at every depth. Slices keep their order. This
// is what we hash to derive SpecId — without sorting, two specs
// that differ only in map-iteration order would hash differently.
//
// Limitations: struct fields marshal in declaration order (which
// is already deterministic), so we only need to re-sort if the
// value contains map[string]any (which appears after a
// json.Marshal/Unmarshal round-trip).
func canonicalJSON(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	return marshalSorted(parsed)
}

func marshalSorted(v any) ([]byte, error) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			vb, err := marshalSorted(x[k])
			if err != nil {
				return nil, err
			}
			buf.Write(vb)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			ib, err := marshalSorted(item)
			if err != nil {
				return nil, err
			}
			buf.Write(ib)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	default:
		return json.Marshal(v)
	}
}

// zeroTime is the zero-value time.Time used when stripping
// metadata before hashing. Defined as a var so tests can reference
// it (and so future refactors don't accidentally inline it).
var zeroTime = (time.Time{})
