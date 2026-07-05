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

	"github.com/atmin/s3lite"
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

func TestReplicaTarget(t *testing.T) {
	// A bucket alone derives s3://{bucket}/meta; the object prefix must NOT leak
	// into the replica path (that would orphan the existing sibling replica). An
	// explicit GITMOTE_DB_REPLICA overrides the derivation.
	tests := []struct {
		name    string
		replica string
		bucket  string
		prefix  string
		want    string
	}{
		{"derived from bucket", "", "gitmote", "", "s3://gitmote/meta"},
		{"prefix does not leak", "", "gitmote", "objects/", "s3://gitmote/meta"},
		{"explicit replica overrides", "s3://other/wal", "gitmote", "objects/", "s3://other/wal"},
		{"no bucket, no replica", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITMOTE_DB_REPLICA", tt.replica)
			t.Setenv("GITMOTE_S3_BUCKET", tt.bucket)
			t.Setenv("GITMOTE_S3_PREFIX", tt.prefix)
			if got := replicaTarget(); got != tt.want {
				t.Errorf("replicaTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMetaConfigDerivesReplicaAndRole(t *testing.T) {
	// With a bucket set, metaConfigFromEnv derives the replica and applies the
	// leased role — a bucket always means durability + the single-writer lease.
	t.Setenv("GITMOTE_DB_REPLICA", "")
	t.Setenv("GITMOTE_S3_BUCKET", "gitmote")
	t.Setenv("GITMOTE_S3_PREFIX", "objects/")
	t.Setenv("GITMOTE_DATA", t.TempDir())

	cfg := metaConfigFromEnv(nil, s3lite.RoleAuto)
	if cfg.RestoreFrom != "s3://gitmote/meta" || cfg.BackupTo != "s3://gitmote/meta" {
		t.Errorf("replica = %q / %q, want s3://gitmote/meta (prefix must not leak)", cfg.RestoreFrom, cfg.BackupTo)
	}
	if cfg.Role != s3lite.RoleAuto {
		t.Errorf("Role = %v, want RoleAuto (a bucket must yield the lease)", cfg.Role)
	}
}

func TestMetaConfigNoBucketStaysRoleOff(t *testing.T) {
	// No bucket and no replica: nothing to coordinate on, so the database stays
	// local-only and RoleOff (always writer) — tests and ephemeral runs unchanged.
	t.Setenv("GITMOTE_DB_REPLICA", "")
	t.Setenv("GITMOTE_S3_BUCKET", "")

	cfg := metaConfigFromEnv(nil, s3lite.RoleAuto)
	if cfg.RestoreFrom != "" || cfg.BackupTo != "" {
		t.Errorf("replica = %q / %q, want empty (no bucket)", cfg.RestoreFrom, cfg.BackupTo)
	}
	if cfg.Role != s3lite.RoleOff {
		t.Errorf("Role = %v, want RoleOff", cfg.Role)
	}
}

func TestDataPathsUnderGitmoteData(t *testing.T) {
	// GITMOTE_DATA alone places the db, cache, and socket under it.
	dir := t.TempDir()
	t.Setenv("GITMOTE_DATA", dir)
	t.Setenv("GITMOTE_DB", "")
	t.Setenv("GITMOTE_CACHE", "")
	t.Setenv("GITMOTE_SOCK", "")

	if got, want := dbPath(), filepath.Join(dir, "meta.sqlite3"); got != want {
		t.Errorf("dbPath() = %q, want %q", got, want)
	}
	if got, want := cachePath(), filepath.Join(dir, "cache"); got != want {
		t.Errorf("cachePath() = %q, want %q", got, want)
	}
	if got, want := sockPath(), filepath.Join(dir, "gitmote.sock"); got != want {
		t.Errorf("sockPath() = %q, want %q", got, want)
	}
}

func TestDataPathOverride(t *testing.T) {
	// The explicit per-path vars still override the GITMOTE_DATA derivation.
	t.Setenv("GITMOTE_DATA", "/data")
	t.Setenv("GITMOTE_DB", "/custom/db.sqlite3")
	if got, want := dbPath(), "/custom/db.sqlite3"; got != want {
		t.Errorf("dbPath() = %q, want the GITMOTE_DB override %q", got, want)
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
