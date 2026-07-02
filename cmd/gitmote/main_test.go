package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(newHandler(nil))
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
	srv := httptest.NewServer(newHandler(nil))
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
	srv := httptest.NewServer(newHandler(nil))
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
	srv := httptest.NewServer(newHandler(gitHandler))
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
