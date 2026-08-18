// Package httpserver implements the V2 HTTP transport for
// `conductor serve`. Per ADR-0010, the daemon exposes a small
// REST/JSON surface rooted at `/v1/`, with one-way SSE for
// streaming. This package owns the listener, the bearer-token
// authentication middleware, and the (currently minimal) route
// table.
//
// Step 1 ships the bare minimum:
//
//   - Bearer token authentication (constant-time compare),
//   - one public route, `GET /v1/healthz`, for liveness probes,
//   - graceful shutdown on signal.
//
// Subsequent steps (audit endpoints, agent CRUD over HTTP, run
// lifecycle, SSE streaming) land in follow-up PRs per ADR-0010
// §9.
package httpserver

// Version is the daemon version exposed on /v1/healthz.
// Updated by the release process.
const Version = "0.2.0-dev"
