// Command gitmote-runner executes one CI job: it claims a queued job from
// gitmote's internal report API, checks out the repo at the pushed SHA, runs the
// workflow with act, and reports the log + pass/fail back. It is spawned per job
// — locally by LocalTrigger (dev) or as a Scaleway Serverless Job (cloud) — and
// reads its coordinates from the environment the trigger injects.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/atmin/gitmote/internal/runner"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := runner.ConfigFromEnv()
	if err != nil {
		logger.Error("ci runner: bad configuration", "error", err)
		os.Exit(1)
	}
	cfg.Cloner = runner.GitCloner{}
	cfg.Engine = runner.ActEngine{}
	cfg.Logger = logger

	if err := runner.Run(context.Background(), cfg); err != nil {
		// An internal failure we could not report — exit non-zero so a crashed
		// runner is distinguishable from a failed build (which reports and exits 0).
		logger.Error("ci runner: internal failure", "job", cfg.JobID, "error", err)
		os.Exit(1)
	}
}
