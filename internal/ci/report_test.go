package ci

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/store"
)

const workerSecret = "worker-secret"

// reportFixture wires a report API over a real meta + in-memory store, seeds a
// repo/run/job, and returns the API's test server, the store, and the ids.
type reportFixture struct {
	srv    *httptest.Server
	store  store.Store
	md     *meta.Metadata
	api    *ReportAPI
	repoID int64
	runID  int64
	jobID  int64
}

func newReportFixture(t *testing.T, isLeader func() bool) reportFixture {
	t.Helper()
	ctx := context.Background()
	md, err := meta.Open(ctx, meta.Config{LocalPath: filepath.Join(t.TempDir(), "meta.sqlite3")})
	if err != nil {
		t.Fatalf("meta.Open: %v", err)
	}
	t.Cleanup(func() { _ = md.Close() })
	s := store.NewMem()

	r, err := md.CreateRepo(ctx, "app", "main")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	run, err := md.CreateRun(ctx, r.ID, "refs/heads/main", "deadbeef")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	job, err := md.CreateJob(ctx, run.ID, "ci.yml")
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	api := NewReportAPI(md, s, nil, isLeader, workerSecret, nil)
	mux := http.NewServeMux()
	api.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return reportFixture{srv: srv, store: s, md: md, api: api, repoID: r.ID, runID: run.ID, jobID: job.ID}
}

// do issues an authenticated request unless secret is "" (then it sends none).
func (f reportFixture) do(t *testing.T, method, path, secret, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		req.Header.Set("X-Worker-Secret", secret)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestClaimTransitionsAndReturnsSpec(t *testing.T) {
	ctx := context.Background()
	f := newReportFixture(t, nil)

	resp := f.do(t, http.MethodGet, "/internal/ci/jobs/"+id(f.jobID), workerSecret, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim status = %d, want 200", resp.StatusCode)
	}
	var spec jobSpec
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if spec.JobID != f.jobID || spec.RunID != f.runID || spec.Repo != "app" ||
		spec.SHA != "deadbeef" || spec.Ref != "refs/heads/main" || spec.WorkflowDir != workflowDir {
		t.Errorf("spec = %+v, want the seeded job's coordinates", spec)
	}

	// The job is now running.
	job, _ := f.md.GetJob(ctx, f.jobID)
	if job.Status != meta.RunRunning {
		t.Errorf("job status = %q, want running", job.Status)
	}

	// A second claim of the now-running job is not claimable → 404.
	resp2 := f.do(t, http.MethodGet, "/internal/ci/jobs/"+id(f.jobID), workerSecret, "")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("re-claim status = %d, want 404", resp2.StatusCode)
	}
}

func TestCompleteStoresLogAndRollsUp(t *testing.T) {
	ctx := context.Background()
	f := newReportFixture(t, nil)
	// Claim first, then complete.
	f.do(t, http.MethodGet, "/internal/ci/jobs/"+id(f.jobID), workerSecret, "").Body.Close()

	resp := f.do(t, http.MethodPost, "/internal/ci/jobs/"+id(f.jobID)+"/complete?status=passed", workerSecret, "build output\n")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete status = %d, want 200", resp.StatusCode)
	}

	// Job is passed, log_key recorded, and the log blob is under the ci/ key.
	job, _ := f.md.GetJob(ctx, f.jobID)
	if job.Status != meta.RunPassed {
		t.Errorf("job status = %q, want passed", job.Status)
	}
	wantKey := "ci/" + id(f.repoID) + "/" + id(f.runID) + "/" + id(f.jobID) + ".log"
	if job.LogKey != wantKey {
		t.Errorf("log_key = %q, want %q", job.LogKey, wantKey)
	}
	rc, err := f.store.Get(ctx, wantKey)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", wantKey, err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "build output\n" {
		t.Errorf("stored log = %q, want %q", data, "build output\n")
	}

	// The single job passed, so the run rolls up to passed.
	run, _ := f.md.GetRun(ctx, f.runID)
	if run.Status != meta.RunPassed {
		t.Errorf("run status = %q, want passed", run.Status)
	}
}

func TestCompleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newReportFixture(t, nil)
	f.do(t, http.MethodGet, "/internal/ci/jobs/"+id(f.jobID), workerSecret, "").Body.Close()
	f.do(t, http.MethodPost, "/internal/ci/jobs/"+id(f.jobID)+"/complete?status=passed", workerSecret, "first\n").Body.Close()

	// A second complete with a different status/log is a no-op — the terminal
	// job keeps its first outcome and log.
	resp := f.do(t, http.MethodPost, "/internal/ci/jobs/"+id(f.jobID)+"/complete?status=failed", workerSecret, "second\n")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second complete status = %d, want 200", resp.StatusCode)
	}
	job, _ := f.md.GetJob(ctx, f.jobID)
	if job.Status != meta.RunPassed {
		t.Errorf("job status = %q, want passed (idempotent)", job.Status)
	}
	rc, _ := f.store.Get(ctx, job.LogKey)
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "first\n" {
		t.Errorf("log = %q, want the first log unchanged", data)
	}
}

func TestReportAPIAuth(t *testing.T) {
	f := newReportFixture(t, nil)
	path := "/internal/ci/jobs/" + id(f.jobID)

	// Missing secret → 401.
	resp := f.do(t, http.MethodGet, path, "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no secret: status = %d, want 401", resp.StatusCode)
	}
	// Wrong secret → 401.
	resp = f.do(t, http.MethodGet, path, "nope", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong secret: status = %d, want 401", resp.StatusCode)
	}
}

func TestReportAPIFollowerReturns503(t *testing.T) {
	f := newReportFixture(t, func() bool { return false }) // a follower
	resp := f.do(t, http.MethodGet, "/internal/ci/jobs/"+id(f.jobID), workerSecret, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("follower claim: status = %d, want 503", resp.StatusCode)
	}
}

func TestCompleteTruncatesOversizeLog(t *testing.T) {
	ctx := context.Background()
	f := newReportFixture(t, nil)
	f.do(t, http.MethodGet, "/internal/ci/jobs/"+id(f.jobID), workerSecret, "").Body.Close()

	big := strings.Repeat("x", logCap+1024) // over the cap
	resp := f.do(t, http.MethodPost, "/internal/ci/jobs/"+id(f.jobID)+"/complete?status=passed", workerSecret, big)
	resp.Body.Close()

	job, _ := f.md.GetJob(ctx, f.jobID)
	rc, err := f.store.Get(ctx, job.LogKey)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.HasSuffix(string(data), logTruncationMarker) {
		t.Error("oversize log missing the truncation marker (silent truncation)")
	}
	// Body cut to the cap, plus the marker — not the full oversize input.
	if len(data) != logCap+len(logTruncationMarker) {
		t.Errorf("stored len = %d, want cap+marker = %d", len(data), logCap+len(logTruncationMarker))
	}
}

func TestCompleteRejectsBadStatus(t *testing.T) {
	f := newReportFixture(t, nil)
	f.do(t, http.MethodGet, "/internal/ci/jobs/"+id(f.jobID), workerSecret, "").Body.Close()
	resp := f.do(t, http.MethodPost, "/internal/ci/jobs/"+id(f.jobID)+"/complete?status=bogus", workerSecret, "x")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad status: code = %d, want 400", resp.StatusCode)
	}
}

func TestReconcileStuckSweepsRunningJobs(t *testing.T) {
	ctx := context.Background()
	f := newReportFixture(t, nil)
	// Claim to put the job in running.
	f.do(t, http.MethodGet, "/internal/ci/jobs/"+id(f.jobID), workerSecret, "").Body.Close()

	// Sweep with a clock far in the future so the running job is past the bound.
	future := time.Now().Add(48 * time.Hour)
	if err := f.api.ReconcileStuck(ctx, time.Hour, future); err != nil {
		t.Fatalf("ReconcileStuck: %v", err)
	}
	job, _ := f.md.GetJob(ctx, f.jobID)
	if job.Status != meta.RunError {
		t.Errorf("stuck job status = %q, want error", job.Status)
	}
	run, _ := f.md.GetRun(ctx, f.runID)
	if run.Status != meta.RunError {
		t.Errorf("run status after sweep = %q, want error", run.Status)
	}
}

func TestLogChunkAppendsToLiveBuffer(t *testing.T) {
	f := newReportFixture(t, nil)

	resp := f.do(t, http.MethodPost, "/internal/ci/jobs/"+id(f.jobID)+"/log", workerSecret, "step 1...\n")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("log chunk status = %d, want 204", resp.StatusCode)
	}
	f.do(t, http.MethodPost, "/internal/ci/jobs/"+id(f.jobID)+"/log", workerSecret, "step 2...\n").Body.Close()

	data, _, done, ok := f.api.live.Read(f.jobID, 0)
	if !ok || done {
		t.Fatalf("live Read = ok %v done %v, want ok=true done=false", ok, done)
	}
	if string(data) != "step 1...\nstep 2...\n" {
		t.Errorf("live buffer = %q, want the two appended chunks", data)
	}
}

func TestLogChunkRequiresAuth(t *testing.T) {
	// The failure path: an unauthenticated chunk is rejected and never buffered.
	f := newReportFixture(t, nil)
	resp := f.do(t, http.MethodPost, "/internal/ci/jobs/"+id(f.jobID)+"/log", "", "sneaky\n")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated chunk status = %d, want 401", resp.StatusCode)
	}
	if _, _, _, ok := f.api.live.Read(f.jobID, 0); ok {
		t.Error("rejected chunk must not create a live buffer")
	}
}

func TestCompleteFinishesLiveTail(t *testing.T) {
	f := newReportFixture(t, nil)
	f.do(t, http.MethodGet, "/internal/ci/jobs/"+id(f.jobID), workerSecret, "").Body.Close()
	f.do(t, http.MethodPost, "/internal/ci/jobs/"+id(f.jobID)+"/log", workerSecret, "running...\n").Body.Close()
	f.do(t, http.MethodPost, "/internal/ci/jobs/"+id(f.jobID)+"/complete?status=passed", workerSecret, "running...\ndone\n").Body.Close()

	// Completion flips the live tail to done so a watching browser reloads to the
	// durable log.
	if _, _, done, ok := f.api.live.Read(f.jobID, 0); !ok || !done {
		t.Fatalf("live tail after complete: ok %v done %v, want both true", ok, done)
	}
}

func id(n int64) string { return strconv.FormatInt(n, 10) }
