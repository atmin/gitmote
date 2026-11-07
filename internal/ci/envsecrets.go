package ci

import (
	"sort"
	"strings"
)

// envSecretPrefix is the process-env prefix that seeds the CI secrets of a
// repo without the UI and DB path: GITMOTE_REPO_SECRET_<REPO>__<NAME>=value.
// It is deliberately distinct from secretEnvPrefix, which the runner sees.
// These values are host config that the operator supplies. They are never
// persisted and never encrypted. They need no master key
// (GITMOTE_CI_SECRET_KEY_V<n>), because the host env is already the CI trust
// boundary (docs/architecture/safety.md §5). With this prefix, the fully
// automated self-deploy keeps its secrets in a gitignored .env instead of the
// Secrets UI (docs/deploy-scaleway.md).
const envSecretPrefix = "GITMOTE_REPO_SECRET_"

// EnvSecrets maps a normalized repo key (see repoKey) to the env-seeded CI
// secrets of that repo. The secrets are a map from name to value. Build it
// once at boot with LoadEnvSecrets. Resolve it per dispatch with For.
type EnvSecrets map[string]map[string]string

// LoadEnvSecrets parses the GITMOTE_REPO_SECRET_<REPO>__<NAME> entries out of
// environ, which is in the os.Environ() form "KEY=VALUE". It ignores these
// entries:
//   - an entry without the prefix
//   - an entry without the "__" delimiter between repo and name
//   - an entry with an empty repo or name
//
// repoKey normalizes the repo segment, so it matches For(repoName) whatever
// the casing in the env. The name stays verbatim, because secret names are
// case-sensitive, like ${{ secrets.NAME }}. A later duplicate key wins.
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

// For returns the env-seeded secrets for repoName, or nil if there are none.
// For normalizes repoName to the env-key form: uppercased, with '-' replaced
// by '_'. So a repo "my-repo" matches GITMOTE_REPO_SECRET_MY_REPO__NAME. A
// repo name that contains "__" can't be addressed this way. A single '_' is
// fine.
func (e EnvSecrets) For(repoName string) map[string]string {
	return e[repoKey(repoName)]
}

// Redacted returns a map from repoKey to the sorted secret names, without the
// values. It is safe to log at boot. The log confirms what was seeded without
// ever printing a value. Redacted returns nil when e is empty.
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

// repoKey normalizes a repo name to its env-key form: uppercased, with '-'
// replaced by '_'. This matches how a repo is spelled in a
// GITMOTE_REPO_SECRET_<REPO>__… var.
func repoKey(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}
