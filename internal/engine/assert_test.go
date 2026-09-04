package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"
)

func assertDirective(t *testing.T, line string) Directive {
	t.Helper()
	ds, err := ParseDirectives(line + "\n")
	if err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	if len(ds) != 1 {
		t.Fatalf("%s: got %d directives", line, len(ds))
	}

	return ds[0]
}

func TestAssertGrammarAccepts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line  string
		query string
		spec  AssertSpec
	}{
		{
			line:  "-- godwit: assert 'SELECT count(*) FROM orders WHERE total IS NULL' = 0",
			query: "SELECT count(*) FROM orders WHERE total IS NULL",
			spec:  AssertSpec{Op: "=", Kind: AssertInt, Value: "0"},
		},
		{
			line:  "-- godwit: assert 'SELECT count(*) FROM users' > 0",
			query: "SELECT count(*) FROM users",
			spec:  AssertSpec{Op: ">", Kind: AssertInt, Value: "0"},
		},
		{
			line:  "-- godwit: assert 'SELECT bool_and(email ~ ''@'') FROM users' = true",
			query: "SELECT bool_and(email ~ '@') FROM users",
			spec:  AssertSpec{Op: "=", Kind: AssertBool, Value: "true"},
		},
		{
			line:  "-- godwit: assert 'SELECT (SELECT count(*) FROM a) - (SELECT count(*) FROM b)' <> -1",
			query: "SELECT (SELECT count(*) FROM a) - (SELECT count(*) FROM b)",
			spec:  AssertSpec{Op: "<>", Kind: AssertInt, Value: "-1"},
		},
		{
			line:  "-- godwit: assert 'WITH d AS (SELECT 1 AS n) SELECT count(*) FROM d' >= 1",
			query: "WITH d AS (SELECT 1 AS n) SELECT count(*) FROM d",
			spec:  AssertSpec{Op: ">=", Kind: AssertInt, Value: "1"},
		},
		{
			line:  "-- godwit: assert 'SELECT count(*) FROM a UNION SELECT count(*) FROM b' <= 9",
			query: "SELECT count(*) FROM a UNION SELECT count(*) FROM b",
			spec:  AssertSpec{Op: "<=", Kind: AssertInt, Value: "9"},
		},
	}
	for _, tc := range cases {
		d := assertDirective(t, tc.line)
		query, spec, err := ParseAssert(d)
		if err != nil {
			t.Fatalf("%s: %v", tc.line, err)
		}
		if query != tc.query || spec != tc.spec {
			t.Fatalf("%s: got %q %+v", tc.line, query, spec)
		}
	}
}

func TestAssertGrammarRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct{ line, want string }{
		{"-- godwit: assert 'UPDATE t SET a = 1' = 0", "must be a SELECT"},
		{"-- godwit: assert 'SELECT 1; SELECT 2' = 0", "must be one statement"},
		{"-- godwit: assert 'SELECT count(*) INTO x FROM t' = 0", "SELECT INTO writes"},
		{"-- godwit: assert 'SELECT id FROM t FOR UPDATE' = 0", "locking clause"},
		{"-- godwit: assert 'WITH d AS (DELETE FROM t RETURNING id) SELECT count(*) FROM d' = 0", "the d CTE writes"},
		{"-- godwit: assert 'WITH d AS (SELECT a, b FROM t) SELECT count(*) FROM d' = 0", "must select one column, got 2"},
		{"-- godwit: assert 'SELECT a, b FROM t' = 0", "must select one column, got 2"},
		{"-- godwit: assert 'SELECT * FROM t' = 0", "not *"},
		{"-- godwit: assert 'SELECT t.* FROM t' = 0", "not *"},
		{"-- godwit: assert 'SELECT count(*) FROM a UNION SELECT x, y FROM b' = 0", "must select one column, got 2"},
		{"-- godwit: assert 'SELECT x, y FROM a UNION SELECT count(*) FROM b' = 0", "must select one column, got 2"},
		{"-- godwit: assert 'WITH d AS (SELECT a, b FROM t) SELECT count(*) FROM d UNION SELECT 1' = 0", "must select one column, got 2"},
		{"-- godwit: assert 'SELECT count(*) FROM a UNION SELECT * FROM b' = 0", "not *"},
		{"-- godwit: assert 'not sql at all' = 0", "not a query"},
		{"-- godwit: assert 'SELECT count(*) FROM t' == 0", "is not one of"},
		{"-- godwit: assert 'SELECT count(*) FROM t' = zero", "is not an integer or true/false"},
		{"-- godwit: assert 'SELECT bool_and(x) FROM t' > true", "compares a boolean"},
		{"-- godwit: assert 'SELECT count(*) FROM t' = 0 extra=1", "has no option"},
		{"-- godwit: assert 'SELECT count(*) FROM t' = 0 = 1", "unexpected argument"},
		{"-- godwit: assert 'SELECT count(*) FROM t' =", "takes 3 argument(s), got 2"},
	}
	for _, tc := range cases {
		_, err := ParseDirectives(tc.line + "\n")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want containing %q", tc.line, err, tc.want)
		}
		var derr *DirectiveError
		if !errors.As(err, &derr) || derr.Line != 1 {
			t.Fatalf("%s: err = %v, want a DirectiveError on line 1", tc.line, err)
		}
	}
}

func TestParseAssertArity(t *testing.T) {
	t.Parallel()
	if _, _, err := ParseAssert(Directive{Op: DirectiveAssert, Args: []string{"SELECT 1"}}); err == nil {
		t.Fatal("want an arity error")
	}
}

func TestAssertSpecHolds(t *testing.T) {
	t.Parallel()
	n := func(v int64) assertResult { return assertResult{rows: 1, num: &v} }
	b := func(v bool) assertResult { return assertResult{rows: 1, flag: &v} }
	cases := []struct {
		spec AssertSpec
		got  assertResult
		want bool
	}{
		{AssertSpec{Op: "=", Kind: AssertInt, Value: "0"}, n(0), true},
		{AssertSpec{Op: "=", Kind: AssertInt, Value: "0"}, n(3), false},
		{AssertSpec{Op: "<>", Kind: AssertInt, Value: "0"}, n(3), true},
		{AssertSpec{Op: "!=", Kind: AssertInt, Value: "3"}, n(3), false},
		{AssertSpec{Op: "<", Kind: AssertInt, Value: "3"}, n(2), true},
		{AssertSpec{Op: "<=", Kind: AssertInt, Value: "3"}, n(3), true},
		{AssertSpec{Op: ">", Kind: AssertInt, Value: "3"}, n(4), true},
		{AssertSpec{Op: ">=", Kind: AssertInt, Value: "4"}, n(3), false},
		{AssertSpec{Op: "~", Kind: AssertInt, Value: "3"}, n(3), false},
		{AssertSpec{Op: "=", Kind: AssertBool, Value: "true"}, b(true), true},
		{AssertSpec{Op: "=", Kind: AssertBool, Value: "false"}, b(true), false},
		{AssertSpec{Op: "<>", Kind: AssertBool, Value: "true"}, b(false), true},
	}
	for _, tc := range cases {
		got, err := tc.spec.holds(tc.got)
		if err != nil || got != tc.want {
			t.Fatalf("%+v over %s: %v, %v", tc.spec, tc.got, got, err)
		}
	}
	for _, spec := range []AssertSpec{
		{Op: "=", Kind: AssertInt, Value: "many"},
		{Op: "=", Kind: AssertBool, Value: "maybe"},
	} {
		if _, err := spec.holds(n(0)); err == nil {
			t.Fatalf("%+v: want an error", spec)
		}
	}
}

func TestAssertQueryDropsTheHeader(t *testing.T) {
	t.Parallel()
	got := assertQuery("-- godwit expanded: assert 'SELECT 1' = 1\n-- another\nSELECT 1\n")
	if got != "SELECT 1" {
		t.Fatalf("got %q", got)
	}
	if assertQuery("SELECT 1") != "SELECT 1" {
		t.Fatal("a body with no header is its own name")
	}
}

func TestAssertResultString(t *testing.T) {
	t.Parallel()
	n, b := int64(7), true
	cases := []struct {
		res  assertResult
		want string
	}{
		{assertResult{num: &n}, "7"},
		{assertResult{flag: &b}, "true"},
		{assertResult{}, "NULL"},
	}
	for _, tc := range cases {
		if got := tc.res.String(); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}

func assertPlan(t *testing.T, sql string, spec AssertSpec, rest ...string) Plan {
	t.Helper()
	sts := []Statement{{SQL: sql, Hash: hashSQL(sql), Assert: &spec}}
	for _, s := range rest {
		sts = append(sts, Statement{SQL: s, Hash: hashSQL(s)})
	}

	return Plan{
		Migration:  Migration{Version: 1, Name: "assert", Checksum: "c"},
		Direction:  DirectionUp,
		Statements: sts,
	}
}

const assertSetup = `CREATE TABLE orders (id bigint PRIMARY KEY, total numeric);
INSERT INTO orders (id, total) VALUES (1, 10), (2, NULL), (3, 30)`

func setupAssert(t *testing.T) func() *pgx.Conn {
	t.Helper()
	connect := newTestDB(t)
	if _, err := connect().Exec(context.Background(), assertSetup); err != nil {
		t.Fatal(err)
	}

	return connect
}

func TestAssertHoldsAndTheRunContinues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := setupAssert(t)
	conn := connect()
	p := assertPlan(t, "SELECT count(*) FROM orders WHERE total IS NULL",
		AssertSpec{Op: "=", Kind: AssertInt, Value: "1"},
		"CREATE TABLE marker (id int)")
	res, err := New(conn, Options{}).Up(ctx, p)
	if err != nil || res.Applied != 2 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if n := scalarInt(t, conn, "SELECT count(*) FROM godwit.migrations"); n != 1 {
		t.Fatalf("migrations = %d", n)
	}
	if n := scalarInt(t, conn, "SELECT count(*) FROM godwit.journal WHERE state = 'done'"); n != 2 {
		t.Fatalf("journal rows = %d", n)
	}
	if n := scalarInt(t, conn, "SELECT count(*) FROM marker"); n != 0 {
		t.Fatalf("marker rows = %d", n)
	}
}

func TestAssertFailsAndStopsTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := setupAssert(t)
	conn := connect()
	p := assertPlan(t, "SELECT count(*) FROM orders WHERE total IS NULL",
		AssertSpec{Op: "=", Kind: AssertInt, Value: "0"},
		"CREATE TABLE marker (id int)")
	_, err := New(conn, Options{}).Up(ctx, p)
	if !errors.Is(err, ErrAssertFailed) {
		t.Fatalf("err = %v, want ErrAssertFailed", err)
	}
	wantErr(t, err, "returned 1, want = 0")
	if n := scalarInt(t, conn, "SELECT count(*) FROM godwit.migrations"); n != 0 {
		t.Fatalf("migrations = %d, want the run to record nothing", n)
	}
	if n := scalarInt(t, conn, "SELECT count(*) FROM godwit.runs WHERE state = 'failed'"); n != 1 {
		t.Fatalf("failed runs = %d", n)
	}
	if n := scalarInt(t, conn,
		"SELECT count(*) FROM pg_class WHERE relname = 'marker'"); n != 0 {
		t.Fatalf("the statement after the assertion ran")
	}
}

func TestAssertShapesTheRunRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct{ name, sql, want string }{
		{"no rows", "SELECT count(*) FROM orders WHERE false GROUP BY id", "returned 0 rows"},
		{"many rows", "SELECT id FROM orders", "returned 3 rows"},
		{"null", "SELECT max(id) FROM orders WHERE false", "returned NULL"},
		{"two columns", "SELECT 1, 2", "selects one column, got 2"},
		{"wrong type", "SELECT 'x'::text", "needs a int column"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := setupAssert(t)()
			p := assertPlan(t, tc.sql, AssertSpec{Op: "=", Kind: AssertInt, Value: "0"})
			_, err := New(conn, Options{}).Up(ctx, p)
			wantErr(t, err, tc.want)
		})
	}
}

func TestAssertUnparseableWantedValue(t *testing.T) {
	t.Parallel()
	conn := setupAssert(t)()
	p := assertPlan(t, "SELECT count(*) FROM orders", AssertSpec{Op: "=", Kind: AssertInt, Value: "many"})
	_, err := New(conn, Options{}).Up(context.Background(), p)
	wantErr(t, err, "not an integer")
}

func TestAssertBooleanCondition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := setupAssert(t)()
	p := assertPlan(t, "SELECT bool_and(id > 0) FROM orders", AssertSpec{Op: "=", Kind: AssertBool, Value: "true"})
	if _, err := New(conn, Options{}).Up(ctx, p); err != nil {
		t.Fatal(err)
	}
	p2 := assertPlan(t, "SELECT bool_and(total IS NOT NULL) FROM orders", AssertSpec{Op: "=", Kind: AssertBool, Value: "true"})
	p2.Migration.Version = 2
	_, err := New(conn, Options{}).Up(ctx, p2)
	wantErr(t, err, "returned false, want = true")
}

// A read-only transaction is the guard the offline check cannot be: volatility lives in the catalog.
func TestAssertRefusesAWritingFunctionAtRuntime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := setupAssert(t)
	conn := connect()
	if _, err := conn.Exec(ctx,
		`CREATE FUNCTION sneak() RETURNS bigint LANGUAGE sql AS $$ INSERT INTO orders VALUES (99, 1); SELECT 0::bigint $$`); err != nil {
		t.Fatal(err)
	}
	p := assertPlan(t, "SELECT sneak()", AssertSpec{Op: "=", Kind: AssertInt, Value: "0"})
	_, err := New(conn, Options{}).Up(ctx, p)
	wantErr(t, err, "read-only transaction")
	if n := scalarInt(t, conn, "SELECT count(*) FROM orders"); n != 3 {
		t.Fatalf("orders = %d, want the write refused", n)
	}
}

// The probe runs the query for its shape and stops before the comparison: the scratch has the schema, not the rows.
func TestAssertProbeRunsWithoutEnforcing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := setupAssert(t)()
	p := assertPlan(t, "SELECT count(*) FROM orders WHERE total IS NULL",
		AssertSpec{Op: "=", Kind: AssertInt, Value: "0"})
	if _, err := New(conn, Options{}, WithAssertProbe()).Up(ctx, p); err != nil {
		t.Fatalf("probe = %v", err)
	}
	p2 := assertPlan(t, "SELECT count(*) FROM missing_table", AssertSpec{Op: "=", Kind: AssertInt, Value: "0"})
	p2.Migration.Version = 2
	_, err := New(conn, Options{}, WithAssertProbe()).Up(ctx, p2)
	wantErr(t, err, "missing_table")
}

// A resume walks past the assertion again: a condition that held before the crash is not one that holds now.
func TestAssertIsRecheckedOnResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := setupAssert(t)
	conn := connect()
	sql := "SELECT count(*) FROM orders WHERE total IS NULL"
	p := assertPlan(t, sql, AssertSpec{Op: "=", Kind: AssertInt, Value: "1"}, "CREATE TABLE marker (id int)")
	p.HoldFrom = 1
	res, err := New(conn, Options{}).Up(ctx, p)
	if err != nil || !res.Held {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO orders (id, total) VALUES (4, NULL)"); err != nil {
		t.Fatal(err)
	}
	p.HoldFrom = 0
	_, err = New(conn, Options{}).Up(ctx, p)
	if !errors.Is(err, ErrAssertFailed) {
		t.Fatalf("err = %v, want the resumed run to re-check the assertion", err)
	}
	wantErr(t, err, "returned 2, want = 1")
}

func TestAssertResumesWhenItStillHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := setupAssert(t)()
	sql := "SELECT count(*) FROM orders WHERE total IS NULL"
	p := assertPlan(t, sql, AssertSpec{Op: "=", Kind: AssertInt, Value: "1"}, "CREATE TABLE marker (id int)")
	p.HoldFrom = 1
	if _, err := New(conn, Options{}).Up(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.HoldFrom = 0
	if _, err := New(conn, Options{}).Up(ctx, p); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, conn, "SELECT count(*) FROM godwit.migrations"); n != 1 {
		t.Fatalf("migrations = %d", n)
	}
	if n := scalarInt(t, conn, "SELECT count(*) FROM godwit.runs"); n != 1 {
		t.Fatalf("runs = %d, want the contract phase to resume the same run", n)
	}
}

func assertMockPlan() Plan {
	sql := "SELECT count(*) FROM t"

	return Plan{
		Migration: Migration{Version: 1, Name: "m", Checksum: "c"},
		Direction: DirectionUp,
		Statements: []Statement{{
			SQL: sql, Hash: hashSQL(sql),
			Assert: &AssertSpec{Op: "=", Kind: AssertInt, Value: "0"},
		}},
	}
}

func expectAssertPreamble(mock pgxmock.PgxConnIface) {
	expectLock(mock)
	expectBootstrap(mock)
	expectNotApplied(mock)
	expectNewRun(mock)
}

func TestAssertErrorPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	zero := int64(0)
	int8Rows := func(vals ...any) *pgxmock.Rows {
		rows := pgxmock.NewRowsWithColumnDefinition(pgconn.FieldDescription{Name: "count", DataTypeOID: pgtype.Int8OID})
		for _, v := range vals {
			rows.AddRow(v)
		}

		return rows
	}
	cases := []struct {
		name string
		set  func(pgxmock.PgxConnIface)
		want string
	}{
		{
			name: "begin",
			set:  func(m pgxmock.PgxConnIface) { m.ExpectBegin().WillReturnError(errBoom) },
			want: "begin assert",
		},
		{
			name: "timeouts",
			set: func(m pgxmock.PgxConnIface) {
				m.ExpectBegin()
				m.ExpectExec("SET LOCAL lock_timeout").WillReturnError(errBoom)
				m.ExpectRollback()
			},
			want: "set timeouts",
		},
		{
			name: "query",
			set: func(m pgxmock.PgxConnIface) {
				expectAssertTx(m)
				m.ExpectQuery("SELECT count").WillReturnError(errBoom)
				m.ExpectRollback()
			},
			want: "exec: boom",
		},
		{
			name: "scan",
			set: func(m pgxmock.PgxConnIface) {
				expectAssertTx(m)
				m.ExpectQuery("SELECT count").WillReturnRows(int8Rows("not a number"))
				m.ExpectRollback()
			},
			want: "read assert",
		},
		{
			name: "journal",
			set: func(m pgxmock.PgxConnIface) {
				expectAssertTx(m)
				m.ExpectQuery("SELECT count").WillReturnRows(int8Rows(&zero))
				m.ExpectRollback()
				m.ExpectExec("INSERT INTO godwit.journal").
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
					WillReturnError(errBoom)
			},
			want: "journal done",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock, exec := newMockExec(t)
			expectAssertPreamble(mock)
			tc.set(mock)
			expectMarkFailed(mock)
			_, err := exec.Up(ctx, assertMockPlan())
			wantErr(t, err, tc.want)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func expectAssertTx(m pgxmock.PgxConnIface) {
	m.ExpectBegin()
	m.ExpectExec("SET LOCAL lock_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
	m.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
	m.ExpectExec("SET LOCAL transaction_read_only").WillReturnResult(pgxmock.NewResult("SET", 0))
}
