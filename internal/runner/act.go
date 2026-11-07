package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// secretEnvPrefix namespaces the CI secrets that the dispatcher injects into
// the runner env. Keep it in sync with the constant of the same name in
// internal/ci. The engine strips the prefix and forwards each secret as an act
// secret.
const secretEnvPrefix = "GITMOTE_CI_SECRET_"

// actPlatformsEnv, when set, is a comma-separated list of act -P platform
// mappings, for example "ubuntu-latest=-self-hosted". The cloud runner image
// sets it to run steps directly in the Job container. Serverless Jobs have no
// Docker daemon for act to nest into. When it is unset (local dev, where a
// real daemon exists), act keeps its default nested-container behavior.
const actPlatformsEnv = "GITMOTE_ACT_PLATFORMS"

// allowBuildsEnv opts a nested-mode runner into container image builds. In
// nested mode, act bind-mounts the host Docker socket into every job container
// by default. So untrusted workflow code can reach the daemon, that is, root
// on the host. Unless this is truthy (strconv.ParseBool), we pass
// `--container-daemon-socket -` to suppress that mount. Set it only for
// trusted repos on a daemon-backed local or VPS host that must run
// `docker build`. In self-hosted mode, there is no job container to mount
// into, so the setting has no effect.
const allowBuildsEnv = "GITMOTE_CI_ALLOW_BUILDS"

// actDaemonSocketEnv overrides the daemon-side Docker socket path that act
// mounts into each job container when builds are enabled. It defaults to
// defaultActDaemonSocket. The mount source must be the socket path that the
// *daemon* sees, not DOCKER_HOST. For a VM-backed daemon (colima, Docker
// Desktop on macOS), DOCKER_HOST is a host path that the in-VM daemon can't
// resolve. If act derives the mount from it, the mount fails with "operation
// not supported". Override it only for an unusual daemon, for example rootless
// docker at $XDG_RUNTIME_DIR/docker.sock.
const actDaemonSocketEnv = "GITMOTE_ACT_DAEMON_SOCKET"

// defaultActDaemonSocket is the socket path of the daemon itself. It is the
// same for native docker and for VM-backed daemons (colima, Docker Desktop).
const defaultActDaemonSocket = "/var/run/docker.sock"

// ActEngine runs the workflows of the repo with nektos/act, the locked engine
// (docs/architecture/ci.md). act speaks GitHub-Actions YAML. So a workflow is
// byte-identical in two places:
//   - .gitmote/workflows, which runs here
//   - .github/workflows, which runs on the GitHub mirror as an emergency
//     fallback
//
// ActEngine requires `act` on PATH. In nested mode (the default), act runs
// each job in a container through the local Docker or podman daemon. Nested
// mode requires a reachable Docker daemon. In self-hosted mode
// (actPlatformsEnv is set), act runs the steps directly, with no job
// container.
type ActEngine struct{}

// Run invokes act over workflowDir inside repoDir on the default (push) event.
// It captures stdout and stderr combined. A non-zero exit of act means that the
// workflow failed (passed=false). A failure to start act at all is an engine
// error.
//
// Run forwards injected CI secrets (GITMOTE_CI_SECRET_<NAME> in the env) to act
// as `-s NAME`. act reads each value from its environment, so values never
// touch the argv, and multiline values survive. act exposes each secret to the
// workflow as `${{ secrets.NAME }}`, exactly as on GitHub.
func (ActEngine) Run(ctx context.Context, repoDir, workflowDir string, sink io.Writer) ([]byte, bool, error) {
	if _, err := exec.LookPath("act"); err != nil {
		return nil, false, fmt.Errorf("act not found on PATH — install nektos/act (https://github.com/nektos/act): %w", err)
	}

	args, extraEnv := actArgs(workflowDir, os.Environ())
	actEnv := append(os.Environ(), extraEnv...)

	cmd := exec.CommandContext(ctx, "act", args...)
	cmd.Dir = repoDir
	cmd.Env = actEnv
	// Capture the full combined output for the durable log. Also tee it to sink
	// for the live tail. A nil sink means capture only.
	var buf bytes.Buffer
	var out io.Writer = &buf
	if sink != nil {
		out = io.MultiWriter(&buf, sink)
	}
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	if err == nil {
		return buf.Bytes(), true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// act ran and the workflow failed. This is a normal failed build, not an
		// internal error.
		return buf.Bytes(), false, nil
	}
	return buf.Bytes(), false, err
}

// actArgs builds the arguments of act and the extra env for one invocation from
// the process environment. Each injected secret (GITMOTE_CI_SECRET_<NAME>)
// becomes `-s NAME` plus a bare NAME=value in extraEnv. act reads the value
// from its env, which keeps it off the argv. GITMOTE_ACT_PLATFORMS becomes one
// `-P` per mapping. actArgs is pure, so it is unit-testable without act on
// PATH.
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
		// There is no nested job container to mount into. The step runs in the
		// container directly, so the flag would be meaningless.
	case !allowBuilds:
		// Nested mode mounts the host Docker socket into each job container by
		// default. That gives untrusted workflow code the daemon, which is root on
		// the host. Suppress the mount unless builds are explicitly opted in.
		args = append(args, "--container-daemon-socket", "-")
	default:
		// Builds are opted in. Mount the socket of the daemon itself, so
		// `docker build` reaches it. Pin the daemon-side path rather than let act
		// derive the mount source from DOCKER_HOST. A VM-backed daemon (colima,
		// Docker Desktop on macOS) can't resolve that host path, and the mount
		// fails.
		args = append(args, "--container-daemon-socket", daemonSocket)
	}
	return args, extraEnv
}
