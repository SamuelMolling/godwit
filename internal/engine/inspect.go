package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Snapshot renders a canonical, ordered description of the database schema
// (tables, columns, constraints, indexes, views — godwit's own schema excluded)
// plus its sha256 fingerprint. Equal schemas produce identical snapshots, so
// drift detection is a string comparison.
func Snapshot(ctx context.Context, db DB) (definition, fingerprint string, err error) {
	// godwit's own bookkeeping tables are invisible to drift; everything else
	// counts, whatever schema it lives in.
	const ownTables = `('migrations', 'runs', 'journal')`
	queries := []struct {
		kind string
		sql  string
	}{
		{"column", `
			SELECT c.table_schema || '.' || c.table_name || '.' || c.column_name || ' ' ||
			       c.data_type || ' null=' || c.is_nullable || ' default=' || coalesce(c.column_default, '<none>')
			FROM information_schema.columns c
			JOIN information_schema.tables t
			  ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
			WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema')
			  AND NOT (c.table_schema = 'godwit' AND c.table_name IN ` + ownTables + `)`},
		{"constraint", `
			SELECT n.nspname || '.' || cl.relname || '.' || con.conname || ' ' || pg_get_constraintdef(con.oid)
			FROM pg_constraint con
			JOIN pg_class cl ON cl.oid = con.conrelid
			JOIN pg_namespace n ON n.oid = cl.relnamespace
			WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
			  AND NOT (n.nspname = 'godwit' AND cl.relname IN ` + ownTables + `)`},
		{"index", `
			SELECT schemaname || '.' || indexname || ' ' || indexdef
			FROM pg_indexes
			WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
			  AND NOT (schemaname = 'godwit' AND tablename IN ` + ownTables + `)`},
		{"view", `
			SELECT schemaname || '.' || viewname || ' ' || md5(definition)
			FROM pg_views
			WHERE schemaname NOT IN ('pg_catalog', 'information_schema')`},
	}

	var lines []string
	for _, q := range queries {
		rows, err := db.Query(ctx, q.sql)
		if err != nil {
			return "", "", fmt.Errorf("inspect %ss: %w", q.kind, err)
		}
		var line string
		if _, err := pgx.ForEachRow(rows, []any{&line}, func() error {
			lines = append(lines, q.kind+" "+line)

			return nil
		}); err != nil {
			return "", "", fmt.Errorf("read %ss: %w", q.kind, err)
		}
	}
	sort.Strings(lines)

	definition = strings.Join(lines, "\n")
	sum := sha256.Sum256([]byte(definition))

	return definition, hex.EncodeToString(sum[:]), nil
}

// DiffSchemas reports the lines present in only one of two snapshots:
// "- " prefixes what the expected schema has and the live one lost,
// "+ " prefixes what the live schema has that was never migrated in.
func DiffSchemas(expected, live string) []string {
	count := map[string]int{}
	for _, l := range strings.Split(expected, "\n") {
		count[l]++
	}
	for _, l := range strings.Split(live, "\n") {
		count[l]--
	}

	var missing, unexpected []string
	for l, c := range count {
		switch {
		case c > 0:
			missing = append(missing, "- "+l)
		case c < 0:
			unexpected = append(unexpected, "+ "+l)
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpected)

	return append(missing, unexpected...)
}
