package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/atmin/gitmote/internal/auth"
	"github.com/atmin/gitmote/internal/ci"
	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/repo"
	"github.com/atmin/gitmote/internal/store"
	"github.com/atmin/gitmote/internal/webui"
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

// TestUIRoutesDoNotShadowGitEndpoints locks the flat-namespace mux seam: the
// browse verbs (/{repo}/tree/…) share the mux with git's own /{repo}/info/refs.
// Only enumerated verbs are registered, so git's smart-HTTP suffixes must still
// fall through to the catch-all while browse verbs and the bare landing reach
// the UI (docs/architecture/urls.md → Implementation seams).
func TestUIRoutesDoNotShadowGitEndpoints(t *testing.T) {
	ctx := context.Background()
	m, err := meta.Open(ctx, meta.Config{LocalPath: filepath.Join(t.TempDir(), "meta.sqlite3")})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	mz := repo.New(m, store.NewMem(), t.TempDir())
	ui, err := webui.New(m, mz, store.NewMem(), nil, auth.NewGuard(m), nil, []byte("k"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}

	gitHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(newHandler(gitHandler, ui, nil, nil))
	defer srv.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	get := func(p string) int {
		resp, err := client.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Git's segment-2 suffixes fall through to the git handler.
	for _, p := range []string{"/some-repo/info/refs?service=git-upload-pack", "/some-repo/HEAD"} {
		if code := get(p); code != http.StatusTeapot {
			t.Errorf("GET %s = %d, want 418 (git catch-all)", p, code)
		}
	}
	// Browse verbs and the bare landing reach the UI (unknown repo, anonymous →
	// login redirect), never the git handler.
	for _, p := range []string{"/some-repo", "/some-repo/tree/main"} {
		if code := get(p); code == http.StatusTeapot {
			t.Errorf("GET %s hit the git handler; want the UI", p)
		}
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
	t.Setenv("GITMOTE_DATA", t.TempDir())

	var out bytes.Buffer
	args := []string{"-handle", "atmin", "-repo", "gitmote"}
	if err := runBootstrap(context.Background(), args, &out); err != nil {
		t.Fatalf("runBootstrap: %v", err)
	}
	if !strings.Contains(out.String(), "gitmote") || !strings.Contains(out.String(), "access token") {
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

func TestRunBootstrapDefaultsHandleAndReissues(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("GITMOTE_DATA", dataDir)
	dbPath := filepath.Join(dataDir, "meta.sqlite3")

	// No handle, no repo: the admin defaults to "admin" and a token is printed.
	var out bytes.Buffer
	if err := runBootstrap(context.Background(), nil, &out); err != nil {
		t.Fatalf("runBootstrap: %v", err)
	}
	if !strings.Contains(out.String(), "admin user:   admin") || !strings.Contains(out.String(), "access token") {
		t.Errorf("bootstrap output missing default admin/token:\n%s", out.String())
	}
	first := extractToken(t, out.String())

	// Only the hash is at rest: the token's secret half never lands in the DB
	// (runBootstrap's Close checkpointed the WAL into the file above).
	_, secret, ok := strings.Cut(first, ".")
	if !ok || secret == "" {
		t.Fatalf("token %q has no secret half", first)
	}
	db, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if bytes.Contains(db, []byte(secret)) {
		t.Error("the raw token secret is stored in the metadata DB; only its hash should be")
	}

	// -reissue mints a fresh token for the existing admin.
	var out2 bytes.Buffer
	if err := runBootstrap(context.Background(), []string{"-reissue"}, &out2); err != nil {
		t.Fatalf("reissue runBootstrap: %v", err)
	}
	second := extractToken(t, out2.String())
	if second == first {
		t.Errorf("reissue returned the same token %q, want a fresh one", second)
	}
}

func TestMaybeAutoBootstrapCreatesAdminOnceOnFreshLeader(t *testing.T) {
	ctx := context.Background()
	md := openMeta(t) // local-only → sole writer → always leader
	logger := slog.New(slog.DiscardHandler)

	// Fresh instance: the default admin is created, no repo.
	maybeAutoBootstrap(ctx, md, logger)
	admin, err := md.GetUser(ctx, "admin")
	if err != nil {
		t.Fatalf("default admin not created: %v", err)
	}
	if !admin.IsAdmin {
		t.Error("bootstrapped user is not a global admin")
	}
	tokens, err := md.ListTokens(ctx, admin.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("admin has %d tokens, want 1", len(tokens))
	}

	// Idempotent: a second boot (admin exists) mints nothing more.
	maybeAutoBootstrap(ctx, md, logger)
	tokens2, _ := md.ListTokens(ctx, admin.ID)
	if len(tokens2) != 1 {
		t.Errorf("second auto-bootstrap changed token count to %d, want 1 (no-op)", len(tokens2))
	}
	users, _ := md.ListUsers(ctx)
	if len(users) != 1 {
		t.Errorf("user count = %d, want 1 (no second admin)", len(users))
	}
}

// extractToken pulls the gmt_ token out of a bootstrap banner.
func extractToken(t *testing.T, s string) string {
	t.Helper()
	tok := regexp.MustCompile(`gmt_[0-9a-f]+\.[0-9a-f]+`).FindString(s)
	if tok == "" {
		t.Fatalf("no token in output:\n%s", s)
	}
	return tok
}

func TestReplicaTarget(t *testing.T) {
	// The replica lives at {root}/meta, under the same storage root as the objects
	// — so a "bucket/base" spec scopes the replica too (they share fate).
	tests := []struct {
		name   string
		bucket string
		want   string
	}{
		{"bucket only", "gitmote", "s3://gitmote/meta"},
		{"bucket with base", "shared/gitmote", "s3://shared/gitmote/meta"},
		{"nested base", "shared/a/b", "s3://shared/a/b/meta"},
		{"no bucket", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITMOTE_S3_BUCKET", tt.bucket)
			if got := replicaTarget(); got != tt.want {
				t.Errorf("replicaTarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMetaConfigDerivesReplicaAndRole(t *testing.T) {
	// With a storage root set, metaConfigFromEnv derives the replica under it and
	// applies the leased role — a root always means durability + the single-writer
	// lease, and the base scopes the replica.
	t.Setenv("GITMOTE_S3_BUCKET", "shared/gitmote")
	t.Setenv("GITMOTE_DATA", t.TempDir())

	cfg := metaConfigFromEnv(nil, s3lite.RoleAuto)
	if cfg.RestoreFrom != "s3://shared/gitmote/meta" || cfg.BackupTo != "s3://shared/gitmote/meta" {
		t.Errorf("replica = %q / %q, want s3://shared/gitmote/meta (base scopes the replica)", cfg.RestoreFrom, cfg.BackupTo)
	}
	if cfg.Role != s3lite.RoleAuto {
		t.Errorf("Role = %v, want RoleAuto (a root must yield the lease)", cfg.Role)
	}
}

func TestMetaConfigNoBucketLeavesRoleUnset(t *testing.T) {
	// No bucket: nothing to coordinate on, so the role is left unset (the zero
	// value, RoleAuto) even for a coordinating role like RoleWriter — s3lite then
	// runs the DB local-only as the sole writer, not demanding a lease it can't get.
	t.Setenv("GITMOTE_S3_BUCKET", "")

	cfg := metaConfigFromEnv(nil, s3lite.RoleWriter)
	if cfg.RestoreFrom != "" || cfg.BackupTo != "" {
		t.Errorf("replica = %q / %q, want empty (no bucket)", cfg.RestoreFrom, cfg.BackupTo)
	}
	if cfg.Role != s3lite.RoleAuto {
		t.Errorf("Role = %v, want RoleAuto (unset zero value — role not applied without a replica)", cfg.Role)
	}
}

func TestDataPathsUnderGitmoteData(t *testing.T) {
	// GITMOTE_DATA places the db, cache, and socket under it.
	dir := t.TempDir()
	t.Setenv("GITMOTE_DATA", dir)

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

// openMeta opens a fresh, local-only metadata DB at a temp path.
func openMeta(t *testing.T) *meta.Metadata {
	t.Helper()
	md, err := meta.Open(context.Background(), meta.Config{
		LocalPath: filepath.Join(t.TempDir(), "meta.sqlite3"),
	})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = md.Close() })
	return md
}

func TestResolveWorkerSecretGeneratesPersistsAndOverrides(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WORKER_SECRET", "")
	md := openMeta(t)

	// No env: generated, and a hex string of the expected width.
	s1, err := resolveWorkerSecret(ctx, md)
	if err != nil {
		t.Fatalf("resolveWorkerSecret: %v", err)
	}
	if len(s1) != hex.EncodedLen(genSecretBytes) {
		t.Errorf("generated secret len = %d, want %d hex chars", len(s1), hex.EncodedLen(genSecretBytes))
	}
	// Stable across calls (persisted, not regenerated).
	if s2, _ := resolveWorkerSecret(ctx, md); s2 != s1 {
		t.Errorf("second resolve = %q, want the persisted %q", s2, s1)
	}

	// An explicit env wins over the persisted value.
	t.Setenv("WORKER_SECRET", "explicit-secret")
	if s3, _ := resolveWorkerSecret(ctx, md); s3 != "explicit-secret" {
		t.Errorf("resolve with env = %q, want the env value", s3)
	}
}

func TestResolveCookieKeyGeneratesPersistsAndOverrides(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GITMOTE_COOKIE_KEY", "")
	md := openMeta(t)

	k1, err := resolveCookieKey(ctx, md)
	if err != nil {
		t.Fatalf("resolveCookieKey: %v", err)
	}
	if len(k1) == 0 {
		t.Fatal("generated cookie key is empty")
	}
	// A restored session key must be stable, so a signed cookie survives a restart.
	if k2, _ := resolveCookieKey(ctx, md); string(k2) != string(k1) {
		t.Errorf("second resolve = %q, want the persisted %q", k2, k1)
	}

	t.Setenv("GITMOTE_COOKIE_KEY", "explicit-key")
	if k3, _ := resolveCookieKey(ctx, md); string(k3) != "explicit-key" {
		t.Errorf("resolve with env = %q, want the env value", k3)
	}
}

// captureTrigger records the env of each trigger call.
type captureTrigger struct{ calls []map[string]string }

func (c *captureTrigger) Trigger(_ context.Context, env map[string]string) error {
	c.calls = append(c.calls, env)
	return nil
}

func TestResolvedWorkerSecretReachesDispatcherAndReportAPI(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WORKER_SECRET", "")
	md := openMeta(t)

	secret, err := resolveWorkerSecret(ctx, md)
	if err != nil {
		t.Fatalf("resolveWorkerSecret: %v", err)
	}

	// The dispatcher injects the resolved secret into the runner env at trigger —
	// the cloud runner reads WORKER_SECRET from there, never a baked-in job def.
	s := store.NewMem()
	mz := repo.New(md, s, t.TempDir())
	r, head := seedWorkflowRepo(t, md, s)
	tr := &captureTrigger{}
	ci.NewDispatcher(ci.Config{
		Runs: md, Materializer: mz, Trigger: tr,
		BaseURL: "https://gitmote.test", WorkerSecret: secret,
	}).Dispatch(ctx, ci.Event{
		RepoID: r.ID, RepoName: r.Name, Ref: "refs/heads/main",
		OldSHA: meta.ZeroSHA, NewSHA: head,
	})
	if len(tr.calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1", len(tr.calls))
	}
	if got := tr.calls[0]["WORKER_SECRET"]; got != secret {
		t.Errorf("injected WORKER_SECRET = %q, want the resolved %q", got, secret)
	}

	// The report API validates that same secret — closing the loop the runner
	// rides: injected at trigger, checked on report-back.
	api := ci.NewReportAPI(md, s, nil, nil, secret, nil)
	mux := http.NewServeMux()
	api.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if code := claimStatus(t, srv.URL, "wrong-secret"); code != http.StatusUnauthorized {
		t.Errorf("claim with wrong secret = %d, want 401", code)
	}
	// The resolved secret authenticates: a nonexistent job id is a 404, not a 401.
	if code := claimStatus(t, srv.URL, secret); code == http.StatusUnauthorized {
		t.Error("claim with resolved secret = 401, want auth to pass")
	}
}

// claimStatus issues a job claim to the report API with the given worker secret
// and returns the HTTP status.
func claimStatus(t *testing.T, base, secret string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/internal/ci/jobs/1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Worker-Secret", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// seedWorkflowRepo builds a real repo with one workflow, loads its objects into
// the store the materializer reads, and records the repo + branch ref in meta.
func seedWorkflowRepo(t *testing.T, md *meta.Metadata, s store.Store) (*meta.Repo, string) {
	t.Helper()
	ctx := context.Background()
	const name = "app"

	src := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-b", "main", ".")
	wf := filepath.Join(src, ".gitmote", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf, "ci.yml"),
		[]byte("name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "seed")
	head := gitRevParse(t, src)

	objRoot := filepath.Join(src, ".git", "objects")
	if err := filepath.WalkDir(objRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(objRoot, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return s.Put(ctx, name+"/objects/"+filepath.ToSlash(rel), bytes.NewReader(data))
	}); err != nil {
		t.Fatalf("seed objects: %v", err)
	}

	r, err := md.CreateRepo(ctx, name, "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if err := md.CASRef(ctx, r.ID, "refs/heads/main", meta.ZeroSHA, head); err != nil {
		t.Fatalf("CASRef: %v", err)
	}
	return r, head
}

func gitRevParse(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
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
