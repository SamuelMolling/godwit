package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stripe/pg-schema-diff/pkg/diff"
	"github.com/stripe/pg-schema-diff/pkg/tempdb"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// ErrDesiredSchema marks a desired schema the author must fix.
var ErrDesiredSchema = errors.New("desired schema failed to apply")

var (
	parseDSN     = pgx.ParseConfig
	generatePlan = diff.Generate
)

// HistoryReplayer rebuilds a target's recorded history on a scratch database (implemented by Validator).
type HistoryReplayer interface {
	Validate(ctx context.Context, target string, plans []engine.Plan, searchPath string) (Validation, error)
}

// SchemaDiff is the migration from a target's live schema to a desired one, and back.
type SchemaDiff struct {
	Observed Observation
	UpSQL    string
	DownSQL  string
	Drift    []string
	// Retained names the retired columns whose drop was taken out of UpSQL.
	Retained []string
}

// keepRetired drops the statements that would remove a column a change-type kept as its rollback; the
// ORM never knew about it, so every diff would otherwise propose the same drop again.
func keepRetired(sql string, retired []RetiredColumn) (string, []string) {
	if sql == "" || len(retired) == 0 {
		return sql, nil
	}
	kept := make([]string, 0, len(retired))
	lines := make([]string, 0, len(retired))
	for _, l := range strings.Split(sql, "\n") {
		if c, ok := dropsRetired(l, retired); ok {
			kept = append(kept, c.String())

			continue
		}
		lines = append(lines, l)
	}

	return strings.Join(lines, "\n"), kept
}

func dropsRetired(line string, retired []RetiredColumn) (RetiredColumn, bool) {
	for _, c := range retired {
		if mentions(line, c.Schema, c.Table) && mentions(line, "DROP COLUMN "+c.Column) {
			return c, true
		}
	}

	return RetiredColumn{}, false
}

// mentions matches a reference however the generator quoted it: pg-schema-diff always quotes, godwit's
// own recipes only where PostgreSQL requires it.
func mentions(line string, parts ...string) bool {
	prefix := ""
	if n := strings.LastIndex(parts[len(parts)-1], " "); n >= 0 {
		prefix, parts[len(parts)-1] = parts[len(parts)-1][:n+1], parts[len(parts)-1][n+1:]
	}
	bare := make([]string, 0, len(parts))
	for _, p := range parts {
		bare = append(bare, engine.Ident(p))
	}

	return strings.Contains(line, prefix+pgx.Identifier(parts).Sanitize()) ||
		strings.Contains(line, prefix+strings.Join(bare, "."))
}

// Differ generates the migration between a target's live schema and a desired DDL applied on a scratch database.
type Differ struct {
	pool    *pgxpool.Pool
	sched   *Scheduler
	history HistoryReplayer
	newID   func() string
}

// NewDiffer wires a Differ over the control-plane pool; history is optional (nil skips drift).
func NewDiffer(pool *pgxpool.Pool, sched *Scheduler, history HistoryReplayer, newID func() string) *Differ {
	return &Differ{pool: pool, sched: sched, history: history, newID: newID}
}

// Diff observes the target, applies ddl on a scratch database and returns the SQL between the two in both directions.
func (d *Differ) Diff(ctx context.Context, target, ddl string) (SchemaDiff, error) {
	tg, err := d.sched.target(ctx, target)
	if err != nil {
		return SchemaDiff{}, err
	}
	obs, err := d.sched.engine.Observe(ctx, tg.dsn)
	if err != nil {
		return SchemaDiff{}, err
	}
	out := SchemaDiff{Observed: obs}
	if d.history != nil {
		val, err := d.history.Validate(ctx, target, nil, obs.SearchPath)
		if err != nil {
			return SchemaDiff{}, err
		}
		out.Drift = engine.DiffSchemas(val.Base, obs.Definition)
	}

	liveCfg, err := parseDSN(tg.dsn)
	if err != nil {
		return SchemaDiff{}, fmt.Errorf("parse target dsn: %w", err)
	}
	live := stdlib.OpenDB(*liveCfg)
	defer func() { _ = live.Close() }()

	factory := &scratchFactory{pool: d.pool, newID: d.newID, searchPath: obs.SearchPath}
	desired, err := factory.Create(ctx)
	if err != nil {
		return SchemaDiff{}, err
	}
	defer func() { _ = desired.Close(ctx) }()
	if _, err := desired.ConnPool.ExecContext(ctx, ddl); err != nil {
		return SchemaDiff{}, fmt.Errorf("%w: %w", ErrDesiredSchema, err)
	}

	if out.UpSQL, err = generate(ctx, live, desired.ConnPool, factory); err != nil {
		return SchemaDiff{}, fmt.Errorf("diff live to desired: %w", err)
	}
	retired, err := d.sched.store.RetiredColumns(ctx, target)
	if err != nil {
		return SchemaDiff{}, err
	}
	out.UpSQL, out.Retained = keepRetired(out.UpSQL, retired)
	if out.DownSQL, err = generate(ctx, desired.ConnPool, live, factory); err != nil {
		return SchemaDiff{}, fmt.Errorf("diff desired to live: %w", err)
	}

	return out, nil
}

func generate(ctx context.Context, from, to *sql.DB, factory tempdb.Factory) (string, error) {
	plan, err := generatePlan(ctx, diff.DBSchemaSource(from), diff.DBSchemaSource(to),
		diff.WithTempDbFactory(factory), diff.WithExcludeSchemas("godwit"), diff.WithLogger(quietLog{}))
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, len(plan.Statements))
	for _, st := range plan.Statements {
		lines = append(lines, st.ToSQL())
	}

	return strings.Join(lines, "\n"), nil
}

type quietLog struct{}

// Errorf drops pg-schema-diff's messages; the returned error carries what matters.
func (quietLog) Errorf(string, ...any) {}

// Warnf drops pg-schema-diff's messages; the returned error carries what matters.
func (quietLog) Warnf(string, ...any) {}

type scratchFactory struct {
	pool       *pgxpool.Pool
	newID      func() string
	searchPath string
}

// Create makes a fresh database on the control-plane server, reachable with the pool's credentials.
func (f *scratchFactory) Create(ctx context.Context) (*tempdb.Database, error) {
	name := "godwit_diff_" + f.newID()
	if _, err := f.pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return nil, fmt.Errorf("create scratch database: %w", err)
	}
	cfg := f.pool.Config().ConnConfig.Copy()
	cfg.Database = name
	if f.searchPath != "" {
		cfg.RuntimeParams["search_path"] = f.searchPath
	}
	db := stdlib.OpenDB(*cfg)

	return &tempdb.Database{ConnPool: db, ContextualCloser: scratchCloser{pool: f.pool, db: db, name: name}}, nil
}

// Close is a no-op: every scratch database is dropped by its own closer.
func (*scratchFactory) Close() error { return nil }

type scratchCloser struct {
	pool *pgxpool.Pool
	db   *sql.DB
	name string
}

// Close drops the scratch database even when ctx is already cancelled.
func (c scratchCloser) Close(ctx context.Context) error {
	_ = c.db.Close()
	_, _ = c.pool.Exec(context.WithoutCancel(ctx),
		"DROP DATABASE IF EXISTS "+pgx.Identifier{c.name}.Sanitize()+" WITH (FORCE)")

	return nil
}
