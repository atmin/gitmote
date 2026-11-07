package runner

import (
	"slices"
	"strings"
	"testing"
)

func TestActArgsForwardsSecrets(t *testing.T) {
	args, extra := actArgs(".gitmote/workflows", []string{
		"PATH=/usr/bin",
		"GITMOTE_CI_SECRET_API_TOKEN=s3cr3t",
		"GITMOTE_URL=http://x", // not a secret, so it must be ignored
	})
	if !slices.Contains(extra, "API_TOKEN=s3cr3t") {
		t.Errorf("extraEnv = %v, want the bare-named secret re-exported", extra)
	}
	if !hasFlag(args, "-s", "API_TOKEN") {
		t.Errorf("args = %v, want -s API_TOKEN", args)
	}
	// The secret value must not appear on the argv.
	if slices.ContainsFunc(args, func(s string) bool { return strings.Contains(s, "s3cr3t") }) {
		t.Errorf("secret value leaked onto argv: %v", args)
	}
}

func TestActArgsPlatforms(t *testing.T) {
	args, _ := actArgs(".gitmote/workflows", []string{
		"GITMOTE_ACT_PLATFORMS=ubuntu-latest=-self-hosted, ubuntu-22.04=-self-hosted",
	})
	if !hasFlag(args, "-P", "ubuntu-latest=-self-hosted") ||
		!hasFlag(args, "-P", "ubuntu-22.04=-self-hosted") {
		t.Errorf("args = %v, want a -P per platform mapping (trimmed)", args)
	}
}

func TestActArgsDefaultsNoOverrides(t *testing.T) {
	args, extra := actArgs(".gitmote/workflows", []string{"HOME=/root"})
	if len(extra) != 0 {
		t.Errorf("extraEnv = %v, want none", extra)
	}
	// There is no -s and no -P. This is local dev, with the default nesting of
	// act.
	if hasFlag(args, "-P", "") || slices.Contains(args, "-s") {
		t.Errorf("args = %v, want no secret/platform overrides", args)
	}
	// This is the safe default. Nested mode without an opt-in suppresses the
	// daemon-socket mount, so untrusted workflow code can't reach the host
	// daemon.
	if !hasFlag(args, "--container-daemon-socket", "-") {
		t.Errorf("args = %v, want --container-daemon-socket - by default", args)
	}
}

func TestActArgsAllowBuildsMountsDaemonSocket(t *testing.T) {
	args, _ := actArgs(".gitmote/workflows", []string{"GITMOTE_CI_ALLOW_BUILDS=1"})
	// Builds are opted in. Mount the socket path of the daemon itself, so
	// `docker build` reaches it. Do not use the act default derived from
	// DOCKER_HOST, which breaks for a VM-backed daemon.
	if !hasFlag(args, "--container-daemon-socket", "/var/run/docker.sock") {
		t.Errorf("args = %v, want --container-daemon-socket /var/run/docker.sock when builds are allowed", args)
	}
}

func TestActArgsDaemonSocketOverride(t *testing.T) {
	args, _ := actArgs(".gitmote/workflows", []string{
		"GITMOTE_CI_ALLOW_BUILDS=1",
		"GITMOTE_ACT_DAEMON_SOCKET=/run/user/1000/docker.sock",
	})
	if !hasFlag(args, "--container-daemon-socket", "/run/user/1000/docker.sock") {
		t.Errorf("args = %v, want the overridden daemon socket path", args)
	}
}

func TestActArgsAllowBuildsFalseSuppressesSocket(t *testing.T) {
	args, _ := actArgs(".gitmote/workflows", []string{"GITMOTE_CI_ALLOW_BUILDS=false"})
	if !hasFlag(args, "--container-daemon-socket", "-") {
		t.Errorf("args = %v, want the socket suppressed when builds are explicitly false", args)
	}
}

func TestActArgsSelfHostedOmitsSocketFlag(t *testing.T) {
	// Self-hosted mode has no job container to mount into, so the socket flag
	// has no use. It must not be emitted, because it would be meaningless or
	// could cause an error.
	args, _ := actArgs(".gitmote/workflows", []string{
		"GITMOTE_ACT_PLATFORMS=ubuntu-latest=-self-hosted",
	})
	if slices.Contains(args, "--container-daemon-socket") {
		t.Errorf("args = %v, want no socket flag in self-hosted mode", args)
	}
}

// hasFlag reports whether args contains flag immediately followed by value.
func hasFlag(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
