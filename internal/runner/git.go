package runner

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// GitCloner clones over the normal git-HTTP path with a scoped token. This is
// the same authenticated path that any client uses. So the git handler has no
// CI-only backdoor.
type GitCloner struct{}

// Clone fetches repo into dest and checks out the exact sha. It authenticates
// by embedding the token as the HTTP password. The guard ignores the username
// when a password is present. When ref names a branch, Clone checks out the
// SHA onto that branch, so the engine can resolve github.ref. Otherwise the
// checkout is detached.
func (GitCloner) Clone(ctx context.Context, baseURL, repo, sha, ref, token, dest string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("parse base url %q: %w", baseURL, err)
	}
	u.User = url.UserPassword("x-access-token", token)
	u.Path = "/" + repo

	if out, err := git(ctx, "", "clone", "--quiet", u.String(), dest); err != nil {
		return fmt.Errorf("git clone: %w: %s", err, out)
	}
	// Check out exactly the pushed SHA. A newer push may move the branch tip
	// before the runner clones, so never trust HEAD. Put the SHA on the pushed
	// branch (checkout -B), so the workflow sees a real ref, not a detached
	// HEAD.
	if branch := strings.TrimPrefix(ref, "refs/heads/"); branch != "" && branch != ref {
		if out, err := git(ctx, dest, "checkout", "--quiet", "-B", branch, sha); err != nil {
			return fmt.Errorf("git checkout -B %s %s: %w: %s", branch, sha, err, out)
		}
		return nil
	}
	if out, err := git(ctx, dest, "-c", "advice.detachedHead=false", "checkout", "--quiet", sha); err != nil {
		return fmt.Errorf("git checkout %s: %w: %s", sha, err, out)
	}
	return nil
}

// git runs a git command in dir and returns the combined output. dir is the
// repo root, or "" for none. GIT_TERMINAL_PROMPT=0 makes a bad credential fail
// fast instead of hanging on a prompt. An empty credential.helper ignores any
// host helper.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-c", "credential.helper="}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd.CombinedOutput()
}
