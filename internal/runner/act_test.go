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
		"GITMOTE_URL=http://x", // not a secret; must be ignored
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
	// Just the workflows flag — no -s, no -P (local dev: act's default nesting).
	if hasFlag(args, "-P", "") || slices.Contains(args, "-s") {
		t.Errorf("args = %v, want only --workflows with no overrides", args)
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
