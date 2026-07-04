package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(newHandler(nil, nil, nil, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestVersion(t *testing.T) {
	srv := httptest.NewServer(newHandler(nil, nil, nil, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /version status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestUnknownRouteNotFound(t *testing.T) {
	srv := httptest.NewServer(newHandler(nil, nil, nil, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestGitHandlerMountedAtRoot(t *testing.T) {
	// A provided git handler serves the catch-all "/" while the exact
	// health/version routes stay more specific and win.
	gitHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(newHandler(gitHandler, nil, nil, nil))
	defer srv.Close()

	health, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want %d", health.StatusCode, http.StatusOK)
	}

	gitResp, err := http.Get(srv.URL + "/some/repo/info/refs")
	if err != nil {
		t.Fatalf("GET /some/repo/info/refs: %v", err)
	}
	gitResp.Body.Close()
	if gitResp.StatusCode != http.StatusTeapot {
		t.Errorf("git route status = %d, want %d (routed to git handler)", gitResp.StatusCode, http.StatusTeapot)
	}
}

func TestLeaderGate(t *testing.T) {
	// A follower must not answer metadata-derived requests with a stale snapshot;
	// the git/read catch-all is 503'd. Health/version stay up so a rolling deploy
	// can still promote (gating them would deadlock it).
	gitHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	leader := false
	srv := httptest.NewServer(newHandler(gitHandler, nil, nil, func() bool { return leader }))
	defer srv.Close()

	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// As a follower: probes stay up, the git read path is refused.
	if code := get("/healthz"); code != http.StatusOK {
		t.Errorf("follower /healthz = %d, want 200 (must stay up for the deploy)", code)
	}
	if code := get("/version"); code != http.StatusOK {
		t.Errorf("follower /version = %d, want 200", code)
	}
	if code := get("/some/repo/info/refs"); code != http.StatusServiceUnavailable {
		t.Errorf("follower git read = %d, want 503", code)
	}

	// After promotion, the same route is served.
	leader = true
	if code := get("/some/repo/info/refs"); code != http.StatusTeapot {
		t.Errorf("leader git read = %d, want %d (served)", code, http.StatusTeapot)
	}
}

func TestRunShutsDownOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logger := slog.New(slog.DiscardHandler)

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, logger, "127.0.0.1:0")
	}()

	// Give the server a moment to start, then trigger shutdown.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not shut down after cancel")
	}
}

func TestRunBootstrap(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite3")
	t.Setenv("GITMOTE_DB", dbPath)

	var out bytes.Buffer
	args := []string{"-handle", "atmin", "-repo", "atmin/gitmote"}
	if err := runBootstrap(context.Background(), args, &out); err != nil {
		t.Fatalf("runBootstrap: %v", err)
	}
	if !strings.Contains(out.String(), "atmin/gitmote") || !strings.Contains(out.String(), "access token") {
		t.Errorf("bootstrap output missing repo/token:\n%s", out.String())
	}

	// Re-running against the same DB refuses to clobber the admin.
	var out2 bytes.Buffer
	if err := runBootstrap(context.Background(), args, &out2); err != nil {
		t.Fatalf("second runBootstrap: %v", err)
	}
	if !strings.Contains(out2.String(), "already bootstrapped") {
		t.Errorf("second run did not report already-bootstrapped:\n%s", out2.String())
	}
}

func TestRunFailsOnBadAddr(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)

	err := run(context.Background(), logger, "not-an-addr")
	if err == nil {
		t.Fatal("run returned nil, want listen error")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("run error = %q, want a listen error", err)
	}
}
