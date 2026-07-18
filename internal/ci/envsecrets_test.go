package ci

import (
	"reflect"
	"testing"
)

func TestLoadEnvSecretsParsesPrefixedEntries(t *testing.T) {
	env := []string{
		"GITMOTE_REPO_SECRET_GITMOTE__SCW_SECRET_KEY=uuid-here",
		"GITMOTE_REPO_SECRET_GITMOTE__GHCR_TOKEN=ghp_abc",
		"GITMOTE_REPO_SECRET_OTHER__API_TOKEN=t0ken",
		"PATH=/usr/bin", // unrelated — ignored
	}
	got := LoadEnvSecrets(env)
	want := EnvSecrets{
		"GITMOTE": {"SCW_SECRET_KEY": "uuid-here", "GHCR_TOKEN": "ghp_abc"},
		"OTHER":   {"API_TOKEN": "t0ken"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadEnvSecrets = %#v, want %#v", got, want)
	}
}

func TestLoadEnvSecretsIgnoresMalformed(t *testing.T) {
	// The failure path: entries missing the prefix, the "__" delimiter, or a repo
	// or name segment must be dropped, never mis-parsed into a bogus secret.
	env := []string{
		"GITMOTE_REPO_SECRET_NOOP",              // no '='/value → whole thing is the key, no "__"
		"GITMOTE_REPO_SECRET_NODELIM=v",         // no "__" between repo and name
		"GITMOTE_REPO_SECRET___NONAME=v",        // empty name after "__"
		"GITMOTE_REPO_SECRET___=v",              // empty repo and name
		"NOT_THE_PREFIX_GITMOTE__SCW=v",         // wrong prefix
		"GITMOTE_REPO_SECRET_GITMOTE__OK=value", // the one valid entry
	}
	got := LoadEnvSecrets(env)
	want := EnvSecrets{"GITMOTE": {"OK": "value"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadEnvSecrets = %#v, want %#v", got, want)
	}
}

func TestEnvSecretsForNormalizesRepoName(t *testing.T) {
	e := LoadEnvSecrets([]string{"GITMOTE_REPO_SECRET_MY_REPO__A=1"})
	// A repo named "my-repo" ('-'→'_', uppercased) must resolve the MY_REPO key.
	if got := e.For("my-repo"); !reflect.DeepEqual(got, map[string]string{"A": "1"}) {
		t.Errorf("For(my-repo) = %#v, want {A:1}", got)
	}
	if got := e.For("absent"); got != nil {
		t.Errorf("For(absent) = %#v, want nil", got)
	}
}

func TestEnvSecretsRedactedOmitsValues(t *testing.T) {
	e := LoadEnvSecrets([]string{
		"GITMOTE_REPO_SECRET_GITMOTE__B=2",
		"GITMOTE_REPO_SECRET_GITMOTE__A=1",
	})
	got := e.Redacted()
	want := map[string][]string{"GITMOTE": {"A", "B"}} // sorted, names only
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Redacted = %#v, want %#v", got, want)
	}
	if EnvSecrets(nil).Redacted() != nil {
		t.Error("Redacted of empty must be nil")
	}
}
