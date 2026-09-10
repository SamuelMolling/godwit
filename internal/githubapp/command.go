package githubapp

import (
	"context"
	"fmt"
	"strings"

	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/comment"
)

// scopes hold nothing above pipeline, so no webhook can reach the RPC that binds a repository to a target.
var scopes = map[string]api.Scope{
	"plan":    api.ScopeRead,
	"apply":   api.ScopePipeline,
	"confirm": api.ScopePipeline,
	"revert":  api.ScopePipeline,
}

type command struct {
	delivery     string
	event        string
	repository   string
	repositoryID int64
	installation int64
	number       int
	head         string
	login        string
	principal    api.Principal
	bound        bindings
	name         string
	cmd          *comment.Command
	projects     []project
	source       string
}

func (c command) projectNames() string {
	out := make([]string, 0, len(c.projects))
	for _, p := range c.projects {
		out = append(out, p.String())
	}

	return strings.Join(out, ", ")
}

func (c command) detail() string {
	parts := []string{
		"command=" + c.name,
		"event=" + c.event,
		"pr=" + fmt.Sprint(c.number),
		"head=" + c.head,
		"scope=" + string(c.principal.Scope),
		"delivery=" + c.delivery,
		"bound=" + strings.Join(c.bound.targets(), "|"),
		"projects=" + c.projectNames(),
	}
	if c.login != "" {
		parts = append(parts, "login="+c.login)
	}

	return strings.Join(parts, " ")
}

type runner interface {
	enqueue(ctx context.Context, tx txn, cmd command) error
}
