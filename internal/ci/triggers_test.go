package ci

import "testing"

func TestParsePushTriggerMatches(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		branch  string
		want    bool
		wantErr bool
	}{
		{"bare push scalar", "on: push\n", "main", true, false},
		{"push in sequence", "on: [push, workflow_dispatch]\n", "anything", true, false},
		{"no push in sequence", "on: [workflow_dispatch]\n", "main", false, false},
		{"push map no filter", "on:\n  push:\n", "main", true, false},
		{"branch allow match", "on:\n  push:\n    branches: [self-deploy]\n", "self-deploy", true, false},
		{"branch allow miss", "on:\n  push:\n    branches: [self-deploy]\n", "main", false, false},
		{"branch scalar form", "on:\n  push:\n    branches: main\n", "main", true, false},
		{"glob star matches segment", "on:\n  push:\n    branches: ['feature/*']\n", "feature/x", true, false},
		{"glob star not across slash", "on:\n  push:\n    branches: ['feature/*']\n", "feature/x/y", false, false},
		{"glob doublestar across slash", "on:\n  push:\n    branches: ['release/**']\n", "release/a/b", true, false},
		{"branches-ignore excludes", "on:\n  push:\n    branches-ignore: [wip]\n", "wip", false, false},
		{"branches-ignore allows other", "on:\n  push:\n    branches-ignore: [wip]\n", "main", true, false},
		{"dispatch only, no push", "on:\n  workflow_dispatch:\n", "main", false, false},
		{"malformed yaml errors", "name: x\n\t- : broken:\n", "main", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			trig, err := parsePushTrigger([]byte(tc.yaml))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parse %q: want error, got none", tc.yaml)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %q: %v", tc.yaml, err)
			}
			if got := trig.matches(tc.branch); got != tc.want {
				t.Errorf("matches(%q) = %v, want %v", tc.branch, got, tc.want)
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"main", "main", true},
		{"main", "master", false},
		{"self-deploy", "self-deploy", true}, // literal hyphen, not a regex range
		{"feature/*", "feature/login", true},
		{"feature/*", "feature/login/deep", false},
		{"**", "any/deep/branch", true},
		{"release/**", "release/1.2/rc", true},
		{"v*.*", "v1.2", true},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}
