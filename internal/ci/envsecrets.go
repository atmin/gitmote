package ci

import (
	"sort"
	"strings"
)

// envSecretPrefix is the process-env prefix that seeds a repo's CI secrets
// without the UI/DB path: GITMOTE_REPO_SECRET_<REPO>__<NAME>=value. It is
// deliberately distinct from the runner-facing secretEnvPrefix. These values are
// operator-supplied host config — never persisted, never encrypted, and needing
// no master key (GITMOTE_CI_SECRET_KEY_V<n>): the host env is already the CI
// trust boundary (docs/architecture/safety.md §7). This is what lets the
// fully-automated self-deploy keep its secrets in a gitignored .env instead of
// the Secrets UI (docs/ops.md).
const envSecretPrefix = "GITMOTE_REPO_SECRET_"

// EnvSecrets maps a normalized repo key (see repoKey) to that repo's env-seeded
// CI secrets (name→value). Build it once at boot with LoadEnvSecrets; resolve it
// per dispatch with For.
type EnvSecrets map[string]map[string]string

// LoadEnvSecrets parses the GITMOTE_REPO_SECRET_<REPO>__<NAME> entries out of
// environ (os.Environ() form, "KEY=VALUE"). Entries without the prefix, without
// the "__" repo/name delimiter, or with an empty repo or name are ignored. The
// repo segment is normalized with repoKey so it matches For(repoName) regardless
// of the casing written in the env; the name is kept verbatim (secret names are
// case-sensitive, matching ${{ secrets.NAME }}). A later duplicate key wins.
func LoadEnvSecrets(environ []string) EnvSecrets {
	out := EnvSecrets{}
	for _, kv := range environ {
		key, val, _ := strings.Cut(kv, "=")
		rest, ok := strings.CutPrefix(key, envSecretPrefix)
		if !ok {
			continue
		}
		repo, name, ok := strings.Cut(rest, "__")
		if !ok || repo == "" || name == "" {
			continue
		}
		rk := repoKey(repo)
		m := out[rk]
		if m == nil {
			m = map[string]string{}
			out[rk] = m
		}
		m[name] = val
	}
	return out
}

// For returns the env-seeded secrets for repoName, or nil if none. repoName is
// normalized to the env-key form (uppercased, '-' → '_'), so a repo "my-repo"
// matches GITMOTE_REPO_SECRET_MY_REPO__NAME. A repo name containing "__" can't be
// addressed this way; a single '_' is fine.
func (e EnvSecrets) For(repoName string) map[string]string {
	return e[repoKey(repoName)]
}

// Redacted returns repoKey→sorted secret names, values omitted — safe to log at
// boot to confirm what was seeded without ever printing a value. Nil when empty.
func (e EnvSecrets) Redacted() map[string][]string {
	if len(e) == 0 {
		return nil
	}
	out := make(map[string][]string, len(e))
	for repo, m := range e {
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		out[repo] = names
	}
	return out
}

// repoKey normalizes a repo name to its env-key form: uppercased with '-' → '_',
// matching how a repo is spelled in a GITMOTE_REPO_SECRET_<REPO>__… var.
func repoKey(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}
