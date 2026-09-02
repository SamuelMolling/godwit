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

// Baseline marks migs applied on target, records run runID holding their files and snapshots the schema for drift.
func (b *Baseliner) Baseline(ctx context.Context, runID, target string, migs []engine.Migration) error {
	dsn, err := b.sched.targetDSN(ctx, target)
	if err != nil {
		return err
	}
	if err := b.sched.engine.MarkApplied(ctx, dsn, migs); err != nil {
		return err
	}
	if err := b.sched.store.CreateBaseline(ctx, runID, target, migrationFiles(migs)); err != nil {
		return err
	}
	b.sched.baseline(ctx, Run{ID: runID, Target: target}, b.sched.log.With("run", runID, "target", target))

	return nil
}

func migrationFiles(migs []engine.Migration) map[string]string {
	files := make(map[string]string, 2*len(migs))
	for _, m := range migs {
		files[fmt.Sprintf("%014d_%s.up.sql", m.Version, m.Name)] = m.UpSQL
		files[fmt.Sprintf("%014d_%s.down.sql", m.Version, m.Name)] = m.DownSQL
	}

	return files
}
