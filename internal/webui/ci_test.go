package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/atmin/gitmote/internal/meta"
)

// fakeLive is a fixed live-log buffer for exercising the live-tail endpoint.
type fakeLive struct {
	data []byte
	done bool
	ok   bool
}

func (f fakeLive) Read(_ int64, offset int) ([]byte, int, bool, bool) {
	if !f.ok {
		return nil, offset, false, false
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(f.data) {
		offset = len(f.data)
	}
	return f.data[offset:], len(f.data), f.done, true
}

// seedQueuedJob creates an ACL-granted repo, a run, and a queued (non-terminal)
// job with no log yet — the running-job state the live tail serves.
func (x *harness) seedQueuedJob(repoName, sha string) (runID, jobID int64) {
	x.t.Helper()
	ctx := context.Background()
	r, err := x.md.CreateRepo(ctx, repoName, "main")
	if err != nil {
		x.t.Fatalf("CreateRepo: %v", err)
	}
	if err := x.md.SetACL(ctx, r.ID, x.admin.ID, meta.PermAdmin); err != nil {
		x.t.Fatalf("SetACL: %v", err)
	}
	run, err := x.md.CreateRun(ctx, r.ID, "refs/heads/main", sha)
	if err != nil {
		x.t.Fatalf("CreateRun: %v", err)
	}
	job, err := x.md.CreateJob(ctx, run.ID, "ci.yml")
	if err != nil {
		x.t.Fatalf("CreateJob: %v", err)
	}
	return run.ID, job.ID
}

// seedRun creates a repo (if repoName is new), a run, and one job with a stored
// log blob, returning the ids. The log carries ANSI + HTML to exercise rendering.
func (x *harness) seedRun(repoName, sha, log string) (repoID, runID, jobID int64) {
	x.t.Helper()
	ctx := context.Background()
	r, err := x.md.GetRepo(ctx, repoName)
	if err != nil {
		r, err = x.md.CreateRepo(ctx, repoName, "main")
		if err != nil {
			x.t.Fatalf("CreateRepo: %v", err)
		}
	}
	// Browse (the CI run views) is gated on repo-read; grant the harness admin an
	// ACL so it can view runs on this private-by-default repo.
	if err := x.md.SetACL(ctx, r.ID, x.admin.ID, meta.PermAdmin); err != nil {
		x.t.Fatalf("SetACL: %v", err)
	}
	run, err := x.md.CreateRun(ctx, r.ID, "refs/heads/main", sha)
	if err != nil {
		x.t.Fatalf("CreateRun: %v", err)
	}
	job, err := x.md.CreateJob(ctx, run.ID, "ci.yml")
	if err != nil {
		x.t.Fatalf("CreateJob: %v", err)
	}
	key := "ci/1/" + itoa(run.ID) + "/" + itoa(job.ID) + ".log"
	if err := x.store.Put(ctx, key, bytes.NewReader([]byte(log))); err != nil {
		x.t.Fatalf("store log: %v", err)
	}
	if err := x.md.SetJobResult(ctx, job.ID, meta.RunPassed, key); err != nil {
		x.t.Fatalf("SetJobResult: %v", err)
	}
	return r.ID, run.ID, job.ID
}

func TestCIRunsListAndDetail(t *testing.T) {
	x := newHarness(t)
	session := x.login(x.mintTokenFor(x.admin.ID))
	_, runID, jobID := x.seedRun("app", "deadbeefcafe1234", "ok\n")

	// Runs list links to the run and shows the short SHA.
	rec := x.do(http.MethodGet, "/app/runs", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("runs list: %d (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/app/runs/"+itoa(runID)) || !strings.Contains(body, "deadbeef") {
		t.Errorf("runs list missing run link or short sha: %s", body)
	}

	// Run detail shows the job and links to its log.
	rec = x.do(http.MethodGet, "/app/runs/"+itoa(runID), nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("run detail: %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "ci.yml") ||
		!strings.Contains(rec.Body.String(), "/runs/"+itoa(runID)+"/job/"+itoa(jobID)+"/log") {
		t.Errorf("run detail missing job or log link: %s", rec.Body)
	}
}

func TestCIJobLogRendersSafely(t *testing.T) {
	x := newHarness(t)
	session := x.login(x.mintTokenFor(x.admin.ID))
	// Green "ok" via ANSI, then HTML that must be escaped.
	_, runID, jobID := x.seedRun("app", "sha1", "\x1b[32mok\x1b[0m <b>x</b>\n")

	rec := x.do(http.MethodGet, "/app/runs/"+itoa(runID)+"/job/"+itoa(jobID)+"/log", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("log view: %d (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ok</span>") {
		t.Errorf("ANSI color not rendered as a span: %s", body)
	}
	if strings.Contains(body, "<b>x</b>") {
		t.Errorf("log HTML not escaped (injection): %s", body)
	}
	if !strings.Contains(body, "&lt;b&gt;x&lt;/b&gt;") {
		t.Errorf("expected escaped log HTML: %s", body)
	}
}

func TestCIRunNotFound(t *testing.T) {
	x := newHarness(t)
	session := x.login(x.mintTokenFor(x.admin.ID))
	r, err := x.md.CreateRepo(context.Background(), "app", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.md.SetACL(context.Background(), r.ID, x.admin.ID, meta.PermAdmin); err != nil {
		t.Fatal(err)
	}
	// Unknown run id → 404.
	if rec := x.do(http.MethodGet, "/app/runs/9999", nil, session); rec.Code != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", rec.Code)
	}
}

func TestCIRunCrossRepoIsNotFound(t *testing.T) {
	x := newHarness(t)
	session := x.login(x.mintTokenFor(x.admin.ID))
	_, runID, _ := x.seedRun("app", "sha1", "x")
	other, err := x.md.CreateRepo(context.Background(), "other", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.md.SetACL(context.Background(), other.ID, x.admin.ID, meta.PermAdmin); err != nil {
		t.Fatal(err)
	}
	// A run that belongs to app must not be reachable under other.
	rec := x.do(http.MethodGet, "/other/runs/"+itoa(runID), nil, session)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-repo run = %d, want 404", rec.Code)
	}
}

func TestCIRunsRequireAdmin(t *testing.T) {
	x := newHarness(t)
	// Unauthenticated GET is redirected to login like the rest of browsing.
	if rec := x.do(http.MethodGet, "/app/runs", nil, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("unauth runs = %d, want 303 redirect", rec.Code)
	}
}

func TestCILiveLogStreamsBytes(t *testing.T) {
	x := newHarness(t)
	x.h.live = fakeLive{data: []byte("building...\n"), ok: true}
	session := x.login(x.mintTokenFor(x.admin.ID))
	runID, jobID := x.seedQueuedJob("app", "sha1")

	rec := x.do(http.MethodGet, "/app/runs/"+itoa(runID)+"/job/"+itoa(jobID)+"/log/live?offset=0", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("live poll: %d (%s)", rec.Code, rec.Body)
	}
	var resp struct {
		Bytes string `json:"bytes"`
		Next  int    `json:"next"`
		Done  bool   `json:"done"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Bytes != "building...\n" || resp.Next != 12 || resp.Done {
		t.Errorf("live resp = %+v, want the buffered bytes, next 12, not done", resp)
	}
}

func TestCILiveLogDoneWhenNoBufferAndTerminal(t *testing.T) {
	// The fallback path: no live buffer + a finished job → done=true, so the page
	// stops polling and reloads to the durable log.
	x := newHarness(t)
	x.h.live = fakeLive{ok: false}
	session := x.login(x.mintTokenFor(x.admin.ID))
	_, runID, jobID := x.seedRun("app", "sha1", "done\n") // seedRun marks the job passed

	rec := x.do(http.MethodGet, "/app/runs/"+itoa(runID)+"/job/"+itoa(jobID)+"/log/live", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("live poll: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"done":true`) {
		t.Errorf("live resp = %s, want done=true for a finished job with no buffer", rec.Body)
	}
}

func TestCIJobLogPageTailsRunningJob(t *testing.T) {
	x := newHarness(t)
	x.h.live = fakeLive{data: []byte("x"), ok: true}
	session := x.login(x.mintTokenFor(x.admin.ID))
	runID, jobID := x.seedQueuedJob("app", "sha1")

	// A running job's log page renders the live-tail element + poll script instead
	// of a durable log.
	rec := x.do(http.MethodGet, "/app/runs/"+itoa(runID)+"/job/"+itoa(jobID)+"/log", nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("log page: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="livelog"`) || !strings.Contains(body, "/log/live") {
		t.Errorf("running-job log page missing live tail: %s", body)
	}
}

func TestCICommitBadge(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	session := x.login(x.mintTokenFor(x.admin.ID))
	head, first := x.seedBrowseRepo("repo", "main")
	r, _ := x.md.GetRepo(ctx, "repo")
	run, err := x.md.CreateRun(ctx, r.ID, "refs/heads/main", head)
	if err != nil {
		t.Fatal(err)
	}

	// The commit with a run shows a badge linking to it.
	rec := x.do(http.MethodGet, "/repo/commit/"+head, nil, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("commit page: %d (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "/repo/runs/"+itoa(run.ID)) {
		t.Errorf("commit with a run is missing the CI badge: %s", rec.Body)
	}

	// A commit without a run shows no badge.
	rec = x.do(http.MethodGet, "/repo/commit/"+first, nil, session)
	if strings.Contains(rec.Body.String(), "/repo/runs/") {
		t.Errorf("commit without a run should have no badge: %s", rec.Body)
	}
}
