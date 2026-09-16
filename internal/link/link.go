// Package link composes the UI addresses that leave the process; it sits outside ui and imports nothing of godwit's, because ui already reaches notify and could not be called back from it.
package link

import (
	"regexp"
	"strings"
)

// Run is the run's page under a public base URL, empty when there is no base to build it from.
func Run(base, id string) string {
	return page(base, "runs", id)
}

// Plan is the plan's page under a public base URL, empty when there is no base to build it from.
func Plan(base, id string) string {
	return page(base, "plans", id)
}

func page(base, kind, id string) string {
	if base == "" || id == "" {
		return ""
	}

	return strings.TrimRight(base, "/") + "/ui/" + kind + "/" + id
}

// Commit is the provenance a run or plan records, as the Action and the App write it: <host>/<owner>/<repo>@<sha>[:<dir>].
type Commit struct {
	Repo  string
	Short string
	Dir   string
	Href  string
}

var commitRe = regexp.MustCompile(`^([a-zA-Z0-9.-]+/[^/@\s]+/[^/@\s]+)@([0-9a-fA-F]{7,40})(?::(.*))?$`)

// CommitOf reads that provenance, and returns the zero Commit for a source it cannot read.
func CommitOf(source string) Commit {
	m := commitRe.FindStringSubmatch(source)
	if m == nil {
		return Commit{}
	}

	return Commit{Repo: m[1], Short: m[2][:7], Dir: m[3], Href: "https://" + m[1] + "/commit/" + m[2]}
}
