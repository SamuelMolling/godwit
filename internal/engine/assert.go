package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// DirectiveAssert states a data condition the run has to meet; it is the only directive that reads.
const DirectiveAssert = "assert"

// Value kinds an assertion compares against.
const (
	AssertInt  = "int"
	AssertBool = "bool"
)

// ErrAssertFailed marks a run stopped by an assertion that did not hold.
var ErrAssertFailed = errors.New("assertion failed")

var assertOps = []string{"=", "<>", "!=", "<", "<=", ">", ">="}

var assertEqualOps = []string{"=", "<>", "!="}

// AssertSpec is the condition an `-- godwit: assert` states about the single value its SELECT returns.
type AssertSpec struct {
	Op    string `json:"op"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// String renders the condition as it was written.
func (a AssertSpec) String() string {
	return a.Op + " " + a.Value
}

// ParseAssert returns the query an assert directive checks and the condition it states.
func ParseAssert(d Directive) (string, AssertSpec, error) {
	if len(d.Args) != 3 {
		return "", AssertSpec{}, fmt.Errorf("assert takes 3 argument(s), got %d", len(d.Args))
	}
	spec := AssertSpec{Op: d.Args[1], Kind: AssertInt, Value: d.Args[2]}
	if _, err := strconv.ParseInt(spec.Value, 10, 64); err != nil {
		spec.Kind = AssertBool
	}
	if spec.Kind == AssertBool && !slices.Contains(assertEqualOps, spec.Op) {
		return "", AssertSpec{}, fmt.Errorf("assert compares a boolean with %s; use %s", spec.Op, "= or <>")
	}

	return d.Args[0], spec, nil
}

func checkAssertOp(v string) error {
	if !slices.Contains(assertOps, v) {
		return fmt.Errorf("%q is not one of %s", v, "=, <>, !=, <, <=, >, >=")
	}

	return nil
}

func checkAssertValue(v string) error {
	if _, err := strconv.ParseInt(v, 10, 64); err == nil {
		return nil
	}
	if v == "true" || v == "false" {
		return nil
	}

	return fmt.Errorf("%q is not an integer or true/false", v)
}

// checkAssertQuery refuses offline anything but a single read-only SELECT of one column.
func checkAssertQuery(v string) error {
	res, err := pgquery.Parse(v)
	if err != nil {
		return fmt.Errorf("not a query: %w", err)
	}
	if len(res.Stmts) != 1 {
		return fmt.Errorf("must be one statement, got %d", len(res.Stmts))
	}
	sel := res.Stmts[0].Stmt.GetSelectStmt()
	if sel == nil {
		return errors.New("must be a SELECT; an assertion reads and never writes")
	}

	return checkAssertSelect(sel)
}

func checkAssertSelect(sel *pgquery.SelectStmt) error {
	if sel.IntoClause != nil {
		return errors.New("SELECT INTO writes a table; an assertion reads and never writes")
	}
	if len(sel.LockingClause) > 0 {
		return errors.New("a locking clause takes row locks; an assertion reads and never writes")
	}
	if err := checkAssertCTEs(sel.WithClause); err != nil {
		return err
	}
	if sel.Larg != nil {
		if err := checkAssertSelect(sel.Larg); err != nil {
			return err
		}

		return checkAssertSelect(sel.Rarg)
	}

	return checkAssertTargets(sel.TargetList)
}

func checkAssertCTEs(with *pgquery.WithClause) error {
	if with == nil {
		return nil
	}
	for _, node := range with.Ctes {
		cte := node.GetCommonTableExpr()
		sel := cte.GetCtequery().GetSelectStmt()
		if sel == nil {
			return fmt.Errorf("the %s CTE writes; an assertion reads and never writes", cte.GetCtename())
		}
		if err := checkAssertSelect(sel); err != nil {
			return err
		}
	}

	return nil
}

func checkAssertTargets(list []*pgquery.Node) error {
	if len(list) != 1 {
		return fmt.Errorf("must select one column, got %d", len(list))
	}
	for _, f := range list[0].GetResTarget().GetVal().GetColumnRef().GetFields() {
		if f.GetAStar() != nil {
			return errors.New("must select one column, not *")
		}
	}

	return nil
}

var assertOIDs = map[string][]uint32{
	AssertInt:  {pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID},
	AssertBool: {pgtype.BoolOID},
}

// assertResult is what the assertion's query returned: how many rows, and the first value of the first row.
type assertResult struct {
	rows int
	num  *int64
	flag *bool
}

func (r assertResult) String() string {
	switch {
	case r.num != nil:
		return strconv.FormatInt(*r.num, 10)
	case r.flag != nil:
		return strconv.FormatBool(*r.flag)
	default:
		return "NULL"
	}
}

func (a AssertSpec) holds(r assertResult) (bool, error) {
	if a.Kind == AssertBool {
		want, err := strconv.ParseBool(a.Value)
		if err != nil {
			return false, fmt.Errorf("assertion wants %q, which is not a boolean", a.Value)
		}

		return (*r.flag == want) == (a.Op == "="), nil
	}
	want, err := strconv.ParseInt(a.Value, 10, 64)
	if err != nil {
		return false, fmt.Errorf("assertion wants %q, which is not an integer", a.Value)
	}

	return compareInt(a.Op, *r.num, want), nil
}

func compareInt(op string, got, want int64) bool {
	switch op {
	case "=":
		return got == want
	case "<>", "!=":
		return got != want
	case "<":
		return got < want
	case "<=":
		return got <= want
	case ">":
		return got > want
	case ">=":
		return got >= want
	default:
		return false
	}
}

// execAssert evaluates one assertion inside a read-only transaction, so a volatile function the offline
// check could not see still cannot write, and journals it done once it holds.
func (e *Executor) execAssert(ctx context.Context, prog runProgress, idx int, st Statement) error {
	res, err := e.readAssert(ctx, st)
	if err != nil {
		return err
	}
	if err := e.checkAssert(st, res); err != nil {
		return err
	}

	return recordJournal(ctx, e.db, prog.runID, idx, "done", st.Hash)
}

// checkAssert compares what the query returned; the probe used by the scratch replay stops before it,
// because the scratch holds the target's schema and not its rows.
func (e *Executor) checkAssert(st Statement, res assertResult) error {
	if e.assertProbe {
		return nil
	}
	query := assertQuery(st.SQL)
	if res.rows != 1 {
		return fmt.Errorf("%w: %s returned %d rows, want exactly one", ErrAssertFailed, query, res.rows)
	}
	if res.num == nil && res.flag == nil {
		return fmt.Errorf("%w: %s returned NULL, want %s", ErrAssertFailed, query, st.Assert)
	}
	ok, err := st.Assert.holds(res)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s returned %s, want %s", ErrAssertFailed, query, res, st.Assert)
	}

	return nil
}

// assertQuery drops the `-- godwit expanded:` header the splicer leaves above a body's first statement,
// so a failure names the query rather than the comment.
func assertQuery(sql string) string {
	for {
		line, rest, ok := strings.Cut(sql, "\n")
		if !ok || !strings.HasPrefix(strings.TrimSpace(line), "--") {
			return strings.TrimSpace(sql)
		}
		sql = rest
	}
}

func (e *Executor) readAssert(ctx context.Context, st Statement) (assertResult, error) {
	var res assertResult
	tx, err := e.db.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin assert: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, set := range append(e.timeoutSQL("SET LOCAL"), "SET LOCAL transaction_read_only = on") {
		if _, err := tx.Exec(ctx, set); err != nil {
			return res, fmt.Errorf("set timeouts: %w", err)
		}
	}
	rows, err := tx.Query(ctx, st.SQL)
	if err != nil {
		return res, fmt.Errorf("exec: %w", err)
	}
	defer rows.Close()

	if err := checkAssertColumn(rows, st.Assert.Kind); err != nil {
		return res, err
	}

	return scanAssert(rows, st.Assert.Kind)
}

func checkAssertColumn(rows pgx.Rows, kind string) error {
	fields := rows.FieldDescriptions()
	// A query that failed describes no fields; draining it is what reports the real error.
	if len(fields) == 0 {
		return nil
	}
	if len(fields) != 1 {
		return fmt.Errorf("an assertion selects one column, got %d", len(fields))
	}
	if !slices.Contains(assertOIDs[kind], fields[0].DataTypeOID) {
		return fmt.Errorf("an assertion compared against %s needs a %s column, got type %d",
			kind, kind, fields[0].DataTypeOID)
	}

	return nil
}

func scanAssert(rows pgx.Rows, kind string) (assertResult, error) {
	var res assertResult
	var num *int64
	var flag *bool
	target := []any{&num}
	if kind == AssertBool {
		target = []any{&flag}
	}
	if _, err := pgx.ForEachRow(rows, target, func() error {
		if res.rows == 0 {
			res.num, res.flag = num, flag
		}
		res.rows++

		return nil
	}); err != nil {
		return assertResult{}, fmt.Errorf("read assert: %w", err)
	}

	return res, nil
}
