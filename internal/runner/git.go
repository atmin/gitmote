package runner

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// GitCloner clones over the normal git-HTTP path with a scoped token — the same
// authenticated path any client uses, so there is no CI-only backdoor in the git
// handler (tasks/16-ci.md Stage 0 #4).
type GitCloner struct{}

// Clone fetches repo into dest and checks out the exact sha. It authenticates by
// embedding the token as the HTTP password (the username is ignored by the guard
// when a password is present). When ref names a branch, the SHA is checked out
// onto that branch so the engine can resolve github.ref; otherwise it is a
// detached checkout.
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
	// Check out the pushed SHA precisely — the branch tip may have moved on by
	// the time the runner clones (a newer push), so never trust HEAD. Put it on
	// the pushed branch (checkout -B) so the workflow sees a real ref, not a
	// detached HEAD.
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

// git runs a git command in dir (repo root; "" for none), returning combined
// output. GIT_TERMINAL_PROMPT=0 makes a bad credential fail fast instead of
// hanging on a prompt; an empty credential.helper ignores any host helper.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-c", "credential.helper="}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd.CombinedOutput()
}
