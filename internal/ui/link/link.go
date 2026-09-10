// Package link composes the UI addresses that leave the process: Slack buttons, pull-request reports.
package link

import "strings"

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
