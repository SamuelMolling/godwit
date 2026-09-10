package api

import (
	"context"
	"errors"
	"fmt"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/notify"
)

// reattach finds the run an earlier job with the same files and rollout created and, when the target still
// matches what that run knew, returns it so the caller follows it instead of queueing a duplicate.
// An explicit plan id is an explicit ask and skips it.
func (s *Server) reattach(ctx context.Context, m *godwitv1.CreateRunRequest, spec runSpec, obs controlplane.Observation) (controlplane.Run, bool, error) {
	if m.PlanId != "" {
		return controlplane.Run{}, false, nil
	}
	plan, err := s.store.BoundPlan(ctx, m.Target, spec.rollout, controlplane.FilesHash(spec.files))
	if errors.Is(err, controlplane.ErrNotFound) {
		return controlplane.Run{}, false, nil
	}
	if err != nil {
		return controlplane.Run{}, false, rpcErr(err)
	}
	run, err := s.store.Run(ctx, plan.RunID)
	if err != nil {
		return controlplane.Run{}, false, rpcErr(err)
	}
	switch run.State {
	case controlplane.StateQueued, controlplane.StateRunning, controlplane.StateAwaitingContract:
		s.joined(ctx, run, plan, false)
	case controlplane.StateSucceeded:
		d, err := s.attribute(ctx, plan, obs)
		if err != nil {
			return run, false, rpcErr(err)
		}
		if d.Removed = unapplied(plan, run, obs); len(d.Removed) > 0 {
			return run, false, s.refuse(ctx, m.Target, &controlplane.PlanStale{
				Plan: plan, Reason: controlplane.StaleHistory, Diff: d,
				Hint: fmt.Sprintf("run %s applied %d migrations the target no longer has; push to the pull request (re-plan) after checking who changed godwit.migrations on %s", notify.ShortID(run.ID), len(d.Removed), m.Target),
			})
		}
		s.joined(ctx, run, plan, false)
	case controlplane.StateFailed, controlplane.StateNeedsAttention:
		if err := s.resumeFresh(ctx, m, spec, plan, run, obs); err != nil {
			return run, false, err
		}
	default:
		if err := s.store.RetirePlan(ctx, plan.ID); err != nil {
			return run, false, rpcErr(err)
		}

		return run, false, nil
	}

	return run, true, nil
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
func (s *Server) resumeFresh(ctx context.Context, m *godwitv1.CreateRunRequest, spec runSpec, plan controlplane.Plan, run controlplane.Run, obs controlplane.Observation) error {
	d, err := s.attribute(ctx, plan, obs)
	if err != nil {
		return rpcErr(err)
	}
	own := map[int64]bool{}
	for _, p := range spec.plans {
		own[p.Migration.Version] = true
	}
	stale := len(d.Removed) > 0
	for _, c := range d.Added {
		stale = stale || (c.RunID == "" && !own[c.Version])
	}
	if stale {
		return s.refuse(ctx, m.Target, &controlplane.PlanStale{Plan: plan, Reason: controlplane.StaleHistory, Diff: d, Hint: staleHint(controlplane.StaleHistory, m.Target)})
	}
	if _, err := s.admit(ctx, m.Target, spec.plans, union(plan.Acked, m.AcknowledgeHazards), m.SkipValidation,
		plan.AllowOutOfOrder || m.AllowOutOfOrder, obs.SearchPath); err != nil {
		return s.replanFailure(ctx, plan, d, err)
	}
	if _, err := s.queue(ctx, notify.RunResumed, "re-attached by a pipeline re-run", func(tx *controlplane.Store) (controlplane.Run, error) {
		return tx.Resume(ctx, run.ID)
	}); err != nil {
		return rpcErr(err)
	}
	s.Metrics.RunResumed(run.Target)
	s.joined(ctx, run, plan, true)

	return nil
}

func (s *Server) joined(ctx context.Context, run controlplane.Run, plan controlplane.Plan, resumed bool) {
	s.Log.Info("run re-attached", "run", run.ID, "target", run.Target, "state", run.State, "plan", plan.ID, "resumed", resumed)
	s.audit(ctx, controlplane.AuditRunReattach, run.ID, run.Target, fmt.Sprintf("state=%s plan=%s resumed=%t", run.State, plan.ID, resumed))
}
