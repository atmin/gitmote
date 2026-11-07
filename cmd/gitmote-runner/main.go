// Command gitmote-runner executes one CI job. It does these steps:
//   - It claims a queued job from gitmote's internal report API.
//   - It checks out the repo at the pushed SHA.
//   - It runs the workflow with act.
//   - It reports the log and the pass or fail result back.
//
// A trigger spawns one runner for each job. In dev, LocalTrigger spawns it
// locally. In the cloud, it runs as a Scaleway Serverless Job. It reads its
// coordinates from the environment that the trigger injects.
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
		// This is an internal failure that the runner could not report. Exit
		// non-zero, so a crashed runner differs from a failed build. A failed
		// build reports and exits 0.
		logger.Error("ci runner: internal failure", "job", cfg.JobID, "error", err)
		os.Exit(1)
	}
}
