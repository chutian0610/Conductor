package httpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"conductor/server/internal/httpserver"
)

// listenAndServe spins up a Server bound to a free port via a
// pre-bound listener (cleanest sync between "bound" and
// "Addr() known"). The test goroutine cancels the ctx on
// t.Cleanup, triggering graceful shutdown.
func listenAndServe(t *testing.T, token string) (addr string, cancel func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr = ln.Addr().String()
	s, err := httpserver.New("placeholder", token, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("New: %v", err)
	}
	ctx, c := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Serve(ctx, ln, 100*time.Millisecond)
	}()
	cancel = func() {
		c()
		<-done
		_ = ln.Close()
	}
	t.Cleanup(cancel)
	// Tiny wait for the goroutine to enter Serve.
	time.Sleep(5 * time.Millisecond)
	return addr, cancel
}

func TestHealthzNoAuth(t *testing.T) {
	addr, _ := listenAndServe(t, "secret-token")

	resp, err := http.Get("http://" + addr + "/v1/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if h := resp.Header.Get("Content-Type"); !strings.HasPrefix(h, "application/json") {
		t.Errorf("Content-Type = %q, want application/json*", h)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want ok", body["status"])
	}
	if body["version"] != httpserver.Version {
		t.Errorf("version field = %q, want %q", body["version"], httpserver.Version)
	}
}

func TestHealthzRejectsNonGET(t *testing.T) {
	addr, _ := listenAndServe(t, "secret-token")

	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if h := resp.Header.Get("Allow"); h != http.MethodGet {
		t.Errorf("Allow = %q, want GET", h)
	}
}

func TestProtectedEndpointWithoutAuthIs401(t *testing.T) {
	addr, _ := listenAndServe(t, "secret-token")

	resp, err := http.Get("http://" + addr + "/v1/agents")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if h := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(h, "Bearer ") {
		t.Errorf("WWW-Authenticate = %q, want Bearer prefix", h)
	}
}

func TestProtectedEndpointWithWrongTokenIs401(t *testing.T) {
	addr, _ := listenAndServe(t, "secret-token")

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestProtectedEndpointWithRightTokenIsPassThrough(t *testing.T) {
	addr, _ := listenAndServe(t, "secret-token")

	// /v1/unknown is intentionally not routed; the auth middleware
	// accepts the bearer, and the mux returns 404 because no
	// handler matches. We use a path that has no agent/runs/etc.
	// routing.
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/unknown", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (auth passed, mux not routed)", resp.StatusCode)
	}
}

func TestNewRejectsEmptyToken(t *testing.T) {
	if _, err := httpserver.New("127.0.0.1:0", "", nil, nil); err == nil {
		t.Fatal("expected error on empty token")
	}
}
