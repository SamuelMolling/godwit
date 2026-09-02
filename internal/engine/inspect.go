package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Applied is one row of the target's godwit.migrations table.
type Applied struct {
	Version   int64     `json:"version"`
	Name      string    `json:"name"`
	Checksum  string    `json:"checksum"`
	AppliedAt time.Time `json:"applied_at"`
}

// ListApplied reads the applied versions without creating godwit's tables; a database never migrated reports none.
func ListApplied(ctx context.Context, db DB) ([]Applied, error) {
	var present bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('godwit.migrations') IS NOT NULL`).Scan(&present); err != nil {
		return nil, fmt.Errorf("probe godwit schema: %w", err)
	}
	if !present {
		return nil, nil
	}

	return readApplied(ctx, db)
}

func readApplied(ctx context.Context, db DB) ([]Applied, error) {
	rows, err := db.Query(ctx, `SELECT version, name, checksum, applied_at FROM godwit.migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("list applied: %w", err)
	}
	var out []Applied
	var a Applied
	if _, err := pgx.ForEachRow(rows, []any{&a.Version, &a.Name, &a.Checksum, &a.AppliedAt}, func() error {
		out = append(out, a)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read applied: %w", err)
	}

	return out, nil
}

// Snapshot renders a canonical description of the schema plus its sha256 fingerprint.
func Snapshot(ctx context.Context, db DB) (definition, fingerprint string, err error) {
	// godwit's own tables are invisible to drift.
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

// DiffSchemas lists the lines present in only one snapshot ("- " expected, "+ " live).
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
