package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// ErrRevertPlan marks a revert whose down side cannot be built from what the run applied.
var ErrRevertPlan = errors.New("revert plan")

// RunMigration is one migration a run put into its target's history, with the inverse frozen for it.
type RunMigration struct {
	Migration  string
	AppliedAt  time.Time
	RevertedBy string
	// Held marks a migration whose contract phase never ran: applied, but not recorded on the target.
	Held bool
	// Expansion is what godwit generated for the migration's directives, or nil when it had none.
	Expansion *Expansion
}

// RecordApplied adds a migration to a run's ledger, or updates it when the contract phase completes it.
func (s *Store) RecordApplied(ctx context.Context, runID, migration string, held bool, exp *Expansion) error {
	var body any
	if exp != nil {
		body = jsonOf(*exp)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO cp_run_applied (run_id, migration, seq, held, expansion)
		VALUES ($1, $2, (SELECT coalesce(max(seq), 0) + 1 FROM cp_run_applied WHERE run_id = $1), $3, $4)
		ON CONFLICT (run_id, migration) DO UPDATE SET held = EXCLUDED.held, applied_at = now()`,
		runID, migration, held, body); err != nil {
		return fmt.Errorf("record applied %s: %w", migration, err)
	}

	return nil
}

// MarkReverted records that revertID undid one migration of run origID.
func (s *Store) MarkReverted(ctx context.Context, origID, revertID, migration string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE cp_run_applied SET reverted_by = $2 WHERE run_id = $1 AND migration = $3`,
		origID, revertID, migration); err != nil {
		return fmt.Errorf("mark reverted %s: %w", migration, err)
	}

	return nil
}

// AppliedMigrations lists a run's ledger in the order the run applied it.
func (s *Store) AppliedMigrations(ctx context.Context, runID string) ([]RunMigration, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT migration, applied_at, coalesce(reverted_by::text, ''), held, expansion
		FROM cp_run_applied WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}
	var out []RunMigration
	var m RunMigration
	if _, err := pgx.ForEachRow(rows, []any{&m.Migration, &m.AppliedAt, &m.RevertedBy, &m.Held, &m.Expansion}, func() error {
		out = append(out, m)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}

	return out, nil
}

// RevertPlan is the down side of what a run actually applied, newest first.
type RevertPlan struct {
	Run     Run
	Applied []RunMigration
	Plans   []engine.Plan
	// Newer marks a run another one already stands on top of: reverting it takes a force.
	Newer bool
}

// Drops lists every table and column the plan would remove, keyed by migration.
func (r RevertPlan) Drops() map[string][]engine.Drop {
	return engine.PlanDrops(r.Plans)
}

// PlanRevert builds the plans that undo run id: the down side of every migration its ledger still
// holds, in reverse order of application, each expanded from the inverse frozen for it.
func (s *Store) PlanRevert(ctx context.Context, id string) (RevertPlan, error) {
	run, err := s.Run(ctx, id)
	if err != nil {
		return RevertPlan{}, err
	}
	applied, err := s.AppliedMigrations(ctx, id)
	if err != nil {
		return RevertPlan{}, err
	}
	standing := make([]RunMigration, 0, len(applied))
	for _, m := range applied {
		if m.RevertedBy == "" {
			standing = append(standing, m)
		}
	}
	if len(standing) == 0 {
		return RevertPlan{}, notRevertable(id, reasonNothing)
	}
	if err := s.checkpointBar(ctx, run.Target, standing); err != nil {
		return RevertPlan{}, err
	}
	files, err := s.RunFiles(ctx, id)
	if err != nil {
		return RevertPlan{}, err
	}
	sub, err := filesOf(files, standing)
	if err != nil {
		return RevertPlan{}, err
	}
	plans, err := PlansFromFiles(sub, engine.DirectionDown)
	if err != nil {
		return RevertPlan{}, fmt.Errorf("%w: %w", ErrRevertPlan, err)
	}
	if plans, err = ExpandDown(plans, standing); err != nil {
		return RevertPlan{}, fmt.Errorf("%w: %w", ErrRevertPlan, err)
	}
	newer, err := s.Newer(ctx, run)
	if err != nil {
		return RevertPlan{}, err
	}

	return RevertPlan{Run: run, Applied: standing, Plans: plans, Newer: newer}, nil
}

// checkpointBar refuses a revert that would reach at or below a checkpoint the target holds. Below it
// there is no history left to undo: the versions it collapses were recorded without running, their
// inverses were written against a state the target never passed through, and the replay rebuilds them
// from the checkpoint's body, which a revert cannot edit.
func (s *Store) checkpointBar(ctx context.Context, target string, standing []RunMigration) error {
	cps, err := s.Checkpoints(ctx, target)
	if err != nil {
		return err
	}
	for _, m := range standing {
		cp, ok := engine.NewestCheckpoint(cps)
		version, versioned := versionOf(m.Migration)
		switch {
		case !versioned:
		case slices.ContainsFunc(cps, func(c engine.Migration) bool { return c.ID() == m.Migration }):
			return notRevertable(m.Migration, reasonCheckpoint)
		case ok && version <= cp.Through:
			return notRevertable(m.Migration, reasonCollapsed, cp.ID(), cp.Through)
		}
	}

	return nil
}

func versionOf(id string) (int64, bool) {
	name, _, ok := strings.Cut(id, "_")
	if !ok || len(name) != 14 {
		return 0, false
	}
	v, err := strconv.ParseInt(name, 10, 64)

	return v, err == nil
}

// filesOf narrows a run's submitted files to the up/down pair of each migration it applied.
func filesOf(files map[string]string, applied []RunMigration) (map[string]string, error) {
	out := make(map[string]string, 2*len(applied))
	for _, m := range applied {
		for _, name := range []string{m.Migration + ".up.sql", m.Migration + ".down.sql"} {
			body, ok := files[name]
			if !ok {
				return nil, fmt.Errorf("%w: run applied %s but did not carry %s", ErrRevertPlan, m.Migration, name)
			}
			out[name] = body
		}
	}

	return out, nil
}
