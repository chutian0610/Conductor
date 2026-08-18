package httpserver

import (
	"conductor/server/internal/runmgr"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

// Server is the V2 HTTP transport. Construct one with [New], then
// call [Serve] (test-friendly) or [ListenAndServe] (production,
// resolves `--bind`).
type Server struct {
	addrResolved string // for Logger; set by ListenAndServe
	token        string
	logger       *slog.Logger
	mgr          *runmgr.Manager // may be nil; run handlers return 503 then
	http         *http.Server
}

// New builds a Server config. The token must be non-empty; passing
// "" is a programming error and surfaces an error from Serve. The
// addr is remembered for [ListenAndServe] but not used by [Serve]
// (tests supply their own listener).
//
// mgr is the run scheduler. A nil mgr is permitted; the run
// handlers (POST /v1/runs, /v1/runs/{id}, /v1/runs/{id}/events,
// /v1/runs/{id}/result, /v1/runs/{id}/stream) return 503 in that
// case. Healthz and version endpoints do not need a manager.
//
// A nil logger falls back to [slog.Default].
func New(addr, token string, logger *slog.Logger, mgr *runmgr.Manager) (*Server, error) {
	if token == "" {
		return nil, errors.New("httpserver: empty bearer token")
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{addrResolved: addr, token: token, logger: logger, mgr: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/version", s.handleVersion)
	mux.HandleFunc("/v1/runs", s.handleRunsListOrStart) // method-dispatch
	mux.HandleFunc("/v1/runs/", s.handleRunSubrouter)
	mux.HandleFunc("/v1/audits/pending", s.handleAuditsPending)
	mux.HandleFunc("/v1/agents", s.handleAgentsListOrCreate) // method-dispatch
	mux.HandleFunc("/v1/agents/", s.handleAgentSubrouter)

	s.http = &http.Server{
		Handler:      s.authMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // SSE streams are long-lived; per-request write deadlines are enforced inside handlers when added.
		IdleTimeout:  120 * time.Second,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	return s, nil
}

// Serve runs the server on the supplied listener until ctx is
// cancelled or the underlying [http.Server] returns a non-ErrServerClosed
// error. Shutdown drains in-flight requests for up to `graceful`.
func (s *Server) Serve(ctx context.Context, ln net.Listener, graceful time.Duration) error {
	if graceful <= 0 {
		graceful = 5 * time.Second
	}
	s.addrResolved = ln.Addr().String()

	serveErr := make(chan error, 1)
	go func() {
		s.logger.Info("httpserver: serving",
			"addr", s.addrResolved,
			"version", Version,
		)
		err := s.http.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			serveErr <- nil
			return
		}
		serveErr <- err
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), graceful)
		defer cancel()
		if dl, ok := shutdownCtx.Deadline(); ok {
			s.logger.Info("httpserver: shutdown initiated", "deadline", dl.Format(time.RFC3339))
		} else {
			s.logger.Info("httpserver: shutdown initiated")
		}
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			s.logger.Warn("httpserver: graceful shutdown failed", "err", err)
			return err
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// ListenAndServe resolves the configured addr and serves. addr
// supports:
//
//   - "host:port"        → net.Listen("tcp", addr)
//   - "unix:///abs/path" → net.Listen("unix", "/abs/path"),
//     unlinking any stale socket at that path.
func (s *Server) ListenAndServe(ctx context.Context, graceful time.Duration) error {
	dialer, err := resolveAddr(s.addrResolved)
	if err != nil {
		return fmt.Errorf("httpserver: listen on %s: %w", s.addrResolved, err)
	}
	return s.Serve(ctx, dialer, graceful)
}

func resolveAddr(addr string) (net.Listener, error) {
	if len(addr) > 7 && addr[:7] == "unix://" {
		path := addr[7:]
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stale socket %s: %w", path, err)
		}
		return net.Listen("unix", path)
	}
	return net.Listen("tcp", addr)
}

// --- handlers --------------------------------------------------------------

// handleHealthz returns 200 with a small JSON body. Public — intended
// for load balancers / orchestrators that need a liveness probe
// without carrying a bearer token.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": Version,
	})
}

// authMiddleware rejects every request that doesn't carry a matching
// `Authorization: Bearer <token>` header. Constant-time compare
// defeats trivial timing attacks; V2 has no scope-based auth, so a
// single shared token is the entire model (see ADR-0010 §3).
//
// A small allowlist (`/v1/healthz`, `/v1/version`) bypasses auth so
// liveness / build-info probes do not have to carry a token. The
// allowlist is exact-match only — sub-paths stay authenticated.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/healthz", "/v1/version":
			next.ServeHTTP(w, r)
			return
		}
		got := bearerFrom(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="conductor"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerFrom parses "Bearer <token>" into <token>. Anything else
// (wrong scheme, no header, case-mismatch) returns "".
func bearerFrom(h string) string {
	const prefix = "Bearer "
	if len(h) < len(prefix) || h[:len(prefix)] != prefix {
		return ""
	}
	return h[len(prefix):]
}
