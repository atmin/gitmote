package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/atmin/gitmote/internal/meta"
	"github.com/atmin/gitmote/internal/render"
	"github.com/atmin/gitmote/internal/store"
)

// ciRunsLimit caps the recent-runs page; a 101st run becomes the "more" marker
// rather than being silently dropped.
const ciRunsLimit = 100

// ciRuns renders a repo's recent CI runs, newest first.
func (h *Handler) ciRuns(w http.ResponseWriter, r *http.Request, repoName string) {
	rp, ok := h.ciRepo(w, r, repoName)
	if !ok {
		return
	}
	runs, err := h.md.ListRuns(r.Context(), rp.ID, ciRunsLimit+1)
	if err != nil {
		h.serverError(w, "list runs", err)
		return
	}
	more := false
	if len(runs) > ciRunsLimit {
		runs, more = runs[:ciRunsLimit], true
	}
	h.render(w, "ci_runs.html", ciRunsData{
		base: h.base(r, "", ""),
		Repo: rp.Name,
		Runs: runs,
		More: more,
	})
}

// ciRun renders either a run's detail (arg = "{id}") or one job's log
// (arg = "{id}/job/{jid}/log").
func (h *Handler) ciRun(w http.ResponseWriter, r *http.Request, repoName, arg string) {
	idStr, rest, hasRest := strings.Cut(arg, "/")
	runID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rp, ok := h.ciRepo(w, r, repoName)
	if !ok {
		return
	}
	run, err := h.md.GetRun(r.Context(), runID)
	if errors.Is(err, meta.ErrNotFound) || (err == nil && run.RepoID != rp.ID) {
		http.NotFound(w, r) // unknown run, or a run belonging to another repo
		return
	}
	if err != nil {
		h.serverError(w, "get run", err)
		return
	}

	if !hasRest {
		h.ciRunDetail(w, r, rp, run)
		return
	}
	// Sub-resources of a run: "job/{jid}/log" (the log page) and
	// "job/{jid}/log/live" (the live-tail JSON the page polls while it runs).
	parts := strings.Split(rest, "/")
	live := len(parts) == 4 && parts[3] == "live"
	if (len(parts) != 3 && !live) || parts[0] != "job" || parts[2] != "log" {
		http.NotFound(w, r)
		return
	}
	jobID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if live {
		h.ciJobLogLive(w, r, run, jobID)
		return
	}
	h.ciJobLog(w, r, rp, run, jobID)
}

func (h *Handler) ciRunDetail(w http.ResponseWriter, r *http.Request, rp *meta.Repo, run *meta.Run) {
	jobs, err := h.md.ListJobs(r.Context(), run.ID)
	if err != nil {
		h.serverError(w, "list jobs", err)
		return
	}
	h.render(w, "ci_run.html", ciRunData{
		base: h.base(r, "", ""),
		Repo: rp.Name,
		Run:  *run,
		Jobs: jobs,
	})
}

func (h *Handler) ciJobLog(w http.ResponseWriter, r *http.Request, rp *meta.Repo, run *meta.Run, jobID int64) {
	job, err := h.md.GetJob(r.Context(), jobID)
	if errors.Is(err, meta.ErrNotFound) || (err == nil && job.RunID != run.ID) {
		http.NotFound(w, r) // unknown job, or a job under a different run
		return
	}
	if err != nil {
		h.serverError(w, "get job", err)
		return
	}

	data := ciLogData{
		base: h.base(r, "", ""),
		Repo: rp.Name,
		Run:  *run,
		Job:  *job,
	}
	switch {
	case !isTerminalStatus(job.Status):
		// Still queued/running: the page tails the live buffer and reloads on done.
		data.Live = true
	case job.LogKey == "":
		data.Note = "No log yet — the job hasn't reported."
	default:
		h.loadLog(r, job.LogKey, &data)
	}
	h.render(w, "ci_log.html", data)
}

// ciJobLogLive answers the log page's poll for a running job: the live buffer's
// bytes from offset onward, the next offset, and whether the job has finished (so
// the page reloads to the durable log). Falls back to done-by-status when there is
// no live buffer (never started, swept, or the live store is disabled).
func (h *Handler) ciJobLogLive(w http.ResponseWriter, r *http.Request, run *meta.Run, jobID int64) {
	job, err := h.md.GetJob(r.Context(), jobID)
	if errors.Is(err, meta.ErrNotFound) || (err == nil && job.RunID != run.ID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, "get job", err)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	resp := liveLogResp{Next: offset}
	if data, next, done, ok := h.readLive(jobID, offset); ok {
		resp.Bytes, resp.Next, resp.Done = string(data), next, done
	} else {
		resp.Done = isTerminalStatus(job.Status)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// readLive reads the live buffer, returning ok=false when the store is disabled.
func (h *Handler) readLive(jobID int64, offset int) (data []byte, next int, done, ok bool) {
	if h.live == nil {
		return nil, offset, false, false
	}
	return h.live.Read(jobID, offset)
}

// liveLogResp is the JSON the log page polls: new bytes, the next offset to poll
// from, and whether the job has finished.
type liveLogResp struct {
	Bytes string `json:"bytes"`
	Next  int    `json:"next"`
	Done  bool   `json:"done"`
}

// isTerminalStatus reports whether a run/job status is a finished state.
func isTerminalStatus(s meta.RunStatus) bool {
	switch s {
	case meta.RunPassed, meta.RunFailed, meta.RunError, meta.RunSuperseded:
		return true
	default:
		return false
	}
}

// loadLog fetches a job's log blob and fills data with either the ANSI-rendered
// HTML or, over the render cap, a plain-text prefix with a note. Read failures
// degrade to a note — the run detail already showed the status.
func (h *Handler) loadLog(r *http.Request, key string, data *ciLogData) {
	rc, err := h.store.Get(r.Context(), key)
	if errors.Is(err, store.ErrNotFound) {
		data.Note = "Log not found in the object store."
		return
	}
	if err != nil {
		h.log.Error("get ci log", "key", key, "error", err)
		data.Note = "Could not read the log."
		return
	}
	defer func() { _ = rc.Close() }()

	raw, err := io.ReadAll(io.LimitReader(rc, render.MaxSize+1))
	if err != nil {
		h.log.Error("read ci log", "key", key, "error", err)
		data.Note = "Could not read the log."
		return
	}
	if len(raw) > render.MaxSize {
		data.Note = "Log is large; showing the first part as plain text."
		data.Plain = string(raw[:render.MaxSize])
		return
	}
	data.Log = render.ANSI(raw)
}

// ciRepo resolves the repo record for a CI page. CI reads meta + the object
// store, not git, so it needs no materialization.
func (h *Handler) ciRepo(w http.ResponseWriter, r *http.Request, repoName string) (*meta.Repo, bool) {
	rp, err := h.md.GetRepo(r.Context(), repoName)
	if errors.Is(err, meta.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		h.serverError(w, "get repo", err)
		return nil, false
	}
	return rp, true
}
