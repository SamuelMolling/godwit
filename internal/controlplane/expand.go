package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	pgquery "github.com/pganalyze/pg_query_go/v6"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// ErrDirective marks a directive godwit will not expand; the API reports it as invalid_argument.
var ErrDirective = errors.New("godwit directive")

// DefaultBatchSize is how many rows a generated backfill touches per transaction when the directive is silent.
const DefaultBatchSize = 5000

// batchKeyAlias names the cursor column the batched backfill returns, kept distinct from the table's own columns.
const batchKeyAlias = "godwit_key"

// RetiredColumn is a column a change-type left behind as the rollback of a completed swap.
type RetiredColumn struct {
	Schema  string `json:"schema"`
	Table   string `json:"table"`
	Column  string `json:"column"`
	Retires string `json:"retires"`
}

// String renders the retired column as a qualified reference.
func (c RetiredColumn) String() string {
	return c.Schema + "." + c.Table + "." + c.Column
}

// Expansion is the SQL godwit generates for one migration's directives, frozen into the plan so the run
// applies what the pull request showed.
type Expansion struct {
	ID       string              `json:"id"`
	UpSQL    string              `json:"up_sql"`
	DownSQL  string              `json:"down_sql"`
	DownHeld string              `json:"down_held_sql,omitempty"`
	Phase    []string            `json:"phase"`
	Batches  []*engine.BatchSpec `json:"batches,omitempty"`
	Notes    []string            `json:"notes,omitempty"`
	Retired  []RetiredColumn     `json:"retired,omitempty"`
	// Unretired are the columns the expansion removes, so a drop-column clears what a change-type retired.
	Unretired []RetiredColumn `json:"unretired,omitempty"`
	Lines     []string        `json:"lines,omitempty"`
	Hash      string          `json:"hash"`
}

// Contract is the index of the first contract statement, or -1 when the expansion has one phase.
func (e Expansion) Contract() int {
	return slices.Index(e.Phase, engine.PhaseContract)
}

// Expander turns directives into statements using a catalog that already holds the target's history.
type Expander struct {
	// KeepOld leaves the pre-swap column in place unless a directive says otherwise.
	KeepOld bool
	// BatchSize is the default rows per backfill transaction; zero means DefaultBatchSize.
	BatchSize int
}

// NewExpander returns an Expander with godwit's defaults.
func NewExpander() *Expander {
	return &Expander{KeepOld: true, BatchSize: DefaultBatchSize}
}

type step struct {
	sql   string
	batch *engine.BatchSpec
}

type built struct {
	expand    []step
	contract  []step
	notes     []string
	retired   []RetiredColumn
	unretired []RetiredColumn
	down      []string
	downHeld  []string
	downWhy   string
}

// Expand renders every directive of m against conn and returns the bodies the plan freezes.
func (x *Expander) Expand(ctx context.Context, conn engine.DB, m engine.Migration) (Expansion, error) {
	if err := checkDestructive(m); err != nil {
		return Expansion{}, err
	}
	if err := checkDuplicates(m.Directives); err != nil {
		return Expansion{}, err
	}
	all := make([]built, 0, len(m.Directives))
	for _, d := range m.Directives {
		b, err := x.one(ctx, conn, d)
		if err != nil {
			return Expansion{}, err
		}
		all = append(all, b)
	}
	exp, err := spliceExpansion(m, all)
	if err != nil {
		return Expansion{}, err
	}
	if m.RevertDirective {
		if exp.DownSQL, err = revertBody(m, all); err != nil {
			return Expansion{}, err
		}
		exp.DownHeld = heldBody(all)
	}
	exp.Hash = expansionHash(exp)

	return exp, nil
}

func (x *Expander) one(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	switch d.Op {
	case "change-type":
		return x.changeType(ctx, conn, d)
	case "backfill":
		return x.backfill(ctx, conn, d)
	case "add-column":
		return x.addColumn(ctx, conn, d)
	case "add-not-null":
		return addNotNull(ctx, conn, d)
	case "add-index":
		return addIndex(ctx, conn, d)
	case "drop-index":
		return dropIndex(ctx, conn, d)
	case "add-fk":
		return addForeignKey(ctx, conn, d)
	case "add-check":
		return addCheck(ctx, conn, d)
	case "drop-column":
		return dropColumn(ctx, conn, d)
	default:
		return built{}, refuse(d, "%s has no expansion", d.Op)
	}
}

func refuse(d engine.Directive, format string, args ...any) error {
	return fmt.Errorf("%w on line %d (%s): %s", ErrDirective, d.Line, d.Text, fmt.Sprintf(format, args...))
}

var contractHazardCodes = []string{"H002", "H003", "H008"}

// checkDestructive refuses a directive migration whose own SQL is destructive: the generated contract block
// is a suffix of the plan, so a destructive statement before it would run in the expand phase.
func checkDestructive(m engine.Migration) error {
	p, err := engine.BuildPlan(m, engine.DirectionUp)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrDirective, m.ID(), err)
	}
	for _, st := range p.Statements {
		for _, h := range st.Hazards {
			if slices.Contains(contractHazardCodes, h.Code) {
				return fmt.Errorf("%w: %s carries a directive and %s in its own SQL; split them into two migrations",
					ErrDirective, m.ID(), h.Code)
			}
		}
	}

	return nil
}

func checkDuplicates(ds []engine.Directive) error {
	seen := map[string]int{}
	for _, d := range ds {
		target := subject(d)
		if line, ok := seen[target]; ok {
			return refuse(d, "%s is already the subject of the directive on line %d", target, line)
		}
		seen[target] = d.Line
	}

	return nil
}

// subject names what a directive acts on, so two directives on it are ambiguous. For most operations the
// first argument is the object; the ones whose first argument is only the table need the finer name.
func subject(d engine.Directive) string {
	switch {
	case len(d.Args) == 0:
		return d.Op
	case d.Op == "add-index":
		if name, ok := d.Opts["name"]; ok {
			return name
		}

		return d.Args[0] + " " + d.Args[1]
	case d.Op == "add-check":
		return d.Args[0] + " " + d.Args[1]
	default:
		return d.Args[0]
	}
}

// spliceExpansion replaces each directive line with its expand statements and appends every contract
// block at the end, so the contract phase is always a suffix the executor can hold from one index.
func spliceExpansion(m engine.Migration, all []built) (Expansion, error) {
	exp := Expansion{ID: m.ID()}
	lines := strings.Split(m.UpSQL, "\n")
	at := map[int]int{}
	for i, d := range m.Directives {
		at[d.Line] = i
		exp.Lines = append(exp.Lines, d.Text)
	}
	var chunks []chunk
	var raw []string
	flush := func() {
		if len(raw) > 0 {
			chunks = append(chunks, chunk{sql: strings.Join(raw, "\n")})
			raw = nil
		}
	}
	for n, line := range lines {
		i, ok := at[n+1]
		if !ok {
			raw = append(raw, line)

			continue
		}
		flush()
		chunks = append(chunks, chunk{sql: "-- godwit expanded: " + m.Directives[i].Op + " " + strings.Join(m.Directives[i].Args, " ")})
		for _, s := range all[i].expand {
			chunks = append(chunks, chunk{sql: s.sql + ";", phase: engine.PhaseExpand, batch: s.batch, one: true})
		}
	}
	flush()
	for _, b := range all {
		for _, s := range b.contract {
			chunks = append(chunks, chunk{sql: s.sql + ";", phase: engine.PhaseContract, batch: s.batch, one: true})
		}
		exp.Notes = append(exp.Notes, b.notes...)
		exp.Retired = append(exp.Retired, b.retired...)
		exp.Unretired = append(exp.Unretired, b.unretired...)
	}
	if err := exp.fill(chunks); err != nil {
		return Expansion{}, fmt.Errorf("%w: %s: %w", ErrDirective, m.ID(), err)
	}

	return exp, nil
}

type chunk struct {
	sql   string
	phase string
	batch *engine.BatchSpec
	one   bool
}

func (e *Expansion) fill(chunks []chunk) error {
	bodies := make([]string, 0, len(chunks))
	for _, c := range chunks {
		bodies = append(bodies, c.sql)
		if c.one {
			e.Phase = append(e.Phase, c.phase)
			e.Batches = append(e.Batches, c.batch)

			continue
		}
		n, err := countStatements(c.sql)
		if err != nil {
			return err
		}
		for range n {
			e.Phase = append(e.Phase, "")
			e.Batches = append(e.Batches, nil)
		}
	}
	e.UpSQL = strings.Join(bodies, "\n")

	return nil
}

func countStatements(sql string) (int, error) {
	res, err := pgquery.Parse(sql)
	if err != nil {
		return 0, fmt.Errorf("a directive must sit between whole statements: %w", err)
	}

	return len(res.Stmts), nil
}

func revertBody(m engine.Migration, all []built) (string, error) {
	var out []string
	for i, b := range all {
		if b.downWhy != "" {
			return "", refuse(m.Directives[i], "%s", b.downWhy)
		}
		out = append(out, b.down...)
	}

	return strings.Join(out, "\n"), nil
}

func heldBody(all []built) string {
	var out []string
	for _, b := range all {
		out = append(out, b.downHeld...)
	}

	return strings.Join(out, "\n")
}

func expansionHash(e Expansion) string {
	h := sha256.Sum256([]byte(e.UpSQL + "\x00" + e.DownSQL + "\x00" + e.DownHeld))

	return hex.EncodeToString(h[:])
}

func (x *Expander) keepOld(d engine.Directive) bool {
	if v, ok := d.Opts["keep-old"]; ok {
		return v == "true"
	}

	return x.KeepOld
}

// batchSize and pauseOf trust the grammar: ValidateDirective already refused a value that does not parse.
func (x *Expander) batchSize(d engine.Directive) int {
	if n, err := strconv.Atoi(d.Opts["batch"]); err == nil && n > 0 {
		return n
	}
	if x.BatchSize > 0 {
		return x.BatchSize
	}

	return DefaultBatchSize
}

func pauseOf(d engine.Directive) time.Duration {
	p, _ := time.ParseDuration(d.Opts["pause"])

	return p
}

// changeType expands the lock-safe type change: a new column kept in sync by a trigger, a batched
// backfill, and a contract phase that swaps the two.
func (x *Expander) changeType(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	col, err := resolveColumn(ctx, conn, d, d.Args[0])
	if err != nil {
		return built{}, err
	}
	newType := d.Args[1]
	newCol, oldCol := col.Column+"_new", col.Column+"_old"
	if err := col.free(ctx, conn, d, newCol, oldCol); err != nil {
		return built{}, err
	}
	if err := col.usable(d); err != nil {
		return built{}, err
	}
	if err := col.unreferenced(ctx, conn, d); err != nil {
		return built{}, err
	}
	if err := col.undepended(ctx, conn, d); err != nil {
		return built{}, err
	}
	expr := engine.Ident(col.Column) + "::" + newType
	if using, ok := d.Opts["using"]; ok {
		if err := checkUsing(ctx, conn, d, using, col.Table); err != nil {
			return built{}, err
		}
		expr = using
	}
	spec, err := x.cursor(ctx, conn, d, col)
	if err != nil {
		return built{}, err
	}
	sync := col.Table + "_" + col.Column + "_sync"
	constraint := col.Table + "_" + newCol + "_not_null"
	notNull := col.NotNull || d.Opts["not-null"] == "true"
	b := built{
		expand: []step{
			{sql: fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", col.rel(), engine.Ident(newCol), newType)},
			{sql: syncFunction(col, sync, newCol, expr)},
			{sql: syncTrigger(col, sync)},
			{sql: backfillSQL(col, spec, newCol+" = "+expr, engine.Ident(newCol)+" IS DISTINCT FROM "+expr), batch: spec},
		},
		contract: []step{
			{sql: fmt.Sprintf("DROP TRIGGER %s ON %s", engine.Ident(sync), col.rel())},
			{sql: fmt.Sprintf("DROP FUNCTION %s.%s()", engine.Ident(col.Schema), engine.Ident(sync))},
			{sql: fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s", col.rel(), engine.Ident(col.Column), engine.Ident(oldCol))},
			{sql: fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s", col.rel(), engine.Ident(newCol), engine.Ident(col.Column))},
		},
		downHeld: []string{
			fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s;", engine.Ident(sync), col.rel()),
			fmt.Sprintf("DROP FUNCTION IF EXISTS %s.%s();", engine.Ident(col.Schema), engine.Ident(sync)),
			fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", col.rel(), engine.Ident(constraint)),
			fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s;", col.rel(), engine.Ident(newCol)),
		},
	}
	if notNull {
		b.expand = append(b.expand,
			step{sql: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID",
				col.rel(), engine.Ident(constraint), engine.Ident(newCol))},
			step{sql: fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", col.rel(), engine.Ident(constraint))})
		b.contract = append(b.contract,
			step{sql: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", col.rel(), engine.Ident(col.Column))},
			step{sql: fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", col.rel(), engine.Ident(constraint))})
	}
	if col.Default != "" {
		b.expand = slices.Insert(b.expand, 1, step{sql: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s",
			col.rel(), engine.Ident(newCol), col.Default)})
		b.notes = append(b.notes, fmt.Sprintf("carries the default %s over to %s.%s; a rename does not move it",
			col.Default, col.rel(), engine.Ident(newCol)))
	}
	x.retire(&b, d, col, newCol, oldCol)
	b.notes = append(b.notes, fmt.Sprintf("backfills %s.%s in batches of %d over %s",
		col.rel(), engine.Ident(newCol), spec.Size, spec.Key))

	return b, nil
}

func (x *Expander) retire(b *built, d engine.Directive, col columnFacts, newCol, oldCol string) {
	if !x.keepOld(d) {
		b.contract = append(b.contract, step{sql: fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", col.rel(), engine.Ident(oldCol))})
		b.notes = append(b.notes, fmt.Sprintf("drops %s.%s in the contract phase: the migration becomes irreversible", col.rel(), engine.Ident(oldCol)))
		b.downWhy = "the down of a change-type with keep-old=false cannot be generated; write it by hand or keep the old column"

		return
	}
	b.retired = append(b.retired, RetiredColumn{Schema: col.Schema, Table: col.Table, Column: oldCol, Retires: col.Column})
	b.notes = append(b.notes, fmt.Sprintf("leaves %s.%s for rollback; drop it with `-- godwit: drop-column %s.%s.%s`",
		col.rel(), engine.Ident(oldCol), col.Schema, col.Table, oldCol))
	b.down = []string{
		fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s;", col.rel(), engine.Ident(col.Column), engine.Ident(newCol)),
		fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s;", col.rel(), engine.Ident(oldCol), engine.Ident(col.Column)),
		fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s;", col.rel(), engine.Ident(newCol)),
	}
}

// backfill expands a directive-driven UPDATE into one resumable batched statement.
func (x *Expander) backfill(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	tbl, err := resolveTable(ctx, conn, d, d.Args[0])
	if err != nil {
		return built{}, err
	}
	spec, err := x.cursor(ctx, conn, d, tbl)
	if err != nil {
		return built{}, err
	}
	where := "true"
	if w, ok := d.Opts["where"]; ok {
		where = w
	}

	return built{
		expand:  []step{{sql: backfillSQL(tbl, spec, d.Opts["set"], where), batch: spec}},
		notes:   []string{fmt.Sprintf("backfills %s in batches of %d over %s", tbl.rel(), spec.Size, spec.Key)},
		downWhy: "a backfill has no generated inverse; write the .down.sql by hand",
	}, nil
}

// addColumn adds the column nullable and, when it must end up NOT NULL, fills it in batches before the
// H007 recipe constrains it. The default is set in its own statement: an inline DEFAULT on a volatile
// expression rewrites the whole table under an ACCESS EXCLUSIVE lock.
func (x *Expander) addColumn(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	col, err := resolveNewColumn(ctx, conn, d, d.Args[0])
	if err != nil {
		return built{}, err
	}
	def, defaulted := d.Opts["default"]
	if d.Opts["not-null"] == "true" && !defaulted {
		return built{}, refuse(d, "%s.%s is declared not-null but has no default= to fill the rows that already exist",
			col.ref(), col.Column)
	}
	b := built{expand: []step{{sql: fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", col.rel(), engine.Ident(col.Column), d.Args[1])}}}
	if defaulted {
		b.expand = append(b.expand, step{sql: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s",
			col.rel(), engine.Ident(col.Column), def)})
	}
	if d.Opts["not-null"] == "true" {
		spec, err := x.cursor(ctx, conn, d, col)
		if err != nil {
			return built{}, err
		}
		b.expand = append(b.expand, step{
			sql:   backfillSQL(col, spec, engine.Ident(col.Column)+" = "+def, engine.Ident(col.Column)+" IS NULL"),
			batch: spec,
		})
		steps, notes, err := notNullSteps(ctx, conn, col)
		if err != nil {
			return built{}, err
		}
		b.expand = append(b.expand, steps...)
		b.notes = append(b.notes, notes...)
		b.notes = append(b.notes, fmt.Sprintf("fills %s.%s in batches of %d over %s before constraining it",
			col.rel(), engine.Ident(col.Column), spec.Size, spec.Key))
	}
	b.down = []string{fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s;", col.rel(), engine.Ident(col.Column))}
	b.downHeld = b.down

	return b, nil
}

// addNotNull constrains an existing column without the scan a bare SET NOT NULL takes.
func addNotNull(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	col, err := resolveColumn(ctx, conn, d, d.Args[0])
	if err != nil {
		return built{}, err
	}
	if col.NotNull {
		return built{}, refuse(d, "%s.%s is already NOT NULL", col.ref(), col.Column)
	}
	steps, notes, err := notNullSteps(ctx, conn, col)
	if err != nil {
		return built{}, err
	}
	down := []string{
		fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", col.rel(), engine.Ident(col.Column+"_not_null")),
		fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL;", col.rel(), engine.Ident(col.Column)),
	}

	return built{expand: steps, notes: notes, down: down, downHeld: down}, nil
}

// notNullSteps is the H007 recipe: a CHECK validated on its own lets SET NOT NULL skip the table scan.
// A CHECK already saying the same thing is reused, and only godwit's own name is dropped afterwards.
func notNullSteps(ctx context.Context, conn engine.DB, col columnFacts) ([]step, []string, error) {
	generated := col.Column + "_not_null"
	name, valid, found, err := notNullCheck(ctx, conn, col)
	if err != nil {
		return nil, nil, err
	}
	var steps []step
	var notes []string
	if found {
		notes = append(notes, fmt.Sprintf("reuses the CHECK %s already on %s.%s instead of adding one",
			engine.Ident(name), col.rel(), engine.Ident(col.Column)))
	} else {
		name = generated
		steps = append(steps, step{sql: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID",
			col.rel(), engine.Ident(name), engine.Ident(col.Column))})
	}
	if !valid {
		steps = append(steps, step{sql: fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", col.rel(), engine.Ident(name))})
	}
	steps = append(steps, step{sql: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", col.rel(), engine.Ident(col.Column))})
	if name == generated {
		steps = append(steps, step{sql: fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", col.rel(), engine.Ident(name))})
	}

	return steps, notes, nil
}

// notNullCheck finds a single-column CHECK on col that already says it is not null.
func notNullCheck(ctx context.Context, conn engine.DB, col columnFacts) (string, bool, bool, error) {
	var name string
	var valid bool
	err := conn.QueryRow(ctx, `
		SELECT k.conname, k.convalidated
		FROM pg_constraint k
		JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		WHERE k.conrelid = to_regclass($1) AND k.contype = 'c' AND k.conkey = ARRAY[a.attnum]
		  AND replace(replace(replace(pg_get_constraintdef(k.oid), ' NOT VALID', ''), '(', ''), ')', '')
		      = 'CHECK ' || quote_ident($2) || ' IS NOT NULL'
		ORDER BY k.conname LIMIT 1`, col.rel(), col.Column).Scan(&name, &valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("inspect %s.%s check constraints: %w", col.ref(), col.Column, err)
	}

	return name, valid, true, nil
}

// addIndex builds the index without blocking writes, clearing first the invalid leftover a previous
// CREATE INDEX CONCURRENTLY leaves behind when it is interrupted.
func addIndex(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	tbl, err := resolveTable(ctx, conn, d, d.Args[0])
	if err != nil {
		return built{}, err
	}
	name := d.Opts["name"]
	if name == "" {
		name = tbl.Table + "_" + strings.Join(indexColumns(d.Args[1]), "_") + "_idx"
	}
	drop := fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s", quoteRef(tbl.Schema, name))
	b := built{down: []string{drop + ";"}}
	b.downHeld = b.down
	leftover, err := invalidIndex(ctx, conn, d, tbl, name)
	if err != nil {
		return built{}, err
	}
	if leftover {
		b.expand = append(b.expand, step{sql: drop})
		b.notes = append(b.notes, fmt.Sprintf("drops the invalid %s left by an interrupted index build before rebuilding it",
			quoteRef(tbl.Schema, name)))
	}
	head := "CREATE INDEX CONCURRENTLY"
	if d.Opts["unique"] == "true" {
		head = "CREATE UNIQUE INDEX CONCURRENTLY"
	}
	sql := fmt.Sprintf("%s %s ON %s", head, engine.Ident(name), tbl.rel())
	if using, ok := d.Opts["using"]; ok {
		sql += " USING " + engine.Ident(using)
	}
	sql += " " + d.Args[1]
	if where, ok := d.Opts["where"]; ok {
		sql += " WHERE " + where
	}
	b.expand = append(b.expand, step{sql: sql})

	return b, nil
}

// indexColumns names the parts of a column list the grammar has already parsed; an expression is "expr",
// which is how the H010 recipe names it too.
func indexColumns(cols string) []string {
	res, err := pgquery.Parse("CREATE INDEX ON t " + cols)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range res.Stmts[0].Stmt.GetIndexStmt().GetIndexParams() {
		name := p.GetIndexElem().GetName()
		if name == "" {
			name = "expr"
		}
		out = append(out, name)
	}

	return out
}

func invalidIndex(ctx context.Context, conn engine.DB, d engine.Directive, tbl columnFacts, name string) (bool, error) {
	var relkind string
	var valid bool
	err := conn.QueryRow(ctx, `
		SELECT c.relkind, coalesce(i.indisvalid, false)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_index i ON i.indexrelid = c.oid
		WHERE n.nspname = $1 AND c.relname = $2`, tbl.Schema, name).Scan(&relkind, &valid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", quoteRef(tbl.Schema, name), err)
	}
	if relkind != "i" {
		return false, refuse(d, "%s already exists and is not an index (relkind %s)", quoteRef(tbl.Schema, name), relkind)
	}
	if valid {
		return false, refuse(d, "index %s already exists; drop it first or pass name=", quoteRef(tbl.Schema, name))
	}

	return true, nil
}

// dropIndex removes the index without blocking reads; IF EXISTS makes the retry after an interrupted
// concurrent drop a no-op.
func dropIndex(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	schema, name := splitRef(d.Args[0])
	var nspname, relkind, constraint string
	err := conn.QueryRow(ctx, `
		SELECT n.nspname, c.relkind, coalesce(k.conname, '')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_constraint k ON k.conindid = c.oid
		WHERE c.oid = to_regclass($1)`, quoteRef(schema, name)).Scan(&nspname, &relkind, &constraint)
	if errors.Is(err, pgx.ErrNoRows) {
		return built{}, refuse(d, "%s does not exist in the schema this migration starts from", d.Args[0])
	}
	if err != nil {
		return built{}, fmt.Errorf("inspect %s: %w", d.Args[0], err)
	}
	if relkind != "i" {
		return built{}, refuse(d, "%s is not an index (relkind %s)", d.Args[0], relkind)
	}
	if constraint != "" {
		return built{}, refuse(d, "%s backs the constraint %s; drop the constraint instead", d.Args[0], constraint)
	}

	return built{
		expand:  []step{{sql: "DROP INDEX CONCURRENTLY IF EXISTS " + quoteRef(nspname, name)}},
		downWhy: "a drop-index has no generated inverse; write the CREATE INDEX in the .down.sql by hand",
	}, nil
}

var fkDeleteActions = map[string]string{
	"cascade": "CASCADE", "restrict": "RESTRICT", "set-null": "SET NULL",
	"set-default": "SET DEFAULT", "no-action": "NO ACTION",
}

// addForeignKey adds the constraint unvalidated so that only the VALIDATE reads the rows, under a lock
// that lets writes through.
func addForeignKey(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	col, err := resolveColumn(ctx, conn, d, d.Args[0])
	if err != nil {
		return built{}, err
	}
	ref, err := resolveColumn(ctx, conn, d, d.Args[2])
	if err != nil {
		return built{}, err
	}
	name := d.Opts["name"]
	if name == "" {
		name = col.Table + "_" + col.Column + "_fkey"
	}
	if err := col.constraintFree(ctx, conn, d, name); err != nil {
		return built{}, err
	}
	unique, err := uniquelyIndexed(ctx, conn, ref)
	if err != nil {
		return built{}, err
	}
	if !unique {
		return built{}, refuse(d, "%s.%s has no single-column unique index; PostgreSQL cannot point a foreign key at it",
			ref.ref(), ref.Column)
	}
	sql := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
		col.rel(), engine.Ident(name), engine.Ident(col.Column), ref.rel(), engine.Ident(ref.Column))
	if action, ok := fkDeleteActions[d.Opts["on-delete"]]; ok {
		sql += " ON DELETE " + action
	}

	return constraintSteps(col, name, sql), nil
}

// addCheck adds the constraint unvalidated so the scan never holds the lock that blocks writes.
func addCheck(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	tbl, err := resolveTable(ctx, conn, d, d.Args[0])
	if err != nil {
		return built{}, err
	}
	name := d.Args[1]
	if err := tbl.constraintFree(ctx, conn, d, name); err != nil {
		return built{}, err
	}

	return constraintSteps(tbl, name, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s)",
		tbl.rel(), engine.Ident(name), d.Args[2])), nil
}

func constraintSteps(t columnFacts, name, add string) built {
	down := []string{fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;", t.rel(), engine.Ident(name))}

	return built{
		expand: []step{
			{sql: add + " NOT VALID"},
			{sql: fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", t.rel(), engine.Ident(name))},
		},
		down:     down,
		downHeld: down,
	}
}

// dropColumn is the one operation in the contract phase: the column goes only after a human confirms the
// application that reads it is gone.
func dropColumn(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	col, err := resolveColumn(ctx, conn, d, d.Args[0])
	if err != nil {
		return built{}, err
	}
	if err := col.droppable(ctx, conn, d); err != nil {
		return built{}, err
	}

	return built{
		contract:  []step{{sql: fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", col.rel(), engine.Ident(col.Column))}},
		unretired: []RetiredColumn{{Schema: col.Schema, Table: col.Table, Column: col.Column}},
		notes: []string{fmt.Sprintf("drops %s.%s in the contract phase; deploy the application version that no longer reads it first",
			col.rel(), engine.Ident(col.Column))},
		downWhy: "a drop-column has no generated inverse; the rows go with the column",
	}, nil
}

func (c columnFacts) constraintFree(ctx context.Context, conn engine.DB, d engine.Directive, name string) error {
	var n int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM pg_constraint WHERE conrelid = to_regclass($1) AND conname = $2`,
		c.rel(), name).Scan(&n); err != nil {
		return fmt.Errorf("inspect %s constraints: %w", c.ref(), err)
	}
	if n > 0 {
		return refuse(d, "%s already has a constraint named %s; pass name= to choose another", c.ref(), name)
	}

	return nil
}

func uniquelyIndexed(ctx context.Context, conn engine.DB, c columnFacts) (bool, error) {
	var ok bool
	if err := conn.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
			WHERE i.indrelid = to_regclass($1) AND i.indisunique AND i.indnatts = 1 AND i.indpred IS NULL
			  AND a.attname = $2)`, c.rel(), c.Column).Scan(&ok); err != nil {
		return false, fmt.Errorf("inspect %s.%s unique indexes: %w", c.ref(), c.Column, err)
	}

	return ok, nil
}

// backfillSQL renders one batch: the cursor picks at most Size keys, the update touches those rows and
// returns their keys so the executor can advance and resume.
func backfillSQL(t columnFacts, spec *engine.BatchSpec, set, where string) string {
	return fmt.Sprintf(
		"WITH b AS (SELECT %s AS %s FROM %s WHERE %s > %s AND (%s) ORDER BY %s LIMIT %d)\n"+
			"UPDATE %s AS t SET %s FROM b WHERE t.%s = b.%s RETURNING b.%s",
		spec.Key, batchKeyAlias, t.rel(), spec.Key, cursorParam(spec.KeyKind), where, spec.Key, spec.Size,
		t.rel(), set, spec.Key, batchKeyAlias, batchKeyAlias)
}

// cursorParam casts the journalled cursor to its own type: a key narrower than bigint would otherwise
// refuse the int8 the executor binds.
func cursorParam(kind string) string {
	switch kind {
	case engine.BatchKeyInt:
		return "$1::bigint"
	case engine.BatchKeyUUID:
		return "$1::uuid"
	default:
		return "$1::text"
	}
}

func syncFunction(col columnFacts, sync, newCol, expr string) string {
	return fmt.Sprintf("CREATE FUNCTION %s.%s() RETURNS trigger LANGUAGE plpgsql AS $godwit$"+
		" BEGIN SELECT %s INTO new.%s FROM (SELECT new.*) AS %s; RETURN new; END $godwit$",
		engine.Ident(col.Schema), engine.Ident(sync), expr, engine.Ident(newCol), engine.Ident(col.Table))
}

func syncTrigger(col columnFacts, sync string) string {
	return fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION %s.%s()",
		engine.Ident(sync), col.rel(), engine.Ident(col.Schema), engine.Ident(sync))
}

// columnFacts is what the scratch catalog says about the object a directive names.
type columnFacts struct {
	Schema    string
	Table     string
	Column    string
	RelKind   string
	Type      string
	Default   string
	NotNull   bool
	Identity  bool
	Generated bool
}

func (c columnFacts) rel() string {
	return engine.Ident(c.Schema) + "." + engine.Ident(c.Table)
}

func (c columnFacts) ref() string {
	return c.Schema + "." + c.Table
}

func (c columnFacts) usable(d engine.Directive) error {
	switch {
	case c.Identity:
		return refuse(d, "%s.%s is an identity column; its sequence stays bound to the physical attribute across the rename", c.ref(), c.Column)
	case c.Generated:
		return refuse(d, "%s.%s is a generated column; its expression stays bound to the physical attribute across the rename", c.ref(), c.Column)
	default:
		return nil
	}
}

func splitRef(ref string) (string, string) {
	parts := strings.Split(ref, ".")
	if len(parts) == 1 {
		return "", parts[0]
	}

	return parts[len(parts)-2], parts[len(parts)-1]
}

func quoteRef(schema, name string) string {
	if schema == "" {
		return engine.Ident(name)
	}

	return engine.Ident(schema) + "." + engine.Ident(name)
}

func resolveTable(ctx context.Context, conn engine.DB, d engine.Directive, ref string) (columnFacts, error) {
	schema, table := splitRef(ref)
	var facts columnFacts
	err := conn.QueryRow(ctx, `
		SELECT n.nspname, c.relname, c.relkind
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass($1)`, quoteRef(schema, table)).Scan(&facts.Schema, &facts.Table, &facts.RelKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return columnFacts{}, refuse(d, "%s does not exist in the schema this migration starts from", ref)
	}
	if err != nil {
		return columnFacts{}, fmt.Errorf("inspect %s: %w", ref, err)
	}
	if facts.RelKind == "p" {
		return columnFacts{}, refuse(d, "%s is partitioned; the swap would have to run per partition", facts.ref())
	}
	if facts.RelKind != "r" {
		return columnFacts{}, refuse(d, "%s is not an ordinary table (relkind %s)", facts.ref(), facts.RelKind)
	}

	return facts, nil
}

func resolveColumn(ctx context.Context, conn engine.DB, d engine.Directive, ref string) (columnFacts, error) {
	parts := strings.Split(ref, ".")
	facts, err := resolveTable(ctx, conn, d, strings.Join(parts[:len(parts)-1], "."))
	if err != nil {
		return columnFacts{}, err
	}
	facts.Column = parts[len(parts)-1]
	err = conn.QueryRow(ctx, `
		SELECT format_type(a.atttypid, a.atttypmod), coalesce(pg_get_expr(ad.adbin, ad.adrelid), ''),
		       a.attnotnull, a.attidentity <> '', a.attgenerated <> ''
		FROM pg_attribute a
		LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		WHERE a.attrelid = to_regclass($1) AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped`,
		facts.rel(), facts.Column).Scan(&facts.Type, &facts.Default, &facts.NotNull, &facts.Identity, &facts.Generated)
	if errors.Is(err, pgx.ErrNoRows) {
		return columnFacts{}, refuse(d, "%s.%s does not exist in the schema this migration starts from", facts.ref(), facts.Column)
	}
	if err != nil {
		return columnFacts{}, fmt.Errorf("inspect %s.%s: %w", facts.ref(), facts.Column, err)
	}

	return facts, nil
}

// resolveNewColumn locates the table of a column the migration is about to create, and refuses the name
// the table already carries.
func resolveNewColumn(ctx context.Context, conn engine.DB, d engine.Directive, ref string) (columnFacts, error) {
	parts := strings.Split(ref, ".")
	facts, err := resolveTable(ctx, conn, d, strings.Join(parts[:len(parts)-1], "."))
	if err != nil {
		return columnFacts{}, err
	}
	facts.Column = parts[len(parts)-1]
	if err := facts.free(ctx, conn, d, facts.Column); err != nil {
		return columnFacts{}, err
	}

	return facts, nil
}

func (c columnFacts) free(ctx context.Context, conn engine.DB, d engine.Directive, names ...string) error {
	rows, err := conn.Query(ctx, `
		SELECT a.attname FROM pg_attribute a
		WHERE a.attrelid = to_regclass($1) AND a.attname = ANY($2) AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attname`, c.rel(), names)
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", c.ref(), err)
	}
	taken, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read %s columns: %w", c.ref(), err)
	}
	if len(taken) > 0 {
		return refuse(d, "%s.%s already exists; the expansion would collide with it", c.ref(), taken[0])
	}

	return nil
}

// dependentsSQL lists every catalog object bound to one column. `auto` decides what a DROP COLUMN does to it:
// PostgreSQL destroys an auto dependent silently and refuses to drop a column a normal one reads. `covers` is
// how many of the table's columns the object spans, which separates an index existing only for this column from
// one that also serves others. The column's own default, and its NOT NULL constraint on PostgreSQL 18, are
// excluded: neither is another object.
const dependentsSQL = `
WITH col AS (
	SELECT a.attrelid AS relid, a.attnum
	FROM pg_attribute a
	WHERE a.attrelid = to_regclass($1) AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped
), dep AS (
	SELECT d.classid, d.objid,
	       bool_or(d.deptype = 'a') AS auto,
	       (SELECT count(DISTINCT d2.refobjsubid) FROM pg_depend d2
	        WHERE d2.classid = d.classid AND d2.objid = d.objid
	          AND d2.refclassid = 'pg_class'::regclass AND d2.refobjid = c.relid AND d2.refobjsubid > 0) AS covers
	FROM pg_depend d, col c
	WHERE d.refclassid = 'pg_class'::regclass AND d.refobjid = c.relid AND d.refobjsubid = c.attnum
	  AND d.deptype IN ('a', 'n')
	  AND NOT (d.classid = 'pg_attrdef'::regclass
	           AND EXISTS (SELECT 1 FROM pg_attrdef ad WHERE ad.oid = d.objid AND ad.adnum = c.attnum))
	  AND NOT (d.classid = 'pg_constraint'::regclass
	           AND EXISTS (SELECT 1 FROM pg_constraint k WHERE k.oid = d.objid AND k.contype = 'n'))
	GROUP BY d.classid, d.objid, c.relid
)
SELECT
	CASE
		WHEN dep.classid = 'pg_rewrite'::regclass THEN
			CASE rwc.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' ELSE 'rule' END
		WHEN dep.classid = 'pg_class'::regclass THEN
			CASE cl.relkind WHEN 'i' THEN 'index' WHEN 'S' THEN 'sequence' ELSE 'relation' END
		WHEN dep.classid = 'pg_constraint'::regclass THEN
			CASE co.contype WHEN 'f' THEN 'foreign key' WHEN 'p' THEN 'primary key' WHEN 'u' THEN 'unique constraint'
			                WHEN 'c' THEN 'check constraint' WHEN 'x' THEN 'exclusion constraint' ELSE 'constraint' END
		WHEN dep.classid = 'pg_trigger'::regclass THEN 'trigger'
		WHEN dep.classid = 'pg_attrdef'::regclass THEN
			CASE WHEN ada.attgenerated <> '' THEN 'generated column' ELSE 'column default' END
		WHEN dep.classid = 'pg_policy'::regclass THEN 'row security policy'
		WHEN dep.classid = 'pg_statistic_ext'::regclass THEN 'statistics object'
		WHEN dep.classid = 'pg_publication_rel'::regclass THEN 'publication'
		ELSE 'object'
	END,
	CASE
		WHEN dep.classid = 'pg_rewrite'::regclass AND rwc.relkind IN ('v', 'm') THEN rwn.nspname || '.' || rwc.relname
		WHEN dep.classid = 'pg_rewrite'::regclass THEN rw.rulename || ' on ' || rwn.nspname || '.' || rwc.relname
		WHEN dep.classid = 'pg_class'::regclass THEN cln.nspname || '.' || cl.relname
		WHEN dep.classid = 'pg_constraint'::regclass THEN co.conname || ' on ' || con.nspname || '.' || cot.relname
		WHEN dep.classid = 'pg_trigger'::regclass THEN tg.tgname || ' on ' || tgn.nspname || '.' || tgt.relname
		WHEN dep.classid = 'pg_attrdef'::regclass THEN adsn.nspname || '.' || adt.relname || '.' || ada.attname
		WHEN dep.classid = 'pg_policy'::regclass THEN po.polname || ' on ' || pon.nspname || '.' || pot.relname
		WHEN dep.classid = 'pg_statistic_ext'::regclass THEN stn.nspname || '.' || st.stxname
		WHEN dep.classid = 'pg_publication_rel'::regclass THEN pu.pubname
		ELSE dep.classid::regclass::text || ' ' || dep.objid::text
	END,
	dep.auto, dep.covers
FROM dep
LEFT JOIN pg_rewrite rw ON dep.classid = 'pg_rewrite'::regclass AND rw.oid = dep.objid
LEFT JOIN pg_class rwc ON rwc.oid = rw.ev_class
LEFT JOIN pg_namespace rwn ON rwn.oid = rwc.relnamespace
LEFT JOIN pg_class cl ON dep.classid = 'pg_class'::regclass AND cl.oid = dep.objid
LEFT JOIN pg_namespace cln ON cln.oid = cl.relnamespace
LEFT JOIN pg_constraint co ON dep.classid = 'pg_constraint'::regclass AND co.oid = dep.objid
LEFT JOIN pg_class cot ON cot.oid = co.conrelid
LEFT JOIN pg_namespace con ON con.oid = cot.relnamespace
LEFT JOIN pg_trigger tg ON dep.classid = 'pg_trigger'::regclass AND tg.oid = dep.objid
LEFT JOIN pg_class tgt ON tgt.oid = tg.tgrelid
LEFT JOIN pg_namespace tgn ON tgn.oid = tgt.relnamespace
LEFT JOIN pg_attrdef ad ON dep.classid = 'pg_attrdef'::regclass AND ad.oid = dep.objid
LEFT JOIN pg_class adt ON adt.oid = ad.adrelid
LEFT JOIN pg_namespace adsn ON adsn.oid = adt.relnamespace
LEFT JOIN pg_attribute ada ON ada.attrelid = ad.adrelid AND ada.attnum = ad.adnum
LEFT JOIN pg_policy po ON dep.classid = 'pg_policy'::regclass AND po.oid = dep.objid
LEFT JOIN pg_class pot ON pot.oid = po.polrelid
LEFT JOIN pg_namespace pon ON pon.oid = pot.relnamespace
LEFT JOIN pg_statistic_ext st ON dep.classid = 'pg_statistic_ext'::regclass AND st.oid = dep.objid
LEFT JOIN pg_namespace stn ON stn.oid = st.stxnamespace
LEFT JOIN pg_publication_rel pr ON dep.classid = 'pg_publication_rel'::regclass AND pr.oid = dep.objid
LEFT JOIN pg_publication pu ON pu.oid = pr.prpubid
ORDER BY 1, 2`

type dependent struct {
	Kind   string
	Name   string
	Auto   bool
	Covers int
}

func (o dependent) String() string {
	return o.Kind + " " + o.Name
}

func namesOf(deps []dependent) string {
	out := make([]string, len(deps))
	for i, o := range deps {
		out[i] = o.String()
	}

	return strings.Join(out, ", ")
}

func verb(deps []dependent) string {
	if len(deps) == 1 {
		return "depends"
	}

	return "depend"
}

func (c columnFacts) dependents(ctx context.Context, conn engine.DB) ([]dependent, error) {
	rows, err := conn.Query(ctx, dependentsSQL, c.rel(), c.Column)
	if err != nil {
		return nil, fmt.Errorf("inspect %s.%s dependencies: %w", c.ref(), c.Column, err)
	}
	deps, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dependent])
	if err != nil {
		return nil, fmt.Errorf("read %s.%s dependencies: %w", c.ref(), c.Column, err)
	}

	return deps, nil
}

// undepended refuses a swap when anything else is bound to the column. A rename moves every dependency
// with the physical attribute, so each dependent silently ends up reading <c>_old.
func (c columnFacts) undepended(ctx context.Context, conn engine.DB, d engine.Directive) error {
	deps, err := c.dependents(ctx, conn)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		return nil
	}

	return refuse(d, "%s %s on %s.%s; the swap renames the column and PostgreSQL moves every dependent with "+
		"the physical attribute, so each one would silently keep reading %s.%s_old. Drop and recreate them around "+
		"this migration, in their own migrations",
		namesOf(deps), verb(deps), c.ref(), c.Column, c.ref(), c.Column)
}

// droppable refuses a DROP COLUMN that PostgreSQL would reject outright, and one that would silently take
// an object also covering other columns. An auto dependent spanning this column alone goes with it by design.
func (c columnFacts) droppable(ctx context.Context, conn engine.DB, d engine.Directive) error {
	deps, err := c.dependents(ctx, conn)
	if err != nil {
		return err
	}
	var blocking, widened []dependent
	for _, o := range deps {
		switch {
		case !o.Auto:
			blocking = append(blocking, o)
		case o.Covers > 1:
			widened = append(widened, o)
		}
	}
	if len(blocking) > 0 {
		return refuse(d, "%s %s on %s.%s; PostgreSQL refuses to drop a column another object reads, so the "+
			"contract phase would fail after you confirmed it. Drop them first, in their own migrations",
			namesOf(blocking), verb(blocking), c.ref(), c.Column)
	}
	if len(widened) > 0 {
		return refuse(d, "%s %s on %s.%s and also covers other columns; the drop would take it with the column "+
			"and silently lose what it gives them. Replace it first, in its own migration",
			namesOf(widened), verb(widened), c.ref(), c.Column)
	}

	return nil
}

func (c columnFacts) unreferenced(ctx context.Context, conn engine.DB, d engine.Directive) error {
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint k, pg_attribute a
		WHERE a.attrelid = to_regclass($1) AND a.attname = $2 AND k.contype = 'f'
		  AND ((k.conrelid = a.attrelid AND a.attnum = ANY (k.conkey))
		    OR (k.confrelid = a.attrelid AND a.attnum = ANY (k.confkey)))`,
		c.rel(), c.Column).Scan(&n); err != nil {
		return fmt.Errorf("inspect %s.%s foreign keys: %w", c.ref(), c.Column, err)
	}
	if n > 0 {
		return refuse(d, "%s.%s takes part in a foreign key; the constraint would still point at the renamed column", c.ref(), c.Column)
	}

	return nil
}

// cursor picks the batching key: the directive's key= or the table's single-column primary key.
func (x *Expander) cursor(ctx context.Context, conn engine.DB, d engine.Directive, t columnFacts) (*engine.BatchSpec, error) {
	key, ok := d.Opts["key"]
	if !ok {
		var err error
		if key, err = primaryKey(ctx, conn, d, t); err != nil {
			return nil, err
		}
	}
	kind, err := keyKind(ctx, conn, d, t, key)
	if err != nil {
		return nil, err
	}
	return &engine.BatchSpec{
		Key: engine.Ident(key), KeyKind: kind, Size: x.batchSize(d), Pause: pauseOf(d),
		Estimate: "SELECT greatest(coalesce((SELECT c.reltuples FROM pg_class c WHERE c.oid = to_regclass(" +
			quoteLiteral(t.rel()) + ")), 0)::bigint, 0)",
	}, nil
}

// keyed lists the operations whose directive can override the batching key.
var keyed = map[string]bool{"change-type": true, "backfill": true}

func keyAdvice(op string) string {
	if keyed[op] {
		return "; pass key=<column>"
	}

	return ""
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func primaryKey(ctx context.Context, conn engine.DB, d engine.Directive, t columnFacts) (string, error) {
	var key string
	err := conn.QueryRow(ctx, `
		SELECT a.attname FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
		WHERE i.indrelid = to_regclass($1) AND i.indisprimary AND i.indnatts = 1`, t.rel()).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", refuse(d, "%s has no single-column primary key to batch on%s", t.ref(), keyAdvice(d.Op))
	}
	if err != nil {
		return "", fmt.Errorf("inspect %s primary key: %w", t.ref(), err)
	}

	return key, nil
}

var keyKinds = map[string]string{
	"int2": engine.BatchKeyInt, "int4": engine.BatchKeyInt, "int8": engine.BatchKeyInt,
	"uuid": engine.BatchKeyUUID, "text": engine.BatchKeyText, "varchar": engine.BatchKeyText, "bpchar": engine.BatchKeyText,
}

func keyKind(ctx context.Context, conn engine.DB, d engine.Directive, t columnFacts, key string) (string, error) {
	var typname string
	var notNull, unique bool
	err := conn.QueryRow(ctx, `
		SELECT y.typname, a.attnotnull, EXISTS (
			SELECT 1 FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid JOIN pg_am m ON m.oid = ic.relam
			WHERE i.indrelid = a.attrelid AND i.indisunique AND i.indnatts = 1 AND i.indkey[0] = a.attnum
			  AND i.indpred IS NULL AND m.amname = 'btree')
		FROM pg_attribute a JOIN pg_type y ON y.oid = a.atttypid
		WHERE a.attrelid = to_regclass($1) AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped`,
		t.rel(), key).Scan(&typname, &notNull, &unique)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", refuse(d, "key %s does not exist on %s", key, t.ref())
	}
	if err != nil {
		return "", fmt.Errorf("inspect key %s.%s: %w", t.ref(), key, err)
	}
	switch kind, known := keyKinds[typname]; {
	case !notNull:
		return "", refuse(d, "key %s.%s is nullable; a cursor over it would skip rows", t.ref(), key)
	case !unique:
		return "", refuse(d, "key %s.%s has no single-column unique btree index; a cursor over it can skip or repeat rows", t.ref(), key)
	case !known:
		return "", refuse(d, "key %s.%s has type %s; a batch cursor needs an integer, uuid or text key", t.ref(), key, typname)
	default:
		return kind, nil
	}
}

// checkUsing refuses an expression the trigger form cannot carry: another table, a subquery, or a
// function the catalog reports as VOLATILE.
func checkUsing(ctx context.Context, conn engine.DB, d engine.Directive, using, table string) error {
	res, err := pgquery.Parse("SELECT " + using)
	if err != nil {
		return refuse(d, "using=%s does not parse: %v", using, err)
	}
	names, err := scanUsing(res.Stmts[0].Stmt.GetSelectStmt().GetTargetList()[0].GetResTarget().GetVal(), table)
	if err != nil {
		return refuse(d, "using=%s %s", using, err)
	}
	if len(names) == 0 {
		return nil
	}
	var volatile string
	err = conn.QueryRow(ctx, `
		SELECT p.proname FROM pg_proc p WHERE p.provolatile = 'v' AND p.proname = ANY($1) LIMIT 1`, names).Scan(&volatile)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect volatility of %s: %w", using, err)
	}

	return refuse(d, "using=%s calls the VOLATILE function %s(); the trigger and the backfill would disagree", using, volatile)
}

func scanUsing(root *pgquery.Node, table string) ([]string, error) {
	var names []string
	var walk func(v *pgquery.Node) error
	walk = func(v *pgquery.Node) error {
		switch {
		case v == nil:
		case v.GetSubLink() != nil:
			return errors.New("contains a subquery; the trigger form cannot express it")
		case v.GetColumnRef() != nil:
			return checkColumnRef(v.GetColumnRef(), table)
		case v.GetFuncCall() != nil:
			f := v.GetFuncCall()
			names = append(names, f.Funcname[len(f.Funcname)-1].GetString_().GetSval())
			for _, a := range f.Args {
				if err := walk(a); err != nil {
					return err
				}
			}
		case v.GetTypeCast() != nil:
			return walk(v.GetTypeCast().Arg)
		case v.GetAExpr() != nil:
			if err := walk(v.GetAExpr().Lexpr); err != nil {
				return err
			}

			return walk(v.GetAExpr().Rexpr)
		case v.GetCoalesceExpr() != nil:
			for _, a := range v.GetCoalesceExpr().Args {
				if err := walk(a); err != nil {
					return err
				}
			}
		}

		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}

	return names, nil
}

func checkColumnRef(ref *pgquery.ColumnRef, table string) error {
	if len(ref.Fields) == 1 {
		return nil
	}
	if ref.Fields[0].GetString_().GetSval() == table {
		return nil
	}

	return fmt.Errorf("references %s; only columns of %s are in scope inside the trigger",
		ref.Fields[0].GetString_().GetSval(), table)
}

// ExpandUp replaces each directive migration's body with the expansion frozen on the plan or the run,
// so what executes is byte for byte what the reviewer saw.
func ExpandUp(plans []engine.Plan, exps map[string]Expansion) ([]engine.Plan, error) {
	return substitute(plans, exps, func(e Expansion) string { return e.UpSQL })
}

// ExpandDown substitutes each migration's own frozen inverse, taken from the ledger of what the run
// applied; a migration whose contract phase never ran gets the pre-swap form. A hand-written
// .down.sql has no expansion and wins untouched.
func ExpandDown(plans []engine.Plan, applied []RunMigration) ([]engine.Plan, error) {
	exps := make(map[string]Expansion, len(applied))
	for _, m := range applied {
		if m.Expansion == nil {
			continue
		}
		e := *m.Expansion
		if m.Held {
			e.DownSQL = e.DownHeld
		}
		exps[m.Migration] = e
	}

	return substitute(plans, exps, func(e Expansion) string { return e.DownSQL })
}

func substitute(plans []engine.Plan, exps map[string]Expansion, body func(Expansion) string) ([]engine.Plan, error) {
	out := slices.Clone(plans)
	for i, p := range out {
		exp, ok := exps[p.Migration.ID()]
		if !ok || body(exp) == "" {
			continue
		}
		if p.Direction == engine.DirectionDown {
			exp.DownSQL = body(exp)
		}
		next, err := ExpandPlan(p, exp)
		if err != nil {
			return nil, err
		}
		out[i] = next
	}

	return out, nil
}

// Unretired lists every column the expansions of one run remove, so the ones a change-type had retired
// stop being reported as a rollback the target still holds.
func Unretired(exps map[string]Expansion) []RetiredColumn {
	var out []RetiredColumn
	for _, e := range exps {
		out = append(out, e.Unretired...)
	}
	slices.SortFunc(out, func(a, b RetiredColumn) int { return strings.Compare(a.String(), b.String()) })

	return out
}

// Retired lists every column the expansions of one run leave behind, keyed by migration.
func Retired(exps map[string]Expansion) map[string][]RetiredColumn {
	out := map[string][]RetiredColumn{}
	for id, e := range exps {
		if len(e.Retired) > 0 {
			out[id] = e.Retired
		}
	}

	return out
}
