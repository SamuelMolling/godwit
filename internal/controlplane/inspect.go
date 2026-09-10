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
	// Unreachable is why the target's own journal was not read, when its credential does not resolve.
	Unreachable string
}

// Inspector reads a target's applied versions, last run and drift baseline without changing anything.
type Inspector struct {
	sched *Scheduler
}

// NewInspector shares the scheduler's store, credential providers and engine.
func NewInspector(sched *Scheduler) *Inspector {
	return &Inspector{sched: sched}
}

// Status lists what the target's database has applied next to the control plane's view, describing rather than refusing a target whose credential does not resolve.
func (i *Inspector) Status(ctx context.Context, name string) (TargetStatus, error) {
	provider, config, err := i.sched.store.Target(ctx, name)
	if err != nil {
		return TargetStatus{}, err
	}
	st := TargetStatus{
		Target: name, Provider: provider,
		Timeouts: TargetTimeouts(config), SearchPath: config[ConfigSearchPath],
	}
	tg, err := i.sched.resolve(ctx, name, provider, config)
	if err != nil {
		st.Unreachable = err.Error()
	} else if st.Applied, st.Repeatables, err = i.sched.engine.Applied(ctx, tg.dsn); err != nil {
		return TargetStatus{}, err
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

	return i.sched.engine.Observe(ctx, tg.dsn, tg.scope)
}

// DataLoss reports which of the drops would destroy data the target still holds.
func (i *Inspector) DataLoss(ctx context.Context, name string, drops []engine.Drop) ([]engine.Loss, error) {
	tg, err := i.sched.target(ctx, name)
	if err != nil {
		return nil, err
	}

	return i.sched.engine.DataLoss(ctx, tg.dsn, drops)
}
