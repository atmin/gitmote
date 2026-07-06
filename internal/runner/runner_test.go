package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSecret = "worker-secret"

var testSpec = spec{
	JobID: 5, RunID: 1, Repo: "atmin/app", SHA: "deadbeef",
	Ref: "refs/heads/main", WorkflowDir: ".gitmote/workflows",
}

// capture records the completion the runner posts.
type capture struct {
	calls  int
	status string
	body   string
}

// newTestServer serves the two report endpoints. claimStatus controls the claim
// response (200 returns testSpec; anything else is returned verbatim, e.g. 404).
func newTestServer(t *testing.T, claimStatus int) (*httptest.Server, *capture) {
	t.Helper()
	cap := &capture{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/ci/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Worker-Secret") != testSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if claimStatus != http.StatusOK {
			w.WriteHeader(claimStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(testSpec)
	})
	mux.HandleFunc("POST /internal/ci/jobs/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Worker-Secret") != testSecret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		cap.calls++
		cap.status = r.URL.Query().Get("status")
		b, _ := io.ReadAll(r.Body)
		cap.body = string(b)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, cap
}

type fakeCloner struct {
	err                   error
	called                bool
	repo, sha, ref, token string
}

func (c *fakeCloner) Clone(_ context.Context, _, repo, sha, ref, token, _ string) error {
	c.called = true
	c.repo, c.sha, c.ref, c.token = repo, sha, ref, token
	return c.err
}

type fakeEngine struct {
	log    []byte
	passed bool
	err    error
	called bool
}

func (e *fakeEngine) Run(_ context.Context, _, _ string) ([]byte, bool, error) {
	e.called = true
	return e.log, e.passed, e.err
}

func baseConfig(t *testing.T, srvURL string, c Cloner, e Engine) Config {
	return Config{
		BaseURL: srvURL, JobID: testSpec.JobID, WorkerSecret: testSecret,
		CloneToken: "clone-tok", Cloner: c, Engine: e, WorkDir: t.TempDir(),
	}
}

func TestRunPassed(t *testing.T) {
	srv, cap := newTestServer(t, http.StatusOK)
	cl := &fakeCloner{}
	en := &fakeEngine{log: []byte("all green\n"), passed: true}

	if err := Run(context.Background(), baseConfig(t, srv.URL, cl, en)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Cloned the claimed SHA/ref with the injected token.
	if !cl.called || cl.repo != testSpec.Repo || cl.sha != testSpec.SHA ||
		cl.ref != testSpec.Ref || cl.token != "clone-tok" {
		t.Errorf("clone = %+v, want repo/sha/ref from the spec and the injected token", cl)
	}
	if cap.calls != 1 || cap.status != "passed" || cap.body != "all green\n" {
		t.Errorf("completion = %+v, want one passed with the engine log", cap)
	}
}

func TestRunFailed(t *testing.T) {
	srv, cap := newTestServer(t, http.StatusOK)
	en := &fakeEngine{log: []byte("boom\n"), passed: false}

	if err := Run(context.Background(), baseConfig(t, srv.URL, &fakeCloner{}, en)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cap.status != "failed" || cap.body != "boom\n" {
		t.Errorf("completion = %+v, want failed with the engine log", cap)
	}
}

func TestRunEngineErrorReportsError(t *testing.T) {
	srv, cap := newTestServer(t, http.StatusOK)
	en := &fakeEngine{err: errStub("act missing")}

	if err := Run(context.Background(), baseConfig(t, srv.URL, &fakeCloner{}, en)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cap.status != "error" || !strings.Contains(cap.body, "act missing") {
		t.Errorf("completion = %+v, want status=error mentioning the engine error", cap)
	}
}

func TestRunCloneErrorReportsError(t *testing.T) {
	srv, cap := newTestServer(t, http.StatusOK)
	cl := &fakeCloner{err: errStub("auth denied")}
	en := &fakeEngine{}

	if err := Run(context.Background(), baseConfig(t, srv.URL, cl, en)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if en.called {
		t.Error("engine ran despite a clone failure")
	}
	if cap.status != "error" || !strings.Contains(cap.body, "clone failed") {
		t.Errorf("completion = %+v, want status=error mentioning the clone failure", cap)
	}
}

func TestRunNotClaimableIsNoOp(t *testing.T) {
	srv, cap := newTestServer(t, http.StatusNotFound)
	cl := &fakeCloner{}

	if err := Run(context.Background(), baseConfig(t, srv.URL, cl, &fakeEngine{})); err != nil {
		t.Fatalf("Run on an unclaimable job = %v, want nil", err)
	}
	if cl.called {
		t.Error("cloned an unclaimable job")
	}
	if cap.calls != 0 {
		t.Errorf("completion calls = %d, want 0 (nothing to report)", cap.calls)
	}
}

func TestRunClaimUnauthorizedFails(t *testing.T) {
	srv, cap := newTestServer(t, http.StatusOK)
	cfg := baseConfig(t, srv.URL, &fakeCloner{}, &fakeEngine{})
	cfg.WorkerSecret = "wrong" // server rejects the claim

	if err := Run(context.Background(), cfg); err == nil {
		t.Error("Run with a bad secret = nil, want an error (claim rejected)")
	}
	if cap.calls != 0 {
		t.Errorf("completion calls = %d, want 0", cap.calls)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
