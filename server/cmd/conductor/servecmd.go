package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"conductor/server/internal/agentregistry"
	"conductor/server/internal/daemonlock"
	"conductor/server/internal/httpserver"
	"conductor/server/internal/runmgr"
	"conductor/server/internal/servertoken"

	"github.com/spf13/cobra"
)

// Default bind address for `conductor serve`. Per ADR-0010 §2.
// Override via --bind (host:port or unix:///path/to/socket).
const defaultServeBind = "127.0.0.1:7411"

// serveCmd builds the `conductor serve` cobra subcommand. The
// tree wires `--bind`, `--allow-public-bind`, `--token-out`,
// `--registry-cwd`, and `--print-token`.
//
// Behaviour:
//
//  1. The registry lock (`flock(2)` on conductor.lock) is taken
//     on the registry directory; a second `conductor serve`
//     against the same directory exits 2 (per ADR-0010 §6).
//  2. The bearer token is loaded or generated at --token-out
//     (default `~/.config/conductor/serve.token`); env
//     CONDUCTOR_TOKEN takes precedence (per ADR-0010 §3).
//  3. The HTTP listener binds and serves; SIGTERM / SIGINT
//     triggers graceful shutdown (up to 5s of in-flight drain).
//  4. On exit, the registry lock is released and the token
//     file is left in place (operator-owned; we do not delete).
//
// Note: business routes (POST /v1/runs, SSE, audit endpoints,
// agent CRUD via HTTP) land in step 1+ PRs per ADR-0010 §9.
func serveCmd() *cobra.Command {
	var (
		bind         string
		allowPublic  bool
		tokenOutPath string
		registryCwd  string
		printToken   bool
	)
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the V2 daemon (HTTP transport)",
		Long: `Start the conductor daemon. The daemon exposes the conductor
agent-layer operations over a small REST/JSON surface rooted at
/v1/, authenticated by a single shared bearer token (ADR-0010).

Only one daemon can hold the registry at a time; a second start
against the same registry exits 2 with a clear error message.
Business routes (POST /v1/runs, audit endpoints, agent CRUD via
HTTP, SSE) land in subsequent step PRs per ADR-0010 §9.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd, runServeDeps{
				bind:         bind,
				allowPublic:  allowPublic,
				tokenOutPath: tokenOutPath,
				registryCwd:  registryCwd,
				printToken:   printToken,
			})
		},
	}
	c.Flags().StringVar(&bind, "bind", defaultServeBind,
		"address to bind (host:port or unix:///path); default 127.0.0.1:7411")
	c.Flags().BoolVar(&allowPublic, "allow-public-bind", false,
		"required to bind a non-loopback address; emits a one-line warning")
	c.Flags().StringVar(&tokenOutPath, "token-out", "",
		"path to the bearer token file; default $HOME/.config/conductor/serve.token. Override via CONDUCTOR_TOKEN env.")
	c.Flags().StringVar(&registryCwd, "registry-cwd", "",
		"directory holding .conductor/ for the registry lock; default $PWD")
	c.Flags().BoolVar(&printToken, "print-token", false,
		"print the bearer token to stderr once at startup (convenience for local dev)")
	return c
}

// runServeDeps captures all flag inputs to runServe so the
// function is testable without spawning a cobra tree.
type runServeDeps struct {
	bind         string
	allowPublic  bool
	tokenOutPath string
	registryCwd  string
	printToken   bool
}

// runServe is the imperative driver behind serveCmd. It is kept
// separable so tests can drive it directly.
func runServe(cmd *cobra.Command, deps runServeDeps) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := verifyBindPolicy(deps.bind, deps.allowPublic, logger); err != nil {
		return err
	}

	// Acquire the registry lock.
	registryDir, err := resolveRegistryDir(deps.registryCwd)
	if err != nil {
		return err
	}
	lock, err := daemonlock.Acquire(context.Background(), registryDir)
	if err != nil {
		if errors.Is(err, daemonlock.ErrAlreadyHeld) {
			// Sentinel mapping → exit code 2 (caller's main()).
			return err
		}
		return err
	}
	defer lock.Release()

	// Resolve token path.
	tokenPath, err := resolveTokenPath(deps.tokenOutPath)
	if err != nil {
		return err
	}

	tok, err := servertoken.LoadOrGenerate(tokenPath, servertoken.DefaultEnvVar)
	if err != nil {
		return err
	}
	if deps.printToken {
		fmt.Fprintf(os.Stderr, "conductor: bearer token = %s (path = %s)\n", tok, tokenPath)
	}
	logger.Info("conductor: token materialised", "path", tokenPath, "via", tokenSource(tokenPath))

	// Build the HTTP server.
	// Open the registry in the same dir the lock protects; the
	// manager and the lock point at the same store.
	reg, err := agentregistry.Open(registryDir)
	if err != nil {
		return fmt.Errorf("conductor serve: open registry: %w", err)
	}
	defer reg.Close()
	mgr := runmgr.New(reg, logger)

	srv, err := httpserver.New(deps.bind, tok, logger, mgr)
	if err != nil {
		return err
	}

	// Signal-aware context for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger.Info("conductor: serving",
		"bind", deps.bind,
		"registry_dir", registryDir,
		"version", httpserver.Version,
	)
	err = srv.ListenAndServe(ctx, 5*time.Second)
	if err != nil {
		logger.Error("conductor: serve exited with error", "err", err)
	}
	logger.Info("conductor: stopped")
	return err
}

// verifyBindPolicy implements ADR-0010 §2: refuse to bind
// non-loopback addresses without --allow-public-bind. Returns
// nil on the happy path.
func verifyBindPolicy(bind string, allowPublic bool, logger *slog.Logger) error {
	if isLoopbackAddr(bind) || allowPublic {
		return nil
	}
	host, _, _ := net.SplitHostPort(bind)
	if host == "" {
		// bare port like ":7411" — treat as 0.0.0.0
		host = "0.0.0.0"
	}
	if host == "0.0.0.0" || host == "::" {
		fmt.Fprintf(os.Stderr,
			"conductor: refusing to bind %s without --allow-public-bind (ADR-0010 §2)\n",
			bind)
		return fmt.Errorf("refusing non-loopback bind %s without --allow-public-bind", bind)
	}
	// Anything with explicit host (not 0.0.0.0 / ::) still gets
	// caught unless --allow-public-bind is set on a non-loopback
	// hostname/IP.
	hostIP := net.ParseIP(host)
	if hostIP != nil && hostIP.IsLoopback() {
		return nil
	}
	fmt.Fprintf(os.Stderr,
		"conductor: refusing non-loopback bind %s without --allow-public-bind (ADR-0010 §2)\n",
		bind)
	return fmt.Errorf("refusing non-loopback bind %s without --allow-public-bind", bind)
}

// isLoopbackAddr reports whether bind is loopback. Handles
// "host:port" and "unix://". Bare ":7411" → non-loopback (reject
// without --allow-public-bind, see verifyBindPolicy).
func isLoopbackAddr(bind string) bool {
	if len(bind) > 7 && bind[:7] == "unix://" {
		return true
	}
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return false
	}
	switch host {
	case "", "0.0.0.0", "::":
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// resolveRegistryDir computes the directory that holds the
// registry SQLite + the new conductor.lock sidecar. It is the
// directory passed to [daemonlock.Acquire].
//
//   - If cwd is empty, use the process working directory.
//   - The .conductor subdir is appended (it is created if absent;
//     daemonlock.Acquire handles MkdirAll).
func resolveRegistryDir(cwd string) (string, error) {
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("conductor serve: getwd: %w", err)
		}
		cwd = wd
	}
	return filepath.Join(cwd, ".conductor"), nil
}

// resolveTokenPath picks the on-disk token path. If the user
// passed an explicit --token-out, expand a leading "~/" to
// $HOME; otherwise use the project default
// "$HOME/.config/conductor/serve.token" (ADR-0010 §3 + Update
// log (a)).
func resolveTokenPath(flagValue string) (string, error) {
	if flagValue == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("conductor serve: no $HOME and no --token-out: %w", err)
		}
		return filepath.Join(home, servertoken.DefaultRelativePath), nil
	}
	return expandHome(flagValue), nil
}

// expandHome replaces a leading "~" or "~/" with $HOME. Anything
// else is returned verbatim.
func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if len(p) >= 2 && p[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// tokenSource is a tiny diagnostic helper for startup logs.
//   - "env" if CONDUCTOR_TOKEN is set (no file path is meaningful).
//   - "file" if the path existed already.
//   - "generated" if we just wrote a fresh one.
func tokenSource(path string) string {
	if os.Getenv(servertoken.DefaultEnvVar) != "" {
		return "env"
	}
	if _, err := os.Stat(path); err == nil {
		return "file"
	}
	return "generated"
}
