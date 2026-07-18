package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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

// allowBuildsEnv opts a nested-mode runner into container image builds. In
// nested mode act bind-mounts the host Docker socket into every job container by
// default, so untrusted workflow code can reach the daemon — i.e. host root.
// Unless this is truthy (strconv.ParseBool) we pass `--container-daemon-socket -`
// to suppress that mount; set it only for trusted repos on a daemon-backed
// local/VPS host that must run `docker build`. In self-hosted mode there is no
// job container to mount into, so the knob is moot.
const allowBuildsEnv = "GITMOTE_CI_ALLOW_BUILDS"

// actDaemonSocketEnv overrides the daemon-side Docker socket path act mounts into
// each job container when builds are enabled. Defaults to defaultActDaemonSocket.
// The mount source must be the socket path the *daemon* sees, not DOCKER_HOST: for
// a VM-backed daemon (colima, Docker Desktop on macOS) DOCKER_HOST is a host path
// the in-VM daemon can't resolve, so letting act derive the mount from it fails
// with "operation not supported". Override only for an exotic daemon (e.g. rootless
// docker at $XDG_RUNTIME_DIR/docker.sock).
const actDaemonSocketEnv = "GITMOTE_ACT_DAEMON_SOCKET"

// defaultActDaemonSocket is the daemon's own socket path for native docker and for
// VM-backed daemons (colima, Docker Desktop) alike.
const defaultActDaemonSocket = "/var/run/docker.sock"

// ActEngine runs the repo's workflows with nektos/act — the locked engine
// (tasks/16-ci.md Stage 0 #1). act runs each job in a container via the local
// Docker/podman daemon and speaks GitHub-Actions YAML, so a workflow is
// byte-identical whether it lives in .gitmote/workflows (run here) or
// .github/workflows (run on the GitHub mirror — break-glass). It requires `act`
// on PATH and a reachable Docker daemon.
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
	var selfHosted, allowBuilds bool
	daemonSocket := defaultActDaemonSocket
	for _, kv := range environ {
		name, val, _ := strings.Cut(kv, "=")
		switch name {
		case actPlatformsEnv:
			for _, p := range strings.Split(val, ",") {
				if p = strings.TrimSpace(p); p != "" {
					args = append(args, "-P", p)
					selfHosted = true
				}
			}
		case allowBuildsEnv:
			allowBuilds, _ = strconv.ParseBool(strings.TrimSpace(val))
		case actDaemonSocketEnv:
			if v := strings.TrimSpace(val); v != "" {
				daemonSocket = v
			}
		default:
			if secret, ok := strings.CutPrefix(name, secretEnvPrefix); ok && secret != "" {
				extraEnv = append(extraEnv, secret+"="+val)
				args = append(args, "-s", secret)
			}
		}
	}
	// How the job-container Docker socket is mounted:
	switch {
	case selfHosted:
		// No nested job container to mount into — the step runs in the container
		// directly, so the flag would be meaningless.
	case !allowBuilds:
		// Nested mode mounts the host Docker socket into each job container by
		// default, handing untrusted workflow code the daemon (host root). Suppress
		// it unless builds are explicitly opted in.
		args = append(args, "--container-daemon-socket", "-")
	default:
		// Builds opted in: mount the daemon's own socket so `docker build` reaches
		// it. Pin the daemon-side path rather than let act derive the mount source
		// from DOCKER_HOST — a VM-backed daemon (colima, Docker Desktop on macOS)
		// can't resolve that host path and the mount fails.
		args = append(args, "--container-daemon-socket", daemonSocket)
	}
	return args, extraEnv
}
