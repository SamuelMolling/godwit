package controlplane

import (
	"context"
	"errors"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// TargetStatus is what the control plane knows about a target next to what its database reports.
type TargetStatus struct {
	Target    string
	Provider  string
	Timeouts  Timeouts
	Applied   []engine.Applied
	LastRun   *Run
	Snapshot  *Snapshot
	OpenDrift bool
}

// Inspector reads a target's applied versions, last run and drift baseline without changing anything.
type Inspector struct {
	sched *Scheduler
}

// NewInspector shares the scheduler's store, credential providers and engine.
func NewInspector(sched *Scheduler) *Inspector {
	return &Inspector{sched: sched}
}

// Status resolves the target's credentials, lists what its database has applied and adds the control plane's view.
func (i *Inspector) Status(ctx context.Context, name string) (TargetStatus, error) {
	tg, err := i.sched.target(ctx, name)
	if err != nil {
		return TargetStatus{}, err
	}
	applied, err := i.sched.engine.Applied(ctx, tg.dsn)
	if err != nil {
		return TargetStatus{}, err
	}
	st := TargetStatus{Target: name, Provider: tg.provider, Timeouts: tg.timeouts, Applied: applied}

	last, ok, err := i.sched.store.LastRun(ctx, name)
	if err != nil {
		return TargetStatus{}, err
	}
	if ok {
		st.LastRun = &last
	}
	snap, err := i.sched.store.SnapshotFor(ctx, name)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return TargetStatus{}, err
	default:
		st.Snapshot = &snap
	}
	if st.OpenDrift, err = i.sched.store.OpenDrift(ctx, name); err != nil {
		return TargetStatus{}, err
	}

	return st, nil
}
