package ci

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/store"
)

const (
	// logCap bounds a stored job log so a runaway workflow can't fill the store
	// (epic §Stage 0 #5). Over the cap, the log is truncated with an explicit
	// marker — never silently.
	logCap = 10 << 20 // 10 MiB
	// logTruncationMarker is appended when a log is truncated at the cap.
	logTruncationMarker = "\n\n[log truncated: exceeded 10 MiB cap]\n"
	// liveLogTTL bounds how long a job's in-memory live buffer survives without an
	// update before Sweep reclaims it — long enough for a browser to catch the
	// terminal status and switch to the durable log.
	liveLogTTL = 10 * time.Minute
)

// ReportMeta is the slice of meta the report API reads and writes. *meta.Metadata
// satisfies it.
type ReportMeta interface {
	GetJob(ctx context.Context, jobID int64) (*meta.Job, error)
	ClaimJob(ctx context.Context, jobID int64) (*meta.Job, error)
	SetJobResult(ctx context.Context, jobID int64, status meta.RunStatus, logKey string) error
	ListJobs(ctx context.Context, runID int64) ([]meta.Job, error)
	GetRun(ctx context.Context, runID int64) (*meta.Run, error)
	SetRunStatus(ctx context.Context, runID int64, status meta.RunStatus) error
	GetRepoByID(ctx context.Context, id int64) (*meta.Repo, error)
	SweepStuckJobs(ctx context.Context, cutoff time.Time) ([]int64, error)
}

// ReportAPI serves the runner's authenticated internal endpoints: claim a job
// and report its completion. Runners never touch s3lite or S3 directly — the
// parent (leader) writes the log blob and the status rows, matching the push-hook
// discipline (tasks/16-ci.md). Auth is a constant-time WORKER_SECRET compare;
// only the leader may write, so a follower returns a retryable 503.
type ReportAPI struct {
	meta     ReportMeta
	store    store.Store
	live     *LiveLogs
	isLeader func() bool
	secret   string
	logger   *slog.Logger
}

// NewReportAPI returns a report API. live is the in-memory live-log store the
// browser tails (nil makes a fresh one). isLeader gates writes (nil means always
// leader, for tests/local). An empty secret rejects every request.
func NewReportAPI(m ReportMeta, s store.Store, live *LiveLogs, isLeader func() bool, secret string, logger *slog.Logger) *ReportAPI {
	if live == nil {
		live = NewLiveLogs()
	}
	if isLeader == nil {
		isLeader = func() bool { return true }
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &ReportAPI{meta: m, store: s, live: live, isLeader: isLeader, secret: secret, logger: logger}
}

// Register mounts the internal CI routes on mux (not under /ui — these are
// machine-to-machine, WORKER_SECRET-authenticated).
func (a *ReportAPI) Register(mux *http.ServeMux) {
	mux.Handle("GET /internal/ci/jobs/{id}", http.HandlerFunc(a.handleClaim))
	mux.Handle("POST /internal/ci/jobs/{id}/log", http.HandlerFunc(a.handleLogChunk))
	mux.Handle("POST /internal/ci/jobs/{id}/complete", http.HandlerFunc(a.handleComplete))
}

// jobSpec is what a claim returns: everything the runner needs to check out and
// run the job.
type jobSpec struct {
	JobID       int64  `json:"job_id"`
	RunID       int64  `json:"run_id"`
	Repo        string `json:"repo"`
	SHA         string `json:"sha"`
	Ref         string `json:"ref"`
	WorkflowDir string `json:"workflow_dir"`
}

// handleClaim atomically claims a queued job (queued→running) and returns its
// spec. A job that is not claimable (absent, already claimed, terminal) is 404.
func (a *ReportAPI) handleClaim(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	id, ok := jobID(w, r)
	if !ok {
		return
	}

	job, err := a.meta.ClaimJob(r.Context(), id)
	if errors.Is(err, meta.ErrNotFound) {
		http.Error(w, "job not found or not claimable", http.StatusNotFound)
		return
	}
	if err != nil {
		a.logger.Error("ci: claim failed", "job", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	run, err := a.meta.GetRun(r.Context(), job.RunID)
	if err != nil {
		a.logger.Error("ci: claim run lookup failed", "run", job.RunID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	repo, err := a.meta.GetRepoByID(r.Context(), run.RepoID)
	if err != nil {
		a.logger.Error("ci: claim repo lookup failed", "repo_id", run.RepoID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, jobSpec{
		JobID:       job.ID,
		RunID:       run.ID,
		Repo:        repo.Name,
		SHA:         run.SHA,
		Ref:         run.Ref,
		WorkflowDir: workflowDir,
	})
}

// handleComplete records a job's outcome. Status and error travel as query
// params; the request body is the combined log. Content-before-pointer: the log
// blob is stored first, then the job status + log_key, then the run rolls up.
// Idempotent: completing an already-terminal job is a no-op.
func (a *ReportAPI) handleComplete(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	status, ok := completionStatus(w, r)
	if !ok {
		return
	}

	job, err := a.meta.GetJob(r.Context(), id)
	if errors.Is(err, meta.ErrNotFound) {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	if err != nil {
		a.logger.Error("ci: complete job lookup failed", "job", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if isTerminal(job.Status) {
		writeJSON(w, map[string]bool{"ok": true}) // idempotent no-op
		return
	}

	run, err := a.meta.GetRun(r.Context(), job.RunID)
	if err != nil {
		a.logger.Error("ci: complete run lookup failed", "run", job.RunID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	logKey := fmt.Sprintf("ci/%d/%d/%d.log", run.RepoID, run.ID, job.ID)
	if err := a.storeLog(r.Context(), logKey, r.Body); err != nil {
		a.logger.Error("ci: store log failed", "job", id, "key", logKey, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Pointer after content: the blob is durable before the row references it.
	if err := a.meta.SetJobResult(r.Context(), job.ID, status, logKey); err != nil {
		a.logger.Error("ci: set job result failed", "job", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.rollupRun(r.Context(), run.ID)
	// Flip the live tail to done so a watching browser switches to the durable log.
	a.live.Finish(id, time.Now())

	writeJSON(w, map[string]bool{"ok": true})
}

// handleLogChunk appends a runner's incremental log bytes to the job's in-memory
// live buffer — the best-effort tail the UI polls while the job runs. It never
// touches s3lite or S3 (the durable log is written once at completion); a lost
// chunk is a cosmetic gap, not data loss. Leader-only (the browser reads the same
// leader's buffer), so a follower's 503 just makes the runner drop the chunk.
func (a *ReportAPI) handleLogChunk(w http.ResponseWriter, r *http.Request) {
	if !a.authed(w, r) {
		return
	}
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	chunk, err := io.ReadAll(io.LimitReader(r.Body, logCap))
	if err != nil {
		http.Error(w, "read chunk", http.StatusBadRequest)
		return
	}
	a.live.Append(id, chunk, time.Now())
	w.WriteHeader(http.StatusNoContent)
}

// storeLog writes the body under key, enforcing the size cap with an explicit
// truncation marker rather than a silent cut.
func (a *ReportAPI) storeLog(ctx context.Context, key string, body io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(body, logCap+1))
	if err != nil {
		return err
	}
	if len(data) > logCap {
		data = append(data[:logCap], []byte(logTruncationMarker)...)
	}
	return a.store.Put(ctx, key, bytes.NewReader(data))
}

// rollupRun recomputes a run's status from its jobs: still running while any job
// is non-terminal, else error > failed > passed by worst outcome. Logged, not
// returned — a rollup failure must not fail an already-recorded completion.
func (a *ReportAPI) rollupRun(ctx context.Context, runID int64) {
	jobs, err := a.meta.ListJobs(ctx, runID)
	if err != nil {
		a.logger.Error("ci: rollup list jobs failed", "run", runID, "error", err)
		return
	}
	status := rollup(jobs)
	if err := a.meta.SetRunStatus(ctx, runID, status); err != nil {
		a.logger.Error("ci: rollup set run status failed", "run", runID, "error", err)
	}
}

// ReconcileStuck marks running jobs older than maxAge as error and rolls up their
// runs, so a runner that dies without reporting doesn't leave a job running
// forever. It is leader-gated: only the writer may touch s3lite, so a follower
// no-ops (the leader's sweep covers the shared state).
func (a *ReportAPI) ReconcileStuck(ctx context.Context, maxAge time.Duration, now time.Time) error {
	if !a.isLeader() {
		return nil
	}
	// Reclaim in-memory live buffers for finished or dead jobs on the same tick.
	a.live.Sweep(now, liveLogTTL)
	stuck, err := a.meta.SweepStuckJobs(ctx, now.Add(-maxAge))
	if err != nil {
		return err
	}
	for _, id := range stuck {
		job, err := a.meta.GetJob(ctx, id)
		if err != nil {
			a.logger.Error("ci: reconcile job lookup failed", "job", id, "error", err)
			continue
		}
		a.rollupRun(ctx, job.RunID)
	}
	return nil
}

// authed enforces the constant-time WORKER_SECRET check and the leader gate,
// writing the response and returning false on failure.
func (a *ReportAPI) authed(w http.ResponseWriter, r *http.Request) bool {
	if a.secret == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Worker-Secret")), []byte(a.secret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if !a.isLeader() {
		// A follower can't write s3lite; the runner should retry against the leader.
		http.Error(w, "not the writer; retry", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// jobID parses the {id} path value, writing a 400 on failure.
func jobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// completionStatus reads and validates the terminal status query param.
func completionStatus(w http.ResponseWriter, r *http.Request) (meta.RunStatus, bool) {
	status := meta.RunStatus(r.URL.Query().Get("status"))
	switch status {
	case meta.RunPassed, meta.RunFailed, meta.RunError:
		return status, true
	default:
		http.Error(w, "status must be passed, failed, or error", http.StatusBadRequest)
		return "", false
	}
}

// rollup derives a run's status from its jobs.
func rollup(jobs []meta.Job) meta.RunStatus {
	worst := meta.RunPassed
	for _, j := range jobs {
		if !isTerminal(j.Status) {
			return meta.RunRunning
		}
		switch j.Status {
		case meta.RunError:
			worst = meta.RunError
		case meta.RunFailed:
			if worst != meta.RunError {
				worst = meta.RunFailed
			}
		}
	}
	return worst
}

// isTerminal reports whether a status is a finished state.
func isTerminal(s meta.RunStatus) bool {
	switch s {
	case meta.RunPassed, meta.RunFailed, meta.RunError, meta.RunSuperseded:
		return true
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
