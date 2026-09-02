package engine

import (
	"context"
	"errors"
	"fmt"
)

// ErrAlreadyMigrated reports a target whose journal already holds applied versions.
var ErrAlreadyMigrated = errors.New("target already has applied migrations")

// MarkApplied records migs as applied without executing them; the target must have no applied versions.
func (e *Executor) MarkApplied(ctx context.Context, migs []Migration) error {
	release, err := acquireLock(ctx, e.db)
	if err != nil {
		return err
	}
	defer release()

	if err := bootstrap(ctx, e.db); err != nil {
		return err
	}
	var applied int
	if err := e.db.QueryRow(ctx, `SELECT count(*) FROM godwit.migrations`).Scan(&applied); err != nil {
		return fmt.Errorf("count applied: %w", err)
	}
	if applied > 0 {
		return fmt.Errorf("%d applied versions: %w", applied, ErrAlreadyMigrated)
	}

	tx, err := e.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin baseline: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, m := range migs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO godwit.migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			m.Version, m.Name, m.Checksum); err != nil {
			return fmt.Errorf("mark %d_%s applied: %w", m.Version, m.Name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit baseline: %w", err)
	}

	return nil
}
