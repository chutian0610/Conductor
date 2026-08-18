// Package ansiclean strips ANSI / CSI / OSC escape sequences from a
// string so the CLI's renderers don't surface terminal-control bytes
// that mix awkwardly into stdout (`\x1b[1m`, `\x1b[31m`, OSC titles,
// etc.).
//
// Why this exists:
//
// Claude Code's stream-json output occasionally surfaces model / log
// fields that contain raw ANSI (e.g. when an upstream tool prints
// with `tput bold`). The conductor wire handler stores them verbatim
// in `Result.Usage` and `Message.*` so the events table keeps a
// faithful replay log; the *renderer* then strips before printing so
// operators see clean output and downstream `grep` / pipelines don't
// see escape noise.
//
// What this does NOT do:
//
// Strip does NOT touch bracket-y text that is not preceded by an
// ESC byte — even when the bracket looks SGR-shaped
// (`[<digits>m`, `[<digits>m]`). Such shapes include legitimate
// Claude variant identifiers (`MiniMax-M3[1m]`) and any other
// identifier suffix convention. Operators may add an explicit
// orphan-stripping helper if they empirically observe clipped
// artifacts from a backend; until then, conservative is correct
// (per CLAUDE's variant-naming convention — see ADR-0008 "Update
// log" item (e)).
//
// Stripping intentionally happens only at the renderer boundary —
// never inside `backend.Result` or the events table. Operators who want
// the raw bytes can read the DB directly.
package ansiclean

import "regexp"

// ansi matches the escape sequences the LLM wire + shell emit:
//
//   - CSI:    ESC '[' params? final-byte  (covers SGR colours, bold,
//             cursor moves, line clears, etc.)
//   - OSC:    ESC ']' ... (BEL or ST)     (titles, hyperlink, etc.)
//   - Misc:   ESC single-byte 0x30-0x7E   (charset/attr, line-text
//             attributes, RIS, DECPAM, etc.)
//
// The regex favours readability and breadth over blazing performance;
// it's only ever called once per renderer call on field sizes that
// are short single-line strings (<4 KiB in practice).
var ansi = regexp.MustCompile(
	`\x1b` +
		`(?:` +
		// CSI: ESC [ params? (0x30-0x3F)* intermediate (0x20-0x2F)* final (0x40-0x7E)
		`\[[\x30-\x3F]*[\x20-\x2F]*[\x40-\x7E]` +
		// OSC: ESC ] ... (BEL | ESC \\)
		`|][^\x07\x1b]*(?:\x07|\x1b\\)` +
		// Single-char escape (charset/attr, range covers all
		// printable ASCII post-ESC bytes used in ECMA-48).
		`|[\x30-\x7E]` +
		`)`,
)

// Strip removes ANSI escape sequences from s. Input without escapes
// is returned unchanged (so the hot path is a no-op allocation-wise).
//
// Strip does NOT modify bracketed text. In particular, Claude variant
// identifiers like `MiniMax-M3[1m]` survive verbatim — see the
// package comment for the rationale.
func Strip(s string) string {
	if !containsESC(s) {
		return s
	}
	return ansi.ReplaceAllString(s, "")
}

// containsESC is a cheap fast-path check: most strings the renderers
// see have no escapes. A bytewise scan skips the regex compile cost.
func containsESC(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			return true
		}
	}
	return false
}
