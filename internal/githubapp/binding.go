// Package githubapp receives GitHub App deliveries and turns a verified one into an authorised command.
package githubapp

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

type binding struct {
	target string
	dir    string
}

type bindings []binding

func bind(stored map[string]string, repository string) bindings {
	var out bindings
	for _, target := range slices.Sorted(maps.Keys(stored)) {
		for _, entry := range strings.Split(stored[target], ",") {
			repo, dir, _ := strings.Cut(strings.TrimSpace(entry), ":")
			if repo == repository {
				out = append(out, binding{target: target, dir: dir})
			}
		}
	}

	return out
}

func (b bindings) targets() []string {
	out := make([]string, 0, len(b))
	for _, e := range b {
		if !slices.Contains(out, e.target) {
			out = append(out, e.target)
		}
	}

	return out
}

// grant refuses identically for a target bound elsewhere and one that does not exist: a caller that holds no token has no ListTargets, and a refusal telling the two apart would hand it one.
func (b bindings) grant(repository, dir, target string) error {
	for _, e := range b {
		if e.target == target && (e.dir == "" || e.dir == dir) {
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
