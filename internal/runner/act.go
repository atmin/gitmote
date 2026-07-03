package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// secretEnvPrefix namespaces CI secrets the dispatcher injects into the runner
// env (kept in sync with internal/ci's constant of the same name). The engine
// strips the prefix and forwards each as an act secret.
const secretEnvPrefix = "GITMOTE_CI_SECRET_"

// ActEngine runs .github/workflows with nektos/act — the locked engine
// (tasks/16-ci.md Stage 0 #1). act runs each job in a container via the local
// Docker/podman daemon, so one definition runs both self-hosted and on the
// GitHub mirror. It requires `act` on PATH and a reachable Docker daemon.
type ActEngine struct{}

// Run invokes act over workflowDir inside repoDir on the default (push) event,
// capturing combined stdout+stderr. act's non-zero exit means the workflow
// failed (passed=false); a failure to start act at all is an engine error.
//
// Injected CI secrets (GITMOTE_CI_SECRET_<NAME> in the env) are forwarded to act
// as `-s NAME`, which act reads from its environment (so values never touch the
// argv, and multiline values survive) and exposes to the workflow as
// `${{ secrets.NAME }}` — exactly as on GitHub.
func (ActEngine) Run(ctx context.Context, repoDir, workflowDir string) ([]byte, bool, error) {
	if _, err := exec.LookPath("act"); err != nil {
		return nil, false, fmt.Errorf("act not found on PATH — install nektos/act (https://github.com/nektos/act): %w", err)
	}

	args := []string{"--workflows", workflowDir}
	actEnv := os.Environ()
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		if secret, ok := strings.CutPrefix(name, secretEnvPrefix); ok && secret != "" {
			// Re-export under the bare name so `act -s NAME` finds it in the env.
			actEnv = append(actEnv, secret+"="+val)
			args = append(args, "-s", secret)
		}
	}

	cmd := exec.CommandContext(ctx, "act", args...)
	cmd.Dir = repoDir
	cmd.Env = actEnv
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
