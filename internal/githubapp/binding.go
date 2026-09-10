// Package githubapp receives GitHub App deliveries and turns a verified one into an authorised command.
package githubapp

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Binding is one target a repository may reach, from one directory in it or, with an empty Dir, from any.
type Binding struct {
	Target string
	Dir    string
}

// Bindings are the targets one repository may reach, by target name.
type Bindings []Binding

func bind(stored map[string]string, repository string) Bindings {
	var out Bindings
	for _, target := range slices.Sorted(maps.Keys(stored)) {
		for _, entry := range strings.Split(stored[target], ",") {
			repo, dir, _ := strings.Cut(strings.TrimSpace(entry), ":")
			if repo == repository {
				out = append(out, Binding{Target: target, Dir: dir})
			}
		}
	}

	return out
}

// Targets names the bound targets, for a log line; it never reaches the repository.
func (b Bindings) Targets() []string {
	out := make([]string, 0, len(b))
	for _, e := range b {
		if !slices.Contains(out, e.Target) {
			out = append(out, e.Target)
		}
	}

	return out
}

// Grant answers whether this repository may reach the target its godwit.yaml named, from dir. The refusal
// reads the same for a target bound elsewhere and for one that does not exist: a caller with no ListTargets
// must not be handed one.
func (b Bindings) Grant(repository, dir, target string) error {
	for _, e := range b {
		if e.Target == target && (e.Dir == "" || e.Dir == dir) {
			return nil
		}
	}

	return fmt.Errorf("repository %s is not bound to a target named %q: ask a godwit operator to bind it "+
		"(godwit target add %s --github-repo %s)", repository, target, target, bindSpec(repository, dir))
}

func bindSpec(repository, dir string) string {
	if dir == "" {
		return repository
	}

	return repository + ":" + dir
}
