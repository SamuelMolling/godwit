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

// Command is one verified, de-duplicated and authorised delivery, resolved to what the App would run.
type Command struct {
	Delivery     string
	Event        string
	Repository   string
	Installation int64
	Number       int
	// Head is resolved live at the moment of the command; the approval and the run both pin to it.
	Head      string
	Login     string
	Principal api.Principal
	// Bindings are the targets this repository may reach; the name godwit.yaml asks for is checked against them.
	Bindings Bindings
	Name     string
	Comment  *comment.Command
	// Source is written from the verified payload, so the provenance revert trusts is not caller-supplied.
	Source string
}

// Detail is the audit line: what would have run, who asked and which delivery carried it.
func (c Command) Detail() string {
	parts := []string{
		"command=" + c.Name,
		"event=" + c.Event,
		"pr=" + fmt.Sprint(c.Number),
		"head=" + c.Head,
		"scope=" + string(c.Principal.Scope),
		"delivery=" + c.Delivery,
		"bound=" + strings.Join(c.Bindings.Targets(), "|"),
	}
	if c.Login != "" {
		parts = append(parts, "login="+c.Login)
	}

	return strings.Join(parts, " ")
}

// Runner is the seam this receiver ends at, above verification, de-duplication and authorisation.
type Runner interface {
	Enqueue(ctx context.Context, tx Tx, cmd Command) error
}

// Recorder is the Runner the App carries until it can fetch migrations: it records what it would have run.
type Recorder struct {
	Log *slog.Logger
}

// Enqueue implements Runner.
func (r Recorder) Enqueue(ctx context.Context, tx Tx, cmd Command) error {
	r.Log.Info("webhook command accepted", "repository", cmd.Repository, "command", cmd.Name,
		"delivery", cmd.Delivery, "login", cmd.Login, "head", cmd.Head, "actor", cmd.Principal.Name)

	return tx.Audit(ctx, controlplane.AuditEntry{
		Actor:  cmd.Principal.Name,
		Action: controlplane.AuditWebhookCommand,
		Detail: cmd.Detail(),
	})
}
