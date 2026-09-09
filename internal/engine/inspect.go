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

// Repeatable is one row of the target's godwit.repeatables table: the content last applied under that name.
type Repeatable struct {
	Name      string    `json:"name"`
	Checksum  string    `json:"checksum"`
	AppliedAt time.Time `json:"applied_at"`
}

// ListApplied reads the applied versions without creating godwit's tables; a database never migrated reports none.
func ListApplied(ctx context.Context, db DB) ([]Applied, error) {
	present, err := hasTable(ctx, db, "godwit.migrations")
	if err != nil || !present {
		return nil, err
	}

	return readApplied(ctx, db)
}

// ListRepeatables reads the recorded repeatables without creating godwit's tables.
func ListRepeatables(ctx context.Context, db DB) ([]Repeatable, error) {
	present, err := hasTable(ctx, db, "godwit.repeatables")
	if err != nil || !present {
		return nil, err
	}

	return readRepeatables(ctx, db)
}

func hasTable(ctx context.Context, db DB, name string) (bool, error) {
	var present bool
	if err := db.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&present); err != nil {
		return false, fmt.Errorf("probe godwit schema: %w", err)
	}

	return present, nil
}

func readRepeatables(ctx context.Context, db DB) ([]Repeatable, error) {
	rows, err := db.Query(ctx, `SELECT name, checksum, applied_at FROM godwit.repeatables ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list repeatables: %w", err)
	}
	var out []Repeatable
	var r Repeatable
	if _, err := pgx.ForEachRow(rows, []any{&r.Name, &r.Checksum, &r.AppliedAt}, func() error {
		out = append(out, r)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read repeatables: %w", err)
	}

	return out, nil
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

// SnapshotScope says whether a snapshot counts the bookkeeping tables another migration tool left behind.
type SnapshotScope bool

// Scopes a snapshot is taken under.
const (
	IgnoreAdopted SnapshotScope = false
	KeepAdopted   SnapshotScope = true
)

// SchemaFormat is the first line of every definition; bump it whenever Snapshot changes what it emits.
const SchemaFormat = "godwit-schema-v2"

// SameFormat reports whether a stored definition was taken by this version of Snapshot.
func SameFormat(definition string) bool {
	return strings.HasPrefix(definition, SchemaFormat)
}

// Schema is a canonical description of a database's schema with what the snapshot left out of it.
type Schema struct {
	Definition  string
	Fingerprint string
	Ignored     []Adopted
}

var ownTables = []string{"godwit.migrations", "godwit.repeatables", "godwit.runs", "godwit.journal"}

var snapshotQueries = []struct {
	kind string
	sql  string
}{
	{"table", `
		SELECT n.nspname || '.' || c.relname, n.nspname || '.' || c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema')`},
	{"column", `
		SELECT c.table_schema || '.' || c.table_name,
		       c.table_schema || '.' || c.table_name || '.' || c.column_name || ' ' ||
		       c.data_type || ' null=' || c.is_nullable || ' default=' || coalesce(c.column_default, '<none>')
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
		WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema')`},
	{"constraint", `
		SELECT n.nspname || '.' || cl.relname,
		       n.nspname || '.' || cl.relname || '.' || con.conname || ' ' || pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_class cl ON cl.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')`},
	// An index backing a constraint is the constraint's line already; emitting both makes one change two lines.
	{"index", `
		SELECT i.schemaname || '.' || i.tablename, i.schemaname || '.' || i.indexname || ' ' || i.indexdef
		FROM pg_indexes i
		WHERE i.schemaname NOT IN ('pg_catalog', 'information_schema')
		  AND NOT EXISTS (
		    SELECT 1 FROM pg_constraint con
		    JOIN pg_class ic ON ic.oid = con.conindid
		    JOIN pg_namespace ins ON ins.oid = ic.relnamespace
		    WHERE ic.relname = i.indexname AND ins.nspname = i.schemaname)`},
	// A sequence a serial or identity column owns is that column's line already.
	{"sequence", `
		SELECT s.schemaname || '.' || s.sequencename,
		       s.schemaname || '.' || s.sequencename || ' ' || s.data_type || ' increment=' || s.increment_by ||
		       ' min=' || s.min_value || ' max=' || s.max_value || ' start=' || s.start_value || ' cycle=' || s.cycle
		FROM pg_sequences s
		JOIN pg_class sc ON sc.relname = s.sequencename AND sc.relnamespace = to_regnamespace(s.schemaname)
		WHERE s.schemaname NOT IN ('pg_catalog', 'information_schema')
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = sc.oid AND d.deptype IN ('a', 'i'))`},
	{"type", `
		SELECT n.nspname || '.' || t.typname,
		       n.nspname || '.' || t.typname || ' enum (' || string_agg(e.enumlabel, ', ' ORDER BY e.enumsortorder) || ')'
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		JOIN pg_enum e ON e.enumtypid = t.oid
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		GROUP BY n.nspname, t.typname`},
	{"view", `
		SELECT schemaname || '.' || viewname, schemaname || '.' || viewname || ' ' || md5(definition)
		FROM pg_views
		WHERE schemaname NOT IN ('pg_catalog', 'information_schema')`},
	{"matview", `
		SELECT schemaname || '.' || matviewname, schemaname || '.' || matviewname || ' ' || md5(definition)
		FROM pg_matviews
		WHERE schemaname NOT IN ('pg_catalog', 'information_schema')`},
}

// Snapshot renders a canonical description of the schema plus its sha256 fingerprint.
func Snapshot(ctx context.Context, db DB, scope SnapshotScope) (Schema, error) {
	var out Schema
	excluded := map[string]bool{}
	for _, t := range ownTables {
		excluded[t] = true
	}
	owned, err := extensionOwned(ctx, db)
	if err != nil {
		return Schema{}, err
	}
	for _, o := range owned {
		excluded[o] = true
	}
	if scope == IgnoreAdopted {
		if out.Ignored, err = DetectAdopted(ctx, db); err != nil {
			return Schema{}, err
		}
		for _, a := range out.Ignored {
			excluded[a.Qualified()] = true
		}
	}

	var lines []string
	for _, q := range snapshotQueries {
		rows, err := db.Query(ctx, q.sql)
		if err != nil {
			return Schema{}, fmt.Errorf("inspect %ss: %w", q.kind, err)
		}
		var owner, line string
		if _, err := pgx.ForEachRow(rows, []any{&owner, &line}, func() error {
			if !excluded[owner] {
				lines = append(lines, q.kind+" "+line)
			}

			return nil
		}); err != nil {
			return Schema{}, fmt.Errorf("read %ss: %w", q.kind, err)
		}
	}
	sort.Strings(lines)

	out.Definition = strings.Join(append([]string{SchemaFormat}, lines...), "\n")
	sum := sha256.Sum256([]byte(out.Definition))
	out.Fingerprint = hex.EncodeToString(sum[:])

	return out, nil
}

func extensionOwned(ctx context.Context, db DB) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT n.nspname || '.' || c.relname
		FROM pg_depend d
		JOIN pg_class c ON c.oid = d.objid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE d.deptype = 'e' AND d.classid = 'pg_class'::regclass
		UNION
		SELECT n.nspname || '.' || t.typname
		FROM pg_depend d
		JOIN pg_type t ON t.oid = d.objid
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE d.deptype = 'e' AND d.classid = 'pg_type'::regclass`)
	if err != nil {
		return nil, fmt.Errorf("inspect extension objects: %w", err)
	}
	var out []string
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&name}, func() error {
		out = append(out, name)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read extension objects: %w", err)
	}

	return out, nil
}

// InvalidIndexes lists the indexes a failed CONCURRENTLY build left behind, schema-qualified and sorted.
func InvalidIndexes(ctx context.Context, db DB) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT n.nspname || '.' || c.relname
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT i.indisvalid AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("inspect invalid indexes: %w", err)
	}
	var out []string
	var name string
	if _, err := pgx.ForEachRow(rows, []any{&name}, func() error {
		out = append(out, name)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read invalid indexes: %w", err)
	}

	return out, nil
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
	delete(count, "")

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
