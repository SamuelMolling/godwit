package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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

// ErrMigrationFiles marks committed migration files that do not replay on top of the target's history.
var ErrMigrationFiles = errors.New("migration files failed to replay")

// ErrValidationDisabled marks a diff against the committed files on a service started with validation off.
var ErrValidationDisabled = errors.New("diffing against the committed files needs validation, which is disabled on this service")

// ErrRepeatablesUnknown marks a diff whose request carried no migration files while the target records
// repeatable migrations, so the objects those files build cannot be told from objects nothing manages.
var ErrRepeatablesUnknown = errors.New("target records repeatable migrations and the request carried no migration files")

// ErrRepeatableSchema marks a repeatable migration that does not build on the desired schema.
var ErrRepeatableSchema = errors.New("repeatable migration does not build on the desired schema")

var (
	parseDSN     = pgx.ParseConfig
	generatePlan = diff.Generate
	listObjects  = scratchObjects
)

// DiffBase is the schema a desired one is compared against.
type DiffBase int

const (
	// DiffBaseLive is the target's live schema.
	DiffBaseLive DiffBase = iota
	// DiffBaseFiles is what the committed migration files produce on top of the target's recorded history.
	DiffBaseFiles
)

// HistoryReplayer rebuilds a target's recorded history on a scratch database (implemented by Validator).
type HistoryReplayer interface {
	Validate(ctx context.Context, target string, plans []engine.Plan, searchPath string) (Validation, error)
	Replay(ctx context.Context, conn engine.DB, target, searchPath string, plans []engine.Plan) error
}

// SchemaDiff is the migration from a target's live schema to a desired one, and back.
type SchemaDiff struct {
	Observed Observation
	UpSQL    string
	DownSQL  string
	Drift    []string
	// Retained names the retired columns whose drop was taken out of UpSQL.
	Retained []string
	// RepeatableObjects names what the request's repeatable migrations build on the desired schema.
	RepeatableObjects []string
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

// Diff applies ddl on a scratch database and returns the SQL between base and it in both directions.
func (d *Differ) Diff(ctx context.Context, target, ddl string, base DiffBase, files map[string]string) (SchemaDiff, error) {
	tg, err := d.sched.target(ctx, target)
	if err != nil {
		return SchemaDiff{}, err
	}
	obs, err := d.sched.engine.Observe(ctx, tg.dsn)
	if err != nil {
		return SchemaDiff{}, err
	}
	if len(files) == 0 && len(obs.Repeatables) > 0 {
		return SchemaDiff{}, fmt.Errorf("%w: %s", ErrRepeatablesUnknown, recordedRepeatables(obs.Repeatables))
	}
	reps, err := repeatablesIn(files)
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
	from, label := live, "live"
	if base == DiffBaseFiles {
		replayed, done, err := d.filesBase(ctx, factory, target, obs.SearchPath, files)
		if err != nil {
			return SchemaDiff{}, err
		}
		defer done()
		from, label = replayed, "files"
	}

	desired, err := factory.Create(ctx)
	if err != nil {
		return SchemaDiff{}, err
	}
	defer func() { _ = desired.Close(ctx) }()
	if _, err := desired.ConnPool.ExecContext(ctx, ddl); err != nil {
		return SchemaDiff{}, fmt.Errorf("%w: %w", ErrDesiredSchema, err)
	}
	if out.RepeatableObjects, err = buildRepeatables(ctx, desired.ConnPool, reps); err != nil {
		return SchemaDiff{}, err
	}

	if out.UpSQL, err = generate(ctx, from, desired.ConnPool, factory); err != nil {
		return SchemaDiff{}, fmt.Errorf("diff %s to desired: %w", label, err)
	}
	retired, err := d.sched.store.RetiredColumns(ctx, target)
	if err != nil {
		return SchemaDiff{}, err
	}
	out.UpSQL, out.Retained = keepRetired(out.UpSQL, retired)
	if out.DownSQL, err = generate(ctx, desired.ConnPool, from, factory); err != nil {
		return SchemaDiff{}, fmt.Errorf("diff desired to %s: %w", label, err)
	}

	return out, nil
}

// filesBase builds the schema the committed files claim to produce: a scratch database carrying the
// target's recorded history with the files it has not run yet replayed on top.
func (d *Differ) filesBase(ctx context.Context, factory *scratchFactory, target, searchPath string, files map[string]string) (*sql.DB, func(), error) {
	if d.history == nil {
		return nil, nil, ErrValidationDisabled
	}
	plans, err := PlansFromFiles(files, engine.DirectionUp)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrMigrationFiles, err)
	}
	name, scratch, err := factory.create(ctx)
	if err != nil {
		return nil, nil, err
	}
	done := func() { _ = scratch.Close(context.WithoutCancel(ctx)) }

	cfg := d.pool.Config().ConnConfig.Copy()
	cfg.Database = name
	conn, err := connectScratch(ctx, cfg)
	if err != nil {
		done()

		return nil, nil, fmt.Errorf("connect scratch database: %w", err)
	}
	err = d.history.Replay(ctx, conn, target, searchPath, plans)
	_ = conn.Close(context.WithoutCancel(ctx))
	if err != nil {
		done()

		return nil, nil, fmt.Errorf("%w: %w", ErrMigrationFiles, err)
	}

	return scratch.ConnPool, done, nil
}

// repeatablesIn loads the R__ pairs of a request, in the order a run applies them; the versioned files are
// left alone so a directory a run would refuse still diffs.
func repeatablesIn(files map[string]string) ([]engine.Migration, error) {
	sub := map[string]string{}
	for name, body := range files {
		if strings.HasPrefix(name, engine.RepeatablePrefix) {
			sub[name] = body
		}
	}
	if len(sub) == 0 {
		return nil, nil
	}
	migs, err := MigrationsFromFiles(sub)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMigrationFiles, err)
	}

	return migs, nil
}

func recordedRepeatables(reps []engine.Repeatable) string {
	names := make([]string, 0, len(reps))
	for _, r := range reps {
		names = append(names, engine.RepeatablePrefix+r.Name)
	}

	return strings.Join(names, ", ")
}

// buildRepeatables applies the request's repeatable migrations on the desired schema, so the objects they
// declare are on both sides of the comparison, and names the objects that appeared.
func buildRepeatables(ctx context.Context, db *sql.DB, reps []engine.Migration) ([]string, error) {
	if len(reps) == 0 {
		return nil, nil
	}
	before, err := listObjects(ctx, db)
	if err != nil {
		return nil, err
	}
	for _, r := range reps {
		if _, err := db.ExecContext(ctx, r.UpSQL); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrRepeatableSchema, r.UpFile(), err)
		}
	}
	after, err := listObjects(ctx, db)
	if err != nil {
		return nil, err
	}
	built := make([]string, 0, len(after))
	for _, o := range after {
		if !slices.Contains(before, o) {
			built = append(built, o)
		}
	}

	return built, nil
}

const objectsSQL = `SELECT coalesce(string_agg(o, chr(10) ORDER BY o), '') FROM (
	SELECT n.nspname || '.' || c.relname AS o
	  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE n.nspname NOT IN ('pg_catalog', 'pg_toast', 'information_schema')
	UNION
	SELECT n.nspname || '.' || p.proname
	  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
	 WHERE n.nspname NOT IN ('pg_catalog', 'pg_toast', 'information_schema')
	UNION
	SELECT n.nspname || '.' || t.tgname
	  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE NOT t.tgisinternal AND n.nspname NOT IN ('pg_catalog', 'pg_toast', 'information_schema')
) objects`

func scratchObjects(ctx context.Context, db *sql.DB) ([]string, error) {
	var list string
	if err := db.QueryRowContext(ctx, objectsSQL).Scan(&list); err != nil {
		return nil, fmt.Errorf("list scratch objects: %w", err)
	}

	return slices.DeleteFunc(strings.Split(list, "\n"), func(o string) bool { return o == "" }), nil
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
	_, db, err := f.create(ctx)

	return db, err
}

func (f *scratchFactory) create(ctx context.Context) (string, *tempdb.Database, error) {
	name := "godwit_diff_" + f.newID()
	if _, err := f.pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", nil, fmt.Errorf("create scratch database: %w", err)
	}
	cfg := f.pool.Config().ConnConfig.Copy()
	cfg.Database = name
	if f.searchPath != "" {
		cfg.RuntimeParams["search_path"] = f.searchPath
	}
	db := stdlib.OpenDB(*cfg)

	return name, &tempdb.Database{ConnPool: db, ContextualCloser: scratchCloser{pool: f.pool, db: db, name: name}}, nil
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
