package admission

import (
	"context"
	"errors"
	"fmt"

	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/notify"
)

// reattach finds the run an earlier job with the same files and rollout created and, when the target still
// matches what that run knew, returns it so the caller follows it instead of queueing a duplicate.
// An explicit plan id is an explicit ask and skips it.
func (g Gate) reattach(ctx context.Context, req Request, set Set, obs controlplane.Observation) (*Reattach, error) {
	if req.PlanID != "" {
		return nil, nil
	}
	plan, err := g.Store.BoundPlan(ctx, req.Target, set.Rollout, controlplane.FilesHash(set.Files))
	if errors.Is(err, controlplane.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run, err := g.Store.Run(ctx, plan.RunID)
	if err != nil {
		return nil, err
	}
	switch run.State {
	case controlplane.StateQueued, controlplane.StateRunning, controlplane.StateAwaitingContract:
		return &Reattach{Run: run, Plan: plan}, nil
	case controlplane.StateSucceeded:
		d, err := g.attribute(ctx, plan, obs)
		if err != nil {
			return nil, err
		}
		if d.Removed = unapplied(plan, run, obs); len(d.Removed) > 0 {
			return nil, g.refuse(ctx, req.Target, &controlplane.PlanStale{
				Plan: plan, Reason: controlplane.StaleHistory, Diff: d,
				Hint: fmt.Sprintf("run %s applied %d migrations the target no longer has; push to the pull request (re-plan) after checking who changed godwit.migrations on %s", notify.ShortID(run.ID), len(d.Removed), req.Target),
			})
		}

		return &Reattach{Run: run, Plan: plan}, nil
	case controlplane.StateFailed, controlplane.StateNeedsAttention:
		if err := g.resumable(ctx, req, set, plan, obs); err != nil {
			return nil, err
		}

		return &Reattach{Run: run, Plan: plan, Resume: true}, nil
	default:
		if err := g.Store.RetirePlan(ctx, plan.ID); err != nil {
			return nil, err
		}

		return nil, nil
	}
}

func unapplied(plan controlplane.Plan, run controlplane.Run, obs controlplane.Observation) []controlplane.HistoryChange {
	have := map[int64]bool{}
	for _, a := range obs.Applied {
		have[a.Version] = true
	}
	var out []controlplane.HistoryChange
	for _, pm := range plan.Pending() {
		if !have[pm.Version] {
			out = append(out, controlplane.HistoryChange{Version: pm.Version, Name: pm.Name, RunID: run.ID})
		}
	}

	return out
}

// A failed run's own partial progress shows up as history the plan did not know; it is expected, not drift.
func (g Gate) resumable(ctx context.Context, req Request, set Set, plan controlplane.Plan, obs controlplane.Observation) error {
	d, err := g.attribute(ctx, plan, obs)
	if err != nil {
		return err
	}
	own := map[int64]bool{}
	for _, p := range set.Plans {
		own[p.Migration.Version] = true
	}
	stale := len(d.Removed) > 0
	for _, c := range d.Added {
		stale = stale || (c.RunID == "" && !own[c.Version])
	}
	if stale {
		return g.refuse(ctx, req.Target, &controlplane.PlanStale{Plan: plan, Reason: controlplane.StaleHistory, Diff: d, Hint: staleHint(controlplane.StaleHistory, req.Target)})
	}
	if _, err := g.Admit(ctx, req.Target, set.Plans, union(plan.Acked, req.Acked), req.SkipValidation,
		plan.AllowOutOfOrder || req.AllowOutOfOrder, obs.SearchPath); err != nil {
		return g.replanFailure(ctx, plan, d, err)
	}

	return nil
}
