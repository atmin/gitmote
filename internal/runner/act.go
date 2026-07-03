package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// ActEngine runs .github/workflows with nektos/act — the locked engine
// (tasks/16-ci.md Stage 0 #1). act runs each job in a container via the local
// Docker/podman daemon, so one definition runs both self-hosted and on the
// GitHub mirror. It requires `act` on PATH and a reachable Docker daemon.
type ActEngine struct{}

// Run invokes act over workflowDir inside repoDir on the default (push) event,
// capturing combined stdout+stderr. act's non-zero exit means the workflow
// failed (passed=false); a failure to start act at all is an engine error.
func (ActEngine) Run(ctx context.Context, repoDir, workflowDir string) ([]byte, bool, error) {
	if _, err := exec.LookPath("act"); err != nil {
		return nil, false, fmt.Errorf("act not found on PATH — install nektos/act (https://github.com/nektos/act): %w", err)
	}
	cmd := exec.CommandContext(ctx, "act", "--workflows", workflowDir)
	cmd.Dir = repoDir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	if err == nil {
		return buf.Bytes(), true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// act ran and the workflow failed — a normal failed build, not an
		// internal error.
		return buf.Bytes(), false, nil
	}
	return buf.Bytes(), false, err
}
