package ci

import (
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// pushTrigger is the parsed push-event filter of a workflow's `on:` — the subset
// gitmote evaluates to decide whether a branch push starts a run. reacts is false
// when the workflow doesn't react to push at all (e.g. workflow_dispatch only),
// so a push never triggers it. branches / branchesIgnore are GitHub's branch
// globs; empty means "no branch constraint".
type pushTrigger struct {
	reacts         bool
	branches       []string
	branchesIgnore []string
}

// workflowOn decodes just a workflow's `on:` key. The explicit tag matters: YAML
// resolves a bare `on` scalar as the boolean true (YAML 1.1), so keying off a
// decoded map would miss it — a tagged struct field matches the literal `on`.
type workflowOn struct {
	On yaml.Node `yaml:"on"`
}

// parsePushTrigger extracts the push filter from a workflow's YAML. A file that
// doesn't parse returns an error (the caller keeps it so the run still surfaces
// the failure). A well-formed file that doesn't react to push returns
// reacts=false with a nil error.
func parsePushTrigger(content []byte) (pushTrigger, error) {
	var w workflowOn
	if err := yaml.Unmarshal(content, &w); err != nil {
		return pushTrigger{}, err
	}
	return interpretOn(&w.On), nil
}

// interpretOn reads the three shapes `on:` takes — a scalar (`on: push`), a
// sequence (`on: [push, …]`), or a mapping (`on: {push: {branches: […]}}`).
func interpretOn(on *yaml.Node) pushTrigger {
	switch on.Kind {
	case yaml.ScalarNode:
		return pushTrigger{reacts: on.Value == "push"}
	case yaml.SequenceNode:
		for _, n := range on.Content {
			if n.Kind == yaml.ScalarNode && n.Value == "push" {
				return pushTrigger{reacts: true}
			}
		}
		return pushTrigger{}
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			if on.Content[i].Value != "push" {
				continue
			}
			t := pushTrigger{reacts: true}
			if v := on.Content[i+1]; v.Kind == yaml.MappingNode {
				t.branches = seqStrings(mapValue(v, "branches"))
				t.branchesIgnore = seqStrings(mapValue(v, "branches-ignore"))
			}
			return t
		}
		return pushTrigger{}
	default:
		return pushTrigger{} // null or absent `on:` — nothing to react to
	}
}

// matches reports whether a push to branch should trigger this workflow: it must
// react to push, pass any `branches` allow-list, and clear any `branches-ignore`
// deny-list.
func (t pushTrigger) matches(branch string) bool {
	if !t.reacts {
		return false
	}
	if len(t.branches) > 0 && !anyGlob(t.branches, branch) {
		return false
	}
	if len(t.branchesIgnore) > 0 && anyGlob(t.branchesIgnore, branch) {
		return false
	}
	return true
}

// mapValue returns the value node for key in a mapping node, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// seqStrings reads a scalar or a sequence of scalars into a string slice (a
// branch filter may be written `branches: main` or `branches: [main, dev]`).
func seqStrings(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, e := range n.Content {
			if e.Kind == yaml.ScalarNode {
				out = append(out, e.Value)
			}
		}
		return out
	default:
		return nil
	}
}

func anyGlob(patterns []string, name string) bool {
	for _, p := range patterns {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

// globMatch reports whether name matches a GitHub branch-filter glob. It models
// the two wildcards that cover real branch filters — `*` (a run of any character
// except `/`) and `**` (any character including `/`) — and treats everything
// else literally. GitHub's rarer pattern features (`?`, `+`, `[]`, `!`) are not
// modeled: a pattern using them matches only its literal form.
func globMatch(pattern, name string) bool {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '*' {
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
			continue
		}
		b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
	}
	b.WriteByte('$')
	re, err := regexp.Compile(b.String())
	if err != nil {
		return pattern == name
	}
	return re.MatchString(name)
}
