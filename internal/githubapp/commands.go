package githubapp

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/authz"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func (w *Worker) apply(ctx context.Context, cmd command, p project, files []*godwitv1.MigrationFile) done {
	if err := authz.Authorize(godwitv1connect.GodwitServiceCreateRunProcedure, cmd.principal); err != nil {
		return refusedBy(cmd, p, err)
	}
	res, err := w.cfg.Service.CreateRun(authz.WithPrincipal(ctx, cmd.principal), connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: p.target, Files: files, AcknowledgeHazards: cmd.acked(), Rollout: cmd.rollout(p),
		AllowOutOfOrder: p.outOfOrder, Source: cmd.sourceOf(p),
	}))
	if err != nil {
		return refusedBy(cmd, p, err)
	}

	return queued(cmd, p, res.Msg.GetRunId(), bindLine(res.Msg))
}

func (w *Worker) confirm(ctx context.Context, cmd command, p project) done {
	if err := authz.Authorize(godwitv1connect.GodwitServiceConfirmRolloutProcedure, cmd.principal); err != nil {
		return refusedBy(cmd, p, err)
	}
	held, err := w.pick(ctx, cmd, p, func(g controlplane.GitHubRun) bool {
		return g.State == controlplane.StateAwaitingContract
	})
	if err != nil {
		return refusedBy(cmd, p, err)
	}
	if held == "" {
		return refusedBy(cmd, p, fmt.Errorf(
			"no run of pull request #%d on %s is awaiting its contract phase", cmd.number, p.target))
	}
	if _, err := w.cfg.Service.ConfirmRollout(authz.WithPrincipal(ctx, cmd.principal),
		connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: held})); err != nil {
		return refusedBy(cmd, p, err)
	}

	return queued(cmd, p, held, "contract phase released")
}

// revert undoes the newest un-reverted run of this pull request. The Action loops over every one of them
// oldest first, which refuses on the second without --force, so the two agree wherever the Action works.
func (w *Worker) revert(ctx context.Context, cmd command, p project) done {
	if err := authz.Authorize(godwitv1connect.GodwitServiceRevertRunProcedure, cmd.principal); err != nil {
		return refusedBy(cmd, p, err)
	}
	undo, err := w.pick(ctx, cmd, p, revertable)
	if err != nil {
		return refusedBy(cmd, p, err)
	}
	if undo == "" {
		return done{
			project: p, conclusion: conclusionSuccess, title: "nothing to revert",
			body: fmt.Sprintf("## godwit revert\n\nNo run of pull request #%d on `%s` is left to revert.\n",
				cmd.number, p.target),
		}
	}
	res, err := w.cfg.Service.RevertRun(authz.WithPrincipal(ctx, cmd.principal), connect.NewRequest(&godwitv1.RevertRunRequest{
		RunId: undo, Target: p.target, AcknowledgeHazards: cmd.acked(),
		Force: cmd.force(), AllowDataLoss: cmd.allowDataLoss(),
	}))
	if err != nil {
		return refusedBy(cmd, p, err)
	}

	return queued(cmd, p, res.Msg.GetRunId(), "reverting run `"+undo+"`")
}

func revertable(g controlplane.GitHubRun) bool {
	if g.Reverts != "" {
		return false
	}

	return g.State == controlplane.StateSucceeded || g.State == controlplane.StateAwaitingContract ||
		g.State == controlplane.StateFailed || g.State == controlplane.StateNeedsAttention
}

func (w *Worker) pick(ctx context.Context, cmd command, p project, want func(controlplane.GitHubRun) bool) (string, error) {
	all, err := w.cfg.Runs.GitHubRunsOf(ctx, cmd.repository, cmd.number, p.target)
	if err != nil {
		return "", err
	}
	for _, g := range all {
		if want(g) {
			return g.RunID, nil
		}
	}

	return "", nil
}

// queued leaves the check open: the pull request is owed the run's outcome, which it does not have yet.
func queued(cmd command, p project, run, detail string) done {
	if run == "" {
		return refusedBy(cmd, p, errors.New("godwit created no run"))
	}

	return done{
		project: p, run: run, title: "godwit " + cmd.name + " queued",
		body: fmt.Sprintf("## godwit %s\n\nRun `%s` is queued on `%s` — %s.\n", cmd.name, run, p.target, detail),
	}
}

func bindLine(res *godwitv1.CreateRunResponse) string {
	switch {
	case res.GetReattached():
		return "re-attached to a run already carrying these migrations"
	case res.GetPlanId() == "":
		return "no stored plan matched this set, so the plan is implicit"
	default:
		return "bound to plan `" + res.GetPlanId() + "`"
	}
}

func (c command) acked() []string {
	if c.cmd == nil {
		return nil
	}

	return c.cmd.Ack
}

func (c command) force() bool { return c.cmd != nil && c.cmd.Force }

func (c command) allowDataLoss() bool { return c.cmd != nil && c.cmd.AllowDataLoss }
