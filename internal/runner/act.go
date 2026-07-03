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

// actPlatformsEnv, when set, is a comma-separated list of act -P platform
// mappings (e.g. "ubuntu-latest=-self-hosted"). The cloud runner image sets it
// to run steps directly in the Job container — Serverless Jobs have no Docker
// daemon for act to nest into. Unset (local dev, where a real daemon exists) →
// act's default nested-container behavior, unchanged.
const actPlatformsEnv = "GITMOTE_ACT_PLATFORMS"

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

	args, extraEnv := actArgs(workflowDir, os.Environ())
	actEnv := append(os.Environ(), extraEnv...)

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

// actArgs builds act's arguments and the extra env for one invocation from the
// process environment. Injected secrets (GITMOTE_CI_SECRET_<NAME>) become
// `-s NAME` plus a bare NAME=value in extraEnv (act reads the value from its env,
// keeping it off the argv). GITMOTE_ACT_PLATFORMS becomes one `-P` per mapping.
// Pure, so it is unit-testable without act on PATH.
func actArgs(workflowDir string, environ []string) (args, extraEnv []string) {
	args = []string{"--workflows", workflowDir}
	for _, kv := range environ {
		name, val, _ := strings.Cut(kv, "=")
		switch name {
		case actPlatformsEnv:
			for _, p := range strings.Split(val, ",") {
				if p = strings.TrimSpace(p); p != "" {
					args = append(args, "-P", p)
				}
			}
		default:
			if secret, ok := strings.CutPrefix(name, secretEnvPrefix); ok && secret != "" {
				extraEnv = append(extraEnv, secret+"="+val)
				args = append(args, "-s", secret)
			}
		}
	}
	return args, extraEnv
}
