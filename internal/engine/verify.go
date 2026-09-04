package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// reconcile reports whether a crashed statement took effect, repairing partial effects otherwise.
func reconcile(ctx context.Context, db DB, st Statement) (done bool, err error) {
	switch st.Verifier {
	case VerifierCreateIndexConcurrently:
		return reconcileCreateIndex(ctx, db, st)
	case VerifierDropIndexConcurrently:
		return reconcileDropIndex(ctx, db, st)
	case VerifierBatch: // the cursor in the journal is the recovery point.
		return false, nil
	default: // VerifierRerun: idempotent by classification, run it again.
		return false, nil
	}
}

func reconcileCreateIndex(ctx context.Context, db DB, st Statement) (bool, error) {
	ref := quoteIndex(st)
	table, want, err := indexShape(st.SQL)
	if err != nil {
		return false, err
	}
	var valid, onTable bool
	var def string
	err = db.QueryRow(ctx, `
		SELECT i.indisvalid, i.indrelid IS NOT DISTINCT FROM to_regclass($2)::oid, pg_get_indexdef(i.indexrelid)
		FROM pg_index i WHERE i.indexrelid = to_regclass($1)::oid`, ref, table).Scan(&valid, &onTable, &def)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect index %s: %w", ref, err)
	}
	_, got, err := indexShape(def)
	if err != nil {
		return false, err
	}
	if !onTable || got != want {
		return false, fmt.Errorf(
			"index %s already exists as %q, which is not what this statement builds (%q); godwit adopts only the index it was asked for: drop or rename the existing one, then resume",
			ref, def, st.SQL)
	}
	if valid {
		return true, nil
	}
	if _, err := db.Exec(ctx, "DROP INDEX "+ref); err != nil {
		return false, fmt.Errorf("drop invalid index %s: %w", ref, err)
	}

	return false, nil
}

// indexShape is the table a CREATE INDEX statement indexes and a canonical rendering of the index it
// builds. The name, the schema qualification and CONCURRENTLY are stripped, and the parser fills the
// omitted access method in, so a planned statement and the catalog's pg_get_indexdef of an existing
// index compare on definition alone: columns, expressions, uniqueness, method, predicate and storage.
func indexShape(sql string) (table, shape string, err error) {
	res, err := pgquery.Parse(sql)
	if err != nil {
		return "", "", fmt.Errorf("parse index statement %q: %w", sql, err)
	}
	stmts := res.GetStmts()
	if len(stmts) != 1 || stmts[0].GetStmt().GetIndexStmt() == nil {
		return "", "", fmt.Errorf("not a single CREATE INDEX statement: %q", sql)
	}
	idx := stmts[0].GetStmt().GetIndexStmt()
	rel := idx.GetRelation()
	table = pgx.Identifier{rel.GetRelname()}.Sanitize()
	if rel.GetSchemaname() != "" {
		table = pgx.Identifier{rel.GetSchemaname(), rel.GetRelname()}.Sanitize()
	}
	idx.Concurrent, idx.IfNotExists, idx.Idxname = false, false, ""
	rel.Schemaname, rel.Relname = "", "t"
	shape, err = pgquery.Deparse(res)

	return table, shape, err
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
