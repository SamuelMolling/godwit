package engine

import (
	"context"
	"errors"
	"fmt"
)

// ErrAlreadyMigrated reports a set with nothing left to adopt: every migration in it is already
// recorded, both by the target's journal and by the caller's own ledger.
var ErrAlreadyMigrated = errors.New("target already has applied migrations")

// ErrHistoryConflict reports a migration the target's journal records under different content.
var ErrHistoryConflict = errors.New("recorded on the target under different content")

// RecordCollapsed writes migs into db's history in one statement, without executing them and without the
// per-migration run rows an executor would add; it is how a scratch replay accounts for what a checkpoint
// already carries, so the replay's history matches the target's without running any of it.
func RecordCollapsed(ctx context.Context, db DB, migs []Migration) error {
	if len(migs) == 0 {
		return nil
	}
	if err := ensureSchema(ctx, db); err != nil {
		return err
	}
	versions, names, sums := make([]int64, 0, len(migs)), make([]string, 0, len(migs)), make([]string, 0, len(migs))
	for _, m := range migs {
		versions, names, sums = append(versions, m.Version), append(names, m.Name), append(sums, m.Checksum)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO godwit.migrations (version, name, checksum)
		SELECT * FROM unnest($1::bigint[], $2::text[], $3::text[])
		ON CONFLICT (version) DO NOTHING`, versions, names, sums); err != nil {
		return fmt.Errorf("record %d collapsed migrations: %w", len(migs), err)
	}

	return nil
}

// Journal is what a target's own history records, keyed by migration id.
func Journal(ctx context.Context, db DB) (map[string]string, error) {
	applied, err := readApplied(ctx, db)
	if err != nil {
		return nil, err
	}
	reps, err := readRepeatables(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(applied)+len(reps))
	for _, a := range applied {
		out[MigrationID(a.Version, a.Name, false)] = a.Checksum
	}
	for _, r := range reps {
		out[MigrationID(0, r.Name, true)] = r.Checksum
	}

	return out, nil
}

// MarkApplied records migs in the target's journal without executing them and reports what it added.
// A migration the journal already holds under the same content is left alone; one it holds under
// different content refuses the whole call, because that is drift between the repository and the target.
func (e *Executor) MarkApplied(ctx context.Context, migs []Migration) ([]Migration, error) {
	release, err := acquireLock(ctx, e.db, e.opts.LockWait)
	if err != nil {
		return nil, err
	}
	defer release()

	if err := ensureSchema(ctx, e.db); err != nil {
		return nil, err
	}
	journal, err := Journal(ctx, e.db)
	if err != nil {
		return nil, err
	}
	add := make([]Migration, 0, len(migs))
	for _, m := range migs {
		sum, ok := journal[m.ID()]
		if ok && sum != m.Checksum {
			return nil, fmt.Errorf("%s: %w", m.ID(), ErrHistoryConflict)
		}
		if !ok {
			add = append(add, m)
		}
	}
	if len(add) == 0 {
		return nil, nil
	}

	tx, err := e.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin baseline: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, m := range add {
		if err := record(ctx, tx, Plan{Migration: m, Direction: DirectionUp}); err != nil {
			return nil, fmt.Errorf("mark %s applied: %w", m.ID(), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit baseline: %w", err)
	}

	return add, nil
}
