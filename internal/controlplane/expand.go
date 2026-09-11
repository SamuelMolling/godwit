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
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// ErrDirective marks a directive godwit will not expand; the API reports it as invalid_argument.
var ErrDirective = errors.New("godwit directive")

// DefaultBatchSize is how many rows a generated backfill touches per transaction when the directive is silent.
const DefaultBatchSize = 5000

const batchKeyAlias = "godwit_key"

const backfillSyncSuffix = "_backfill_sync"

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

// Expansion is the SQL godwit generates for one migration's directives, frozen into the plan so the run applies what the pull request showed.
type Expansion struct {
	ID        string               `json:"id"`
	UpSQL     string               `json:"up_sql"`
	DownSQL   string               `json:"down_sql"`
	DownHeld  string               `json:"down_held_sql,omitempty"`
	Phase     []string             `json:"phase"`
	Batches   []*engine.BatchSpec  `json:"batches,omitempty"`
	Asserts   []*engine.AssertSpec `json:"asserts,omitempty"`
	Notes     []string             `json:"notes,omitempty"`
	Retired   []RetiredColumn      `json:"retired,omitempty"`
	Unretired []RetiredColumn      `json:"unretired,omitempty"`
	Lines     []string             `json:"lines,omitempty"`
	Hash      string               `json:"hash"`
}

// Contract is the index of the first contract statement, or -1 when the expansion has one phase.
func (e Expansion) Contract() int {
	return slices.Index(e.Phase, engine.PhaseContract)
}

// Expansions frozen before assertions existed carry none.
func (e Expansion) assertAt(i int) *engine.AssertSpec {
	if i >= len(e.Asserts) {
		return nil
	}

	return e.Asserts[i]
}

// Expander turns directives into statements using a catalog that already holds the target's history.
type Expander struct {
	KeepOld   bool
	BatchSize int
}

// NewExpander returns an Expander with godwit's defaults.
func NewExpander() *Expander {
	return &Expander{KeepOld: true, BatchSize: DefaultBatchSize}
}

type step struct {
	sql    string
	batch  *engine.BatchSpec
	assert *engine.AssertSpec
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
	case engine.DirectiveAssert:
		return assertion(d)
	default:
		return built{}, refuse(d, "%s has no expansion", d.Op)
	}
}

func refuse(d engine.Directive, format string, args ...any) error {
	return fmt.Errorf("%w on line %d (%s): %s", ErrDirective, d.Line, d.Text, fmt.Sprintf(format, args...))
}

var contractHazardCodes = []string{"H002", "H003", "H008"}

func checkDestructive(m engine.Migration) error {
	if !slices.ContainsFunc(m.Directives, func(d engine.Directive) bool { return d.Op != engine.DirectiveAssert }) {
		return nil
	}
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
		chunks = append(chunks, chunk{sql: expandedHeader(m.Directives[i])})
		for _, s := range all[i].expand {
			chunks = append(chunks, chunk{sql: s.sql + ";", phase: engine.PhaseExpand, batch: s.batch, assert: s.assert, one: true})
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

func expandedHeader(d engine.Directive) string {
	args := d.Args
	if d.Op == engine.DirectiveAssert {
		args = []string{quoteLiteral(d.Args[0]), d.Args[1], d.Args[2]}
	}

	return engine.ExpandedMarker + d.Op + " " + strings.Join(args, " ")
}

type chunk struct {
	sql    string
	phase  string
	batch  *engine.BatchSpec
	assert *engine.AssertSpec
	one    bool
}

func (e *Expansion) fill(chunks []chunk) error {
	bodies := make([]string, 0, len(chunks))
	for _, c := range chunks {
		bodies = append(bodies, c.sql)
		if c.one {
			e.Phase = append(e.Phase, c.phase)
			e.Batches = append(e.Batches, c.batch)
			e.Asserts = append(e.Asserts, c.assert)

			continue
		}
		n, err := countStatements(c.sql)
		if err != nil {
			return err
		}
		for range n {
			e.Phase = append(e.Phase, "")
			e.Batches = append(e.Batches, nil)
			e.Asserts = append(e.Asserts, nil)
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
	if len(out) == 0 && len(m.Directives) > 0 {
		return "", refuse(m.Directives[0], "an assertion has no inverse; drop the -- godwit: revert sentinel or write the .down.sql by hand")
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
	body := e.UpSQL + "\x00" + e.DownSQL + "\x00" + e.DownHeld
	for i, a := range e.Asserts {
		if a != nil {
			body += fmt.Sprintf("\x00assert %d %s %s %s", i, a.Kind, a.Op, a.Value)
		}
	}
	h := sha256.Sum256([]byte(body))

	return hex.EncodeToString(h[:])
}

func (x *Expander) keepOld(d engine.Directive) bool {
	if v, ok := d.Opts["keep-old"]; ok {
		return v == "true"
	}

	return x.KeepOld
}

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
	pending := engine.Ident(newCol) + " IS DISTINCT FROM " + expr
	b := built{
		expand: []step{
			{sql: fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", col.rel(), engine.Ident(newCol), newType)},
			{sql: syncFunction(col, sync, newCol, expr)},
			{sql: syncTrigger(col, sync)},
			{sql: backfillSQL(col, spec, newCol+" = "+expr, pending), batch: spec},
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
	b.expand = append(b.expand, step{
		sql:    fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", col.rel(), pending),
		assert: &engine.AssertSpec{Op: "=", Kind: engine.AssertInt, Value: "0"},
	})
	if col.Default != "" {
		b.expand = slices.Insert(b.expand, 1, step{sql: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s",
			col.rel(), engine.Ident(newCol), col.Default)})
		b.notes = append(b.notes, fmt.Sprintf("carries the default %s over to %s.%s; a rename does not move it",
			col.Default, col.rel(), engine.Ident(newCol)))
	}
	x.retire(&b, d, col, newCol, oldCol)
	b.notes = append(b.notes,
		fmt.Sprintf("backfills %s.%s in batches of %d over %s",
			col.rel(), engine.Ident(newCol), spec.Size, spec.Key),
		fmt.Sprintf("counts what is left before the expand phase ends, and again when the contract phase is "+
			"confirmed: the swap happens only over a %s.%s every row of %s agrees with",
			col.rel(), engine.Ident(newCol), col.rel()))

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

func (x *Expander) backfill(ctx context.Context, conn engine.DB, d engine.Directive) (built, error) {
	tbl, err := resolveTable(ctx, conn, d, d.Args[0])
	if err != nil {
		return built{}, err
	}
	spec, err := x.cursor(ctx, conn, d, tbl)
	if err != nil {
		return built{}, err
	}
	set, err := parseAssignments(d)
	if err != nil {
		return built{}, err
	}
	if err := set.syncable(ctx, conn, d, tbl, spec); err != nil {
		return built{}, err
	}
	where, fresh := "true", "true"
	if w, ok := d.Opts["where"]; ok {
		val, err := checkedExpr(ctx, conn, d, "where="+w, w, tbl.Table)
		if err != nil {
			return built{}, err
		}
		where, fresh = w, qualifiedText(val)
	}
	sync := tbl.Table + backfillSyncSuffix
	if err := tbl.syncFree(ctx, conn, d, sync); err != nil {
		return built{}, err
	}
	set.qualify()
	pending := set.pending(where)

	return built{
		expand: []step{
			{sql: backfillSyncFunction(tbl, sync, set)},
			{sql: backfillSyncTrigger(tbl, sync, set.pendingNew(fresh))},
			{sql: backfillSQL(tbl, spec, set.set(), pending), batch: spec},
			{
				sql:    fmt.Sprintf("SELECT count(*) FROM %s WHERE %s", tbl.rel(), pending),
				assert: &engine.AssertSpec{Op: "=", Kind: engine.AssertInt, Value: "0"},
			},
			{sql: fmt.Sprintf("DROP TRIGGER %s ON %s", engine.Ident(sync), tbl.rel())},
			{sql: fmt.Sprintf("DROP FUNCTION %s.%s()", engine.Ident(tbl.Schema), engine.Ident(sync))},
		},
		notes: []string{
			fmt.Sprintf("backfills %s in batches of %d over %s", tbl.rel(), spec.Size, spec.Key),
			fmt.Sprintf("keeps rows written while it runs in sync through the trigger %s, and drops it after the "+
				"last batch; a run abandoned before that leaves it in place — `DROP TRIGGER %s ON %s; DROP FUNCTION %s.%s();`",
				engine.Ident(sync), engine.Ident(sync), tbl.rel(), engine.Ident(tbl.Schema), engine.Ident(sync)),
			fmt.Sprintf("counts what is left before it finishes: the run fails rather than reporting success while "+
				"a row of %s still matches the backfill", tbl.rel()),
		},
		downWhy: "a backfill has no generated inverse; write the .down.sql by hand",
	}, nil
}

type assignment struct {
	column string
	val    *pgquery.Node
	expr   string
	fresh  string
}

type assignments []assignment

func (as assignments) set() string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = engine.Ident(a.column) + " = " + a.expr
	}

	return strings.Join(out, ", ")
}

func (as assignments) into() string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = "new." + engine.Ident(a.column)
	}

	return strings.Join(out, ", ")
}

func (as assignments) exprs() string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.expr
	}

	return strings.Join(out, ", ")
}

func (as assignments) qualify() {
	for i := range as {
		as[i].fresh = qualifiedText(as[i].val)
	}
}

func (as assignments) pending(where string) string {
	return as.notYet(where, func(a assignment) (string, string) { return engine.Ident(a.column), a.expr })
}

func (as assignments) pendingNew(where string) string {
	return as.notYet(where, func(a assignment) (string, string) { return "new." + engine.Ident(a.column), a.fresh })
}

func (as assignments) notYet(where string, of func(assignment) (string, string)) string {
	cols := make([]string, len(as))
	exprs := make([]string, len(as))
	for i, a := range as {
		cols[i], exprs[i] = of(a)
	}

	return fmt.Sprintf("(%s) AND (ROW(%s) IS DISTINCT FROM ROW(%s))",
		where, strings.Join(cols, ", "), strings.Join(exprs, ", "))
}

func parseAssignments(d engine.Directive) (assignments, error) {
	set := d.Opts["set"]
	res, err := pgquery.Parse("UPDATE t SET " + set)
	if err != nil {
		return nil, refuse(d, "set=%s does not parse: %v", set, err)
	}
	var out assignments
	for _, node := range res.Stmts[0].Stmt.GetUpdateStmt().GetTargetList() {
		t := node.GetResTarget()
		if len(t.GetIndirection()) > 0 {
			return nil, refuse(d, "set= writes into an element or field of %s; the trigger assigns whole columns", t.GetName())
		}
		if t.GetVal().GetSetToDefault() != nil {
			return nil, refuse(d, "set= assigns DEFAULT to %s; write the default expression out so godwit can tell "+
				"a backfilled row from a stale one", t.GetName())
		}
		expr, err := exprText(t.GetVal())
		if err != nil {
			return nil, refuse(d, "set= assigns %s in a form godwit cannot render on its own (%v); a multi-column "+
				"assignment has to be written one column at a time", t.GetName(), err)
		}
		out = append(out, assignment{column: t.GetName(), val: t.GetVal(), expr: expr})
	}

	return out, nil
}

func (as assignments) syncable(ctx context.Context, conn engine.DB, d engine.Directive, t columnFacts, spec *engine.BatchSpec) error {
	written := make([]string, len(as))
	for i, a := range as {
		written[i] = a.column
	}
	for _, a := range as {
		if engine.Ident(a.column) == spec.Key {
			return refuse(d, "set= assigns the batch key %s; a row that moves under the cursor is skipped or repeated", spec.Key)
		}
		read, funcs, err := scanExpr(a.val, t.Table)
		if err != nil {
			return refuse(d, "set=%s %s", a.expr, err)
		}
		if other := slices.IndexFunc(read, func(c string) bool { return slices.Contains(written, c) }); other >= 0 {
			return refuse(d, "set= assigns %s from %s, which it also assigns; applying it twice would not mean the "+
				"same as applying it once, and the trigger and the batches would both apply it", a.column, read[other])
		}
		if err := checkVolatile(ctx, conn, d, "set="+a.expr, funcs); err != nil {
			return err
		}
	}

	return as.comparable(ctx, conn, d, t, written)
}

func (as assignments) comparable(ctx context.Context, conn engine.DB, d engine.Directive, t columnFacts, cols []string) error {
	rows, err := conn.Query(ctx, `
		SELECT w.name, coalesce(format_type(a.atttypid, a.atttypmod), ''),
		       EXISTS (SELECT 1 FROM pg_operator o
		               WHERE o.oprname = '=' AND o.oprleft = a.atttypid AND o.oprright = a.atttypid)
		FROM unnest($2::text[]) AS w(name)
		LEFT JOIN pg_attribute a ON a.attrelid = to_regclass($1) AND a.attname = w.name
		     AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY w.name`, t.rel(), cols)
	if err != nil {
		return fmt.Errorf("inspect %s set= columns: %w", t.ref(), err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[setTarget])
	if err != nil {
		return fmt.Errorf("read %s set= columns: %w", t.ref(), err)
	}
	for _, c := range found {
		if c.Type == "" {
			return refuse(d, "set= assigns %s.%s, which does not exist in the schema this migration starts from", t.ref(), c.Name)
		}
		if !c.Equality {
			return refuse(d, "set= assigns %s.%s of type %s, which has no equality operator; godwit could not tell "+
				"a backfilled row from a stale one", t.ref(), c.Name, c.Type)
		}
	}

	return nil
}

type setTarget struct {
	Name     string
	Type     string
	Equality bool
}

func (c columnFacts) syncFree(ctx context.Context, conn engine.DB, d engine.Directive, name string) error {
	var n int
	if err := conn.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM pg_trigger WHERE tgrelid = to_regclass($1) AND tgname = $3 AND NOT tgisinternal)
		     + (SELECT count(*) FROM pg_proc p JOIN pg_namespace s ON s.oid = p.pronamespace
		        WHERE s.nspname = $2 AND p.proname = $3)`, c.rel(), c.Schema, name).Scan(&n); err != nil {
		return fmt.Errorf("inspect %s sync trigger: %w", c.ref(), err)
	}
	if n > 0 {
		return refuse(d, "%s already carries a trigger or a function named %s; the expansion would collide with it",
			c.ref(), name)
	}

	return nil
}

func backfillSyncFunction(t columnFacts, sync string, set assignments) string {
	rel := engine.Ident(t.Table)
	body := fmt.Sprintf(" BEGIN SELECT %s INTO %s FROM (SELECT new.*) AS %s; RETURN new; END ",
		set.exprs(), set.into(), rel)
	tag := dollarTag(body)

	return fmt.Sprintf("CREATE FUNCTION %s.%s() RETURNS trigger LANGUAGE plpgsql AS %s%s%s",
		engine.Ident(t.Schema), engine.Ident(sync), tag, body, tag)
}

func backfillSyncTrigger(t columnFacts, sync, when string) string {
	return fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION %s.%s()",
		engine.Ident(sync), t.rel(), when, engine.Ident(t.Schema), engine.Ident(sync))
}

func qualifiedText(val *pgquery.Node) string {
	walkNodes(val.ProtoReflect(), func(m protoreflect.Message) bool {
		if ref, ok := m.Interface().(*pgquery.ColumnRef); ok {
			var name string
			for _, f := range ref.GetFields() {
				name = f.GetString_().GetSval()
			}
			ref.Fields = []*pgquery.Node{pgquery.MakeStrNode("new"), pgquery.MakeStrNode(name)}
		}

		return true
	})
	out, _ := exprText(val)

	return out
}

func exprText(val *pgquery.Node) (string, error) {
	sel := &pgquery.SelectStmt{TargetList: []*pgquery.Node{pgquery.MakeResTargetNodeWithVal(val, 0)}}
	out, err := pgquery.Deparse(&pgquery.ParseResult{Stmts: []*pgquery.RawStmt{
		{Stmt: &pgquery.Node{Node: &pgquery.Node_SelectStmt{SelectStmt: sel}}},
	}})
	if err != nil {
		return "", err
	}

	return strings.TrimPrefix(out, "SELECT "), nil
}

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

func assertion(d engine.Directive) (built, error) {
	query, spec, err := engine.ParseAssert(d)
	if err != nil {
		return built{}, refuse(d, "%s", err)
	}

	return built{expand: []step{{sql: strings.TrimRight(strings.TrimSpace(query), ";"), assert: &spec}}}, nil
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

func backfillSQL(t columnFacts, spec *engine.BatchSpec, set, where string) string {
	return fmt.Sprintf(
		"WITH b AS (SELECT %s AS %s FROM %s WHERE %s > %s AND (%s) ORDER BY %s LIMIT %d)\n"+
			"UPDATE %s AS t SET %s FROM b WHERE t.%s = b.%s RETURNING b.%s",
		spec.Key, batchKeyAlias, t.rel(), spec.Key, cursorParam(spec.KeyKind), where, spec.Key, spec.Size,
		t.rel(), set, spec.Key, batchKeyAlias, batchKeyAlias)
}

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
	body := fmt.Sprintf(" BEGIN SELECT %s INTO new.%s FROM (SELECT new.*) AS %s; RETURN new; END ",
		expr, engine.Ident(newCol), engine.Ident(col.Table))
	tag := dollarTag(body)

	return fmt.Sprintf("CREATE FUNCTION %s.%s() RETURNS trigger LANGUAGE plpgsql AS %s%s%s",
		engine.Ident(col.Schema), engine.Ident(sync), tag, body, tag)
}

func dollarTag(body string) string {
	tag := "$godwit$"
	for n := 0; strings.Contains(body, tag); n++ {
		tag = fmt.Sprintf("$godwit%d$", n)
	}

	return tag
}

func syncTrigger(col columnFacts, sync string) string {
	return fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION %s.%s()",
		engine.Ident(sync), col.rel(), engine.Ident(col.Schema), engine.Ident(sync))
}

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

// The column's own default, and its NOT NULL constraint on PostgreSQL 18, are excluded: neither is another object.
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

func checkUsing(ctx context.Context, conn engine.DB, d engine.Directive, using, table string) error {
	_, err := checkedExpr(ctx, conn, d, "using="+using, using, table)

	return err
}

func checkedExpr(ctx context.Context, conn engine.DB, d engine.Directive, label, expr, table string) (*pgquery.Node, error) {
	res, err := pgquery.Parse("SELECT " + expr)
	if err != nil {
		return nil, refuse(d, "%s does not parse: %v", label, err)
	}
	val := res.Stmts[0].Stmt.GetSelectStmt().GetTargetList()[0].GetResTarget().GetVal()
	_, funcs, err := scanExpr(val, table)
	if err != nil {
		return nil, refuse(d, "%s %s", label, err)
	}

	return val, checkVolatile(ctx, conn, d, label, funcs)
}

func checkVolatile(ctx context.Context, conn engine.DB, d engine.Directive, label string, funcs []string) error {
	if len(funcs) == 0 {
		return nil
	}
	var volatile string
	err := conn.QueryRow(ctx, `
		SELECT p.proname FROM pg_proc p WHERE p.provolatile = 'v' AND p.proname = ANY($1) LIMIT 1`, funcs).Scan(&volatile)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect volatility of %s: %w", label, err)
	}

	return refuse(d, "%s calls the VOLATILE function %s(); the trigger and the backfill would disagree", label, volatile)
}

func scanExpr(root *pgquery.Node, table string) ([]string, []string, error) {
	r := exprRefs{table: table}
	walkNodes(root.ProtoReflect(), r.visit)
	if r.err != nil {
		return nil, nil, r.err
	}

	return r.cols, r.funcs, nil
}

func walkNodes(m protoreflect.Message, visit func(protoreflect.Message) bool) bool {
	if !visit(m) {
		return false
	}
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if fd.Kind() != protoreflect.MessageKind || !m.Has(fd) {
			continue
		}
		v := m.Get(fd)
		if !fd.IsList() {
			if !walkNodes(v.Message(), visit) {
				return false
			}

			continue
		}
		for j := range v.List().Len() {
			if !walkNodes(v.List().Get(j).Message(), visit) {
				return false
			}
		}
	}

	return true
}

type exprRefs struct {
	table string
	cols  []string
	funcs []string
	err   error
}

func (r *exprRefs) visit(m protoreflect.Message) bool {
	switch m.Descriptor().Name() {
	case "SubLink":
		r.err = errors.New("contains a subquery; the trigger form cannot express it")

		return false
	case "ColumnRef":
		return r.column(m.Interface().(*pgquery.ColumnRef))
	case "FuncCall":
		var name string
		for _, n := range m.Interface().(*pgquery.FuncCall).GetFuncname() {
			name = n.GetString_().GetSval()
		}
		r.funcs = append(r.funcs, name)
	}

	return true
}

func (r *exprRefs) column(ref *pgquery.ColumnRef) bool {
	var name string
	for i, f := range ref.GetFields() {
		name = f.GetString_().GetSval()
		if i == 0 && len(ref.GetFields()) > 1 && name != r.table {
			r.err = fmt.Errorf("references %s; only columns of %s are in scope inside the trigger", name, r.table)

			return false
		}
	}
	r.cols = append(r.cols, name)

	return true
}

// ExpandUp replaces each directive migration's body with the expansion frozen on the plan or the run.
func ExpandUp(plans []engine.Plan, exps map[string]Expansion) ([]engine.Plan, error) {
	return substitute(plans, exps, func(e Expansion) string { return e.UpSQL })
}

// ExpandDown substitutes each migration's own frozen inverse from the ledger; a hand-written .down.sql has no expansion and wins untouched.
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

// Unretired lists every column the expansions of one run remove, so a retired one stops being reported as a rollback the target holds.
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
