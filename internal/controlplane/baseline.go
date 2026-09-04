package controlplane

import (
	"context"
	"fmt"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// Baseliner marks migrations applied on a target without running them and records that as a succeeded run.
type Baseliner struct {
	sched *Scheduler
}

// NewBaseliner shares the scheduler's store, credential providers and engine.
func NewBaseliner(sched *Scheduler) *Baseliner {
	return &Baseliner{sched: sched}
}

// Baseline marks migs applied on target, records run runID (with provenance p) holding the files of what
// it put on the books, and snapshots the schema for drift. Both sides are idempotent: a migration the
// target's journal already records is left alone, and one the ledger already stands on is not recorded twice.
func (b *Baseliner) Baseline(ctx context.Context, runID, target string, migs []engine.Migration, p Provenance) error {
	dsn, err := b.sched.targetDSN(ctx, target)
	if err != nil {
		return err
	}
	marked, err := b.sched.engine.MarkApplied(ctx, dsn, migs)
	if err != nil {
		return err
	}
	applied, err := b.sched.store.Applied(ctx, target)
	if err != nil {
		return err
	}
	record := make([]engine.Migration, 0, len(migs))
	for _, m := range migs {
		if !applied.Has(m) {
			record = append(record, m)
		}
	}
	if len(marked) == 0 && len(record) == 0 {
		return fmt.Errorf("%s: %w", target, engine.ErrAlreadyMigrated)
	}
	if err := b.sched.store.CreateAdoption(ctx, runID, target, KindBaseline, record, p); err != nil {
		return err
	}
	b.sched.baseline(ctx, Run{ID: runID, Target: target}, b.sched.log.With("run", runID, "target", target))

	return nil
}
