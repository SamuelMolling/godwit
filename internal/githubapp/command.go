package githubapp

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/SamuelMolling/godwit/internal/api"
	"github.com/SamuelMolling/godwit/internal/comment"
	"github.com/SamuelMolling/godwit/internal/controlplane"
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
	installation int64
	number       int
	head         string
	login        string
	principal    api.Principal
	bound        bindings
	name         string
	cmd          *comment.Command
	source       string
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
	}
	if c.login != "" {
		parts = append(parts, "login="+c.login)
	}

	return strings.Join(parts, " ")
}

type runner interface {
	enqueue(ctx context.Context, tx txn, cmd command) error
}

type recorder struct {
	log *slog.Logger
}

func (r recorder) enqueue(ctx context.Context, tx txn, cmd command) error {
	r.log.Info("webhook command accepted", "repository", cmd.repository, "command", cmd.name,
		"delivery", cmd.delivery, "login", cmd.login, "head", cmd.head, "actor", cmd.principal.Name)

	return tx.Audit(ctx, controlplane.AuditEntry{
		Actor:  cmd.principal.Name,
		Action: controlplane.AuditWebhookCommand,
		Detail: cmd.detail(),
	})
}
