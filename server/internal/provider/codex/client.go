package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
)

// Client is a single connection to a running `codex app-server`
// subprocess. Methods are goroutine-safe.
//
// Lifecycle:
//
//	c, err := NewClient(ctx, ClientConfig{...})
//	defer c.Close()
//	if err := c.Call(ctx, "thread/start", params, &result); err != nil { ... }
//	for ev := range c.Events() { ... }
//	c.Close()
//
// `Events()` returns a channel that closes when:
//   - the subprocess exits
//   - Close() is called
//   - ctx is canceled (and the subprocess has been signalled)
type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderr  *bufio.Reader
	events  chan map[string]any // raw JSON objects; caller maps to protocol events
	closeOnce sync.Once
	done      chan struct{}  // closed when cmd exits
	nextID    atomic.Int64
	pending   sync.Map       // id -> chan json.RawMessage (response waiter)
}

// ClientConfig holds the parameters for spawning a Codex app-server.
type ClientConfig struct {
	// Bin is the path to the `codex` binary. Empty defaults to
	// "codex" (resolved via $PATH).
	Bin string

	// Args are extra CLI args passed before "app-server". Phase 1
	// doesn't need any; reserved for Phase 2 flags like --profile.
	Args []string

	// Home is the per-spec HOME directory. Required: the Codex
	// app-server reads ~/.codex/config.toml from this HOME on startup.
	Home string

	// Cwd is the working directory the subprocess should chdir into.
	Cwd string

	// Env is appended to os.Environ() in the subprocess. Code must
	// include the provider's auth key here (e.g. OPENROUTER_API_KEY).
	Env []string
}

// NewClient spawns `codex app-server` and returns a connected Client.
// The subprocess is bound to ctx: cancelling ctx sends SIGTERM, then
// SIGKILL after a grace period (see §14 of docs/design.md).
//
// Returns an error if the binary can't be found or fails to start.
// The subprocess is killed on error to avoid leaking processes.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	bin := cfg.Bin
	if bin == "" {
		bin = "codex"
	}

	cmd := exec.CommandContext(ctx, bin, append(cfg.Args, "app-server")...)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	env := append([]string(nil), osEnviron()...)
	env = append(env, "HOME="+cfg.Home)
	env = append(env, cfg.Env...)
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start codex app-server: %w (binary: %s)", err, bin)
	}

	c := &Client{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		stderr: bufio.NewReader(stderr),
		events: make(chan map[string]any, 64),
		done:   make(chan struct{}),
	}

	go c.pumpStdout()
	go c.pumpStderr()

	return c, nil
}

// Events returns the notification channel. Closes when the
// subprocess exits or Close() is called.
func (c *Client) Events() <-chan map[string]any { return c.events }

// Done returns a channel closed when the subprocess has exited.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close sends SIGTERM, then SIGKILL after a 5s grace, and waits for
// the subprocess to exit. Idempotent.
func (c *Client) Close() error {
	var errVal error
	c.closeOnce.Do(func() {
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Signal(unixSIGTERM())
		}
		select {
		case <-c.done:
		case <-afterChan(5_000_000_000): // 5s
			if c.cmd != nil && c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-c.done
		}
		errVal = c.cmd.Wait()
		close(c.events)
	})
	return errVal
}

// Call sends a JSON-RPC request and blocks until the matching
// response arrives or ctx is canceled. The params / result types
// are caller-defined (must be JSON-marshalable).
//
// `result` is optional; pass nil if you don't care about the response
// body (only whether the call succeeded).
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	id := c.nextID.Add(1)

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}

	// Register the response waiter BEFORE writing the request,
	// so a fast server doesn't reply before we register.
	respCh := make(chan json.RawMessage, 1)
	c.pending.Store(id, respCh)
	defer c.pending.Delete(id)

	if err := c.writeJSON(req); err != nil {
		return fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case raw := <-respCh:
		if result == nil {
			return nil
		}
		return json.Unmarshal(raw, result)
	}
}

// writeJSON encodes v as a single JSON-RPC line and writes it to
// stdin followed by \n. JSON-RPC over stdio uses line-delimited JSON.
func (c *Client) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}
	return nil
}

// pumpStdout reads one JSON-RPC message per line and dispatches it:
// responses go to the waiter for their id; notifications go to
// the events channel. Exits when EOF (subprocess closed stdout).
func (c *Client) pumpStdout() {
	defer close(c.done)
	for {
		line, err := c.stdout.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// Best-effort: we don't have a logger here yet.
				_ = err
			}
			return
		}
		// Parse the outer envelope to decide response vs notification.
		var env struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			continue // malformed line, skip
		}
		if env.Method == "" {
			// Response: route to the waiter.
			if id, ok := parseID(env.ID); ok {
				if ch, found := c.pending.Load(id); found {
					respCh := ch.(chan json.RawMessage)
					// Prefer the result field; fall back to error
					// (so the caller can surface failures uniformly).
					body := env.Result
					if len(body) == 0 {
						body = env.Error
					}
					respCh <- body
				}
			}
			continue
		}
		// Notification: send the raw object to Events().
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err == nil {
			select {
			case c.events <- raw:
			case <-c.done:
				return
			}
		}
	}
}

// pumpStderr drains the subprocess's stderr so it doesn't block
// on a full pipe. Phase 1 doesn't surface stderr; Phase 2 will route
// it to a logger.
func (c *Client) pumpStderr() {
	_, _ = io.Copy(io.Discard, c.stderr)
}

// parseID converts a JSON id (number or string) into an int64.
// Returns ok=false if the id isn't a non-negative integer; callers
// should treat that as an unmatched response (probably from an old
// or unknown request).
func parseID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	// Try as integer first.
	if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil && n >= 0 {
		return n, true
	}
	// Codex uses integer ids, but be defensive about string ids.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}
