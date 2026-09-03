package controlplane

import (
	"context"
	"errors"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// TargetStatus is what the control plane knows about a target next to what its database reports.
type TargetStatus struct {
	Target      string
	Provider    string
	Timeouts    Timeouts
	SearchPath  string
	Applied     []engine.Applied
	Repeatables []engine.Repeatable
	LastRun     *Run
	Snapshot    *Snapshot
	OpenDrift   bool
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
	applied, reps, err := i.sched.engine.Applied(ctx, tg.dsn)
	if err != nil {
		return TargetStatus{}, err
	}
	st := TargetStatus{
		Target: name, Provider: tg.provider, Timeouts: tg.timeouts, SearchPath: tg.searchPath,
		Applied: applied, Repeatables: reps,
	}

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

// Observe reads the target's live history and schema over one connection.
func (i *Inspector) Observe(ctx context.Context, name string) (Observation, error) {
	tg, err := i.sched.target(ctx, name)
	if err != nil {
		return Observation{}, err
	}

	return i.sched.engine.Observe(ctx, tg.dsn)
}

// DataLoss reports which of the drops would destroy data the target still holds.
func (i *Inspector) DataLoss(ctx context.Context, name string, drops []engine.Drop) ([]engine.Loss, error) {
	tg, err := i.sched.target(ctx, name)
	if err != nil {
		return nil, err
	}

	return i.sched.engine.DataLoss(ctx, tg.dsn, drops)
}
