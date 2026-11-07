// Package runner is the CI job runner. It is the code that does these steps:
//   - It claims a queued job from the internal report API of gitmote.
//   - It checks out the repo at the pushed SHA.
//   - It runs the workflow engine over .gitmote/workflows.
//   - It reports back the combined log and the pass or fail result.
//
// It is deliberately substrate-agnostic. The same code runs as a local process
// (LocalTrigger, dev) and inside a Scaleway Serverless Job container (cloud).
// It never touches s3lite or S3. The leader writes the log blob and the status
// rows in response to the report calls.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// errNotClaimable means that the job is absent, already claimed or terminal.
// The runner has nothing to do and exits cleanly. Another runner owns the job,
// or the job is already reported.
var errNotClaimable = errors.New("job not claimable")

// Cloner fetches a repo at an exact SHA into dest. When ref names a branch, the
// SHA goes onto that branch. Cloner authenticates with a scoped token over the
// git-HTTP endpoint of baseURL. GitCloner is the real implementation.
type Cloner interface {
	Clone(ctx context.Context, baseURL, repo, sha, ref, token, dest string) error
}

// Engine runs the workflows under workflowDir within repoDir. It returns the
// combined log and whether the run passed. A non-nil error means that the
// engine could not execute at all, for example because act is missing. That is
// distinct from a workflow that ran and failed (passed=false, err=nil).
//
// Engine also tees the combined output to sink as it is produced. A nil sink
// disables this. The runner uses sink to stream a live tail. sink is
// best-effort and never affects the returned log or status.
type Engine interface {
	Run(ctx context.Context, repoDir, workflowDir string, sink io.Writer) (log []byte, passed bool, err error)
}

// Config wires one runner invocation.
type Config struct {
	BaseURL      string // GITMOTE_URL, for example http://localhost:8080
	JobID        int64  // GITMOTE_CI_JOB_ID
	WorkerSecret string // WORKER_SECRET, which authenticates the report API
	CloneToken   string // GITMOTE_CI_CLONE_TOKEN: read-only, repo-scoped, expiring

	Cloner     Cloner
	Engine     Engine
	HTTPClient *http.Client
	WorkDir    string // parent dir for the checkout. A temp dir when empty.
	Logger     *slog.Logger
}

// spec is the job coordinates that a claim returns. It mirrors the jobSpec of
// the report API.
type spec struct {
	JobID       int64  `json:"job_id"`
	RunID       int64  `json:"run_id"`
	Repo        string `json:"repo"`
	SHA         string `json:"sha"`
	Ref         string `json:"ref"`
	WorkflowDir string `json:"workflow_dir"`
}

// Run executes one job end-to-end, in this order: claim, clone, engine,
// complete. It returns an error only for an internal failure that it could not
// report. A failed claim or a failed completion POST is such a failure. A
// workflow that runs and fails is a successful run of the runner. The runner
// then reports status=failed. The caller (the runner binary) exits non-zero
// on a returned error. So a crashed runner is distinguishable from a failed
// build.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}

	sp, err := claim(ctx, cfg)
	if errors.Is(err, errNotClaimable) {
		cfg.Logger.Info("ci runner: job not claimable, nothing to do", "job", cfg.JobID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim job %d: %w", cfg.JobID, err)
	}
	cfg.Logger.Info("ci runner: claimed job", "job", sp.JobID, "run", sp.RunID, "repo", sp.Repo, "sha", sp.SHA)

	workDir := cfg.WorkDir
	if workDir == "" {
		d, err := os.MkdirTemp("", "gitmote-ci-")
		if err != nil {
			return fmt.Errorf("work dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(d) }()
		workDir = d
	}
	dest := filepath.Join(workDir, "repo")

	if err := cfg.Cloner.Clone(ctx, cfg.BaseURL, sp.Repo, sp.SHA, sp.Ref, cfg.CloneToken, dest); err != nil {
		cfg.Logger.Error("ci runner: clone failed", "job", sp.JobID, "error", err)
		return complete(ctx, cfg, sp.JobID, "error", []byte("clone failed: "+err.Error()+"\n"))
	}

	// Stream the combined output to the live buffer of the leader as it is
	// produced, so the UI can tail the job. This is best-effort. complete below
	// still ships the durable log whole. So a dropped chunk or an unreachable
	// leader is only a cosmetic gap in the live view.
	streamer := newLogStreamer(cfg, sp.JobID)
	streamer.start()
	log, passed, err := cfg.Engine.Run(ctx, dest, sp.WorkflowDir, streamer)
	streamer.stop() // final flush before the durable completion below
	if err != nil {
		cfg.Logger.Error("ci runner: engine error", "job", sp.JobID, "error", err)
		body := append(append([]byte{}, log...), []byte("\nengine error: "+err.Error()+"\n")...)
		return complete(ctx, cfg, sp.JobID, "error", body)
	}

	status := "failed"
	if passed {
		status = "passed"
	}
	cfg.Logger.Info("ci runner: workflow finished", "job", sp.JobID, "status", status)
	return complete(ctx, cfg, sp.JobID, status, log)
}

// claim performs GET /internal/ci/jobs/{id}. A 404 maps to errNotClaimable.
func claim(ctx context.Context, cfg Config) (spec, error) {
	u := fmt.Sprintf("%s/internal/ci/jobs/%d", cfg.BaseURL, cfg.JobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return spec{}, err
	}
	req.Header.Set("X-Worker-Secret", cfg.WorkerSecret)
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return spec{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return spec{}, errNotClaimable
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return spec{}, fmt.Errorf("claim: status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var sp spec
	if err := json.NewDecoder(resp.Body).Decode(&sp); err != nil {
		return spec{}, fmt.Errorf("claim: decode spec: %w", err)
	}
	return sp, nil
}

// complete performs POST /internal/ci/jobs/{id}/complete?status=... with the log
// as the body.
func complete(ctx context.Context, cfg Config, jobID int64, status string, log []byte) error {
	u := fmt.Sprintf("%s/internal/ci/jobs/%d/complete?status=%s", cfg.BaseURL, jobID, url.QueryEscape(status))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(log))
	if err != nil {
		return err
	}
	req.Header.Set("X-Worker-Secret", cfg.WorkerSecret)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("complete job %d: %w", jobID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("complete job %d: status %d: %s", jobID, resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}

// ConfigFromEnv reads the configuration of the runner from the environment
// that the trigger injects (dispatcher.jobEnv). The caller sets Cloner, Engine
// and Logger.
func ConfigFromEnv() (Config, error) {
	jobID, err := strconv.ParseInt(os.Getenv("GITMOTE_CI_JOB_ID"), 10, 64)
	if err != nil {
		return Config{}, fmt.Errorf("GITMOTE_CI_JOB_ID: %w", err)
	}
	cfg := Config{
		BaseURL:      os.Getenv("GITMOTE_URL"),
		JobID:        jobID,
		WorkerSecret: os.Getenv("WORKER_SECRET"),
		CloneToken:   os.Getenv("GITMOTE_CI_CLONE_TOKEN"),
	}
	if cfg.BaseURL == "" {
		return Config{}, errors.New("GITMOTE_URL is not set")
	}
	if cfg.WorkerSecret == "" {
		return Config{}, errors.New("WORKER_SECRET is not set")
	}
	return cfg, nil
}
