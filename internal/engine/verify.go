package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// reconcile reports whether a crashed statement took effect, repairing partial effects otherwise.
func reconcile(ctx context.Context, db DB, st Statement) (done bool, err error) {
	switch st.Verifier {
	case VerifierCreateIndexConcurrently:
		return reconcileCreateIndex(ctx, db, st)
	case VerifierDropIndexConcurrently:
		return reconcileDropIndex(ctx, db, st)
	default: // VerifierRerun: idempotent by classification, run it again.
		return false, nil
	}
}

func reconcileCreateIndex(ctx context.Context, db DB, st Statement) (bool, error) {
	ref := quoteIndex(st)
	var valid bool
	err := db.QueryRow(ctx,
		`SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass($1)::oid`,
		ref).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect index %s: %w", ref, err)
	}
	if valid {
		return true, nil
	}
	if _, err := db.Exec(ctx, "DROP INDEX "+ref); err != nil {
		return false, fmt.Errorf("drop invalid index %s: %w", ref, err)
	}

	return false, nil
}

func reconcileDropIndex(ctx context.Context, db DB, st Statement) (bool, error) {
	ref := quoteIndex(st)
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, ref).Scan(&exists); err != nil {
		return false, fmt.Errorf("inspect index %s: %w", ref, err)
	}

	return !exists, nil
}

func quoteIndex(st Statement) string {
	if st.IndexSchema == "" {
		return pgx.Identifier{st.IndexName}.Sanitize()
	}

	return pgx.Identifier{st.IndexSchema, st.IndexName}.Sanitize()
}
