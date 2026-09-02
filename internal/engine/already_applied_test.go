package engine

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestOpacity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sql  string
		want string
	}{
		{"CREATE TABLE t (id int PRIMARY KEY, v text DEFAULT '');", ""},
		{"CREATE TABLE IF NOT EXISTS t (id serial);", ""},
		{"CREATE INDEX i ON t (v);", ""},
		{"CREATE UNIQUE INDEX CONCURRENTLY i ON t (v) WHERE v IS NOT NULL;", ""},
		{"CREATE VIEW v AS SELECT 1;", ""},
		{"CREATE OR REPLACE VIEW v AS SELECT 1;", ""},
		{"DROP TABLE t;", ""},
		{"DROP INDEX CONCURRENTLY i;", ""},
		{"DROP VIEW v;", ""},
		{"ALTER TABLE t RENAME TO u;", ""},
		{"ALTER TABLE t RENAME COLUMN a TO b;", ""},
		{"ALTER TABLE t RENAME CONSTRAINT a TO b;", ""},
		{"ALTER INDEX i RENAME TO j;", ""},
		{"ALTER VIEW v RENAME TO w;", ""},
		{"ALTER TABLE t ADD COLUMN a int NOT NULL DEFAULT 0;", ""},
		{"ALTER TABLE t DROP COLUMN a;", ""},
		{"ALTER TABLE t ALTER COLUMN a TYPE bigint;", ""},
		{"ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0) NOT VALID;", ""},
		{"ALTER TABLE t VALIDATE CONSTRAINT c;", ""},
		{"ALTER TABLE t DROP CONSTRAINT c;", ""},
		{"ALTER TABLE t ALTER COLUMN a SET NOT NULL;", ""},
		{"ALTER TABLE t ALTER COLUMN a DROP NOT NULL;", ""},
		{"ALTER TABLE t ALTER COLUMN a SET DEFAULT 1;", ""},

		{"INSERT INTO t VALUES (1);", OpaqueDML},
		{"UPDATE t SET a = 1;", OpaqueDML},
		{"DELETE FROM t;", OpaqueDML},
		{"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN DELETE;", OpaqueDML},
		{"SELECT 1;", OpaqueDML},
		{"COPY t FROM STDIN;", OpaqueDML},
		{"TRUNCATE t;", OpaqueDML},
		{"CALL p();", OpaqueDML},
		{"DO $$ BEGIN END $$;", OpaqueDML},

		{"CREATE TEMP TABLE t (id int);", OpaqueUnknown},
		{"CREATE UNLOGGED TABLE t (id int);", OpaqueUnknown},
		{"CREATE TABLE t (id int) WITH (fillfactor = 70);", OpaqueUnknown},
		{"CREATE TABLE t (id int) TABLESPACE ts;", OpaqueUnknown},
		{"CREATE TABLE t (id int) USING heap;", OpaqueUnknown},
		{"CREATE TABLE t (id int) PARTITION BY RANGE (id);", OpaqueUnknown},
		{"CREATE TABLE t1 PARTITION OF t FOR VALUES FROM (1) TO (2);", OpaqueUnknown},
		{"CREATE TABLE t (id int) INHERITS (p);", OpaqueUnknown},
		{"CREATE TABLE t OF ty;", OpaqueUnknown},
		{"CREATE TABLE t (LIKE u);", OpaqueUnknown},
		{"CREATE TABLE t (id int GENERATED ALWAYS AS IDENTITY);", OpaqueUnknown},
		{"CREATE TABLE t (id int, d int GENERATED ALWAYS AS (id * 2) STORED);", OpaqueUnknown},
		{"CREATE TABLE t (v text COLLATE \"C\");", OpaqueUnknown},
		{"CREATE INDEX i ON t (v) TABLESPACE ts;", OpaqueUnknown},
		{"CREATE VIEW v WITH (security_barrier) AS SELECT 1;", OpaqueUnknown},
		{"CREATE VIEW v AS SELECT 1 WITH CHECK OPTION;", OpaqueUnknown},
		{"CREATE MATERIALIZED VIEW mv AS SELECT 1;", OpaqueUnknown},
		{"DROP FUNCTION f;", OpaqueUnknown},
		{"ALTER FUNCTION f RENAME TO g;", OpaqueUnknown},
		{"ALTER VIEW v RENAME COLUMN a TO b;", OpaqueUnknown},
		{"ALTER TABLE t ADD COLUMN a int GENERATED ALWAYS AS IDENTITY;", OpaqueUnknown},
		{"ALTER TABLE t ALTER COLUMN a TYPE text COLLATE \"C\";", OpaqueUnknown},
		{"ALTER TABLE t SET LOGGED;", OpaqueUnknown},
		{"ALTER TABLE t ENABLE ROW LEVEL SECURITY;", OpaqueUnknown},
		{"ALTER INDEX i SET (fillfactor = 70);", OpaqueUnknown},
		{"CREATE FUNCTION f() RETURNS int AS 'SELECT 1' LANGUAGE sql;", OpaqueUnknown},
		{"CREATE TYPE mood AS ENUM ('a');", OpaqueUnknown},
		{"CREATE SEQUENCE s;", OpaqueUnknown},
		{"CREATE EXTENSION pgcrypto;", OpaqueUnknown},
		{"GRANT SELECT ON t TO r;", OpaqueUnknown},
		{"COMMENT ON TABLE t IS 'x';", OpaqueUnknown},
		{"VACUUM t;", OpaqueUnknown},
	}
	for _, tc := range cases {
		p := buildPlanT(t, Migration{Version: 1, Name: "m", UpSQL: tc.sql, DownSQL: "SELECT 1;"}, DirectionUp)
		if got := p.Statements[0].Opaque; got != tc.want {
			t.Errorf("%s: opaque = %q, want %q", tc.sql, got, tc.want)
		}
	}
}

func TestPlanOpaqueFirstReason(t *testing.T) {
	t.Parallel()
	p := buildPlanT(t, Migration{
		Version: 1, Name: "m", UpSQL: "CREATE TABLE t (id int); CREATE FUNCTION f() RETURNS int AS 'SELECT 1' LANGUAGE sql; INSERT INTO t VALUES (1);",
		DownSQL: "SELECT 1;",
	}, DirectionUp)
	if got := p.Opaque(); got != OpaqueUnknown {
		t.Fatalf("opaque = %q", got)
	}
	if got := buildPlanT(t, Migration{Version: 1, Name: "m", UpSQL: "CREATE TABLE t (id int);", DownSQL: "SELECT 1;"}, DirectionUp).Opaque(); got != "" {
		t.Fatalf("opaque = %q", got)
	}
}

func TestMarkOnlyRecordsWithoutExecuting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := newTestDB(t)
	setup := connect()
	if _, err := setup.Exec(ctx, "CREATE TABLE done (id int)"); err != nil {
		t.Fatal(err)
	}

	p := buildPlanT(t, Migration{Version: 1, Name: "done", Checksum: "c", UpSQL: "CREATE TABLE done (id int);", DownSQL: "DROP TABLE done;"}, DirectionUp)
	p.MarkOnly = true
	res, err := New(connect(), Options{}).Up(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || res.Applied != 0 {
		t.Fatalf("result = %+v", res)
	}
	var count, stmts int
	if err := setup.QueryRow(ctx, `SELECT count(*) FROM godwit.migrations WHERE version = 1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migrations rows = %d, err = %v", count, err)
	}
	if err := setup.QueryRow(ctx, `SELECT stmt_count FROM godwit.runs WHERE version = 1 AND state = 'succeeded'`).Scan(&stmts); err != nil || stmts != 0 {
		t.Fatalf("stmt_count = %d, err = %v", stmts, err)
	}

	res, err = New(connect(), Options{}).Up(ctx, p)
	if err != nil || !res.Skipped {
		t.Fatalf("second up: %+v, %v", res, err)
	}

	if _, err := setup.Exec(ctx, "CREATE TABLE dup (v int); INSERT INTO dup VALUES (1), (1)"); err != nil {
		t.Fatal(err)
	}
	broken := buildPlanT(t, Migration{Version: 2, Name: "uidx", Checksum: "c", UpSQL: "CREATE UNIQUE INDEX CONCURRENTLY uidx_dup ON dup (v);", DownSQL: "SELECT 1;"}, DirectionUp)
	if _, err := New(connect(), Options{}).Up(ctx, broken); err == nil {
		t.Fatal("unique index over duplicates must fail")
	}
	broken.MarkOnly = true
	wantErr(t, must2(New(connect(), Options{}).Up(ctx, broken)), "uidx_dup exists but is INVALID")
	if err := setup.QueryRow(ctx, `SELECT count(*) FROM godwit.migrations WHERE version = 2`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("migrations rows = %d, err = %v", count, err)
	}
}

func must2(_ Result, err error) error { return err }

func TestMarkErrorPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	down := buildPlanT(t, Migration{Version: 1, Name: "m", Checksum: "c", UpSQL: "SELECT 1;", DownSQL: "SELECT 2;"}, DirectionDown)
	down.MarkOnly = true
	mock, exec := newMockExec(t)
	expectLock(mock)
	expectBootstrap(mock)
	mock.ExpectQuery("SELECT checksum FROM godwit.migrations").WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"checksum"}).AddRow("c"))
	wantErr(t, must2(exec.Down(ctx, down)), "mark requires an up plan")

	up := txPlan(t)
	up.MarkOnly = true
	mock, exec = newMockExec(t)
	expectLock(mock)
	expectBootstrap(mock)
	expectNotApplied(mock)
	mock.ExpectQuery("FROM pg_index").WillReturnError(errBoom)
	wantErr(t, must2(exec.Up(ctx, up)), "inspect invalid indexes")

	mock, exec = newMockExec(t)
	expectLock(mock)
	expectBootstrap(mock)
	expectNotApplied(mock)
	mock.ExpectQuery("FROM pg_index").WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("x").RowError(0, errBoom))
	wantErr(t, must2(exec.Up(ctx, up)), "read invalid indexes")

	mock, exec = newMockExec(t)
	expectLock(mock)
	expectBootstrap(mock)
	expectNotApplied(mock)
	mock.ExpectQuery("FROM pg_index").WillReturnRows(pgxmock.NewRows([]string{"name"}))
	mock.ExpectExec("INSERT INTO godwit.runs").WithArgs(pgxmock.AnyArg(), int64(1)).WillReturnError(errBoom)
	wantErr(t, must2(exec.Up(ctx, up)), "insert run")

	mock, exec = newMockExec(t)
	expectLock(mock)
	expectBootstrap(mock)
	expectNotApplied(mock)
	mock.ExpectQuery("FROM pg_index").WillReturnRows(pgxmock.NewRows([]string{"name"}))
	mock.ExpectExec("INSERT INTO godwit.runs").WithArgs(pgxmock.AnyArg(), int64(1)).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO godwit.migrations").WithArgs(int64(1), "m", "c").WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE godwit.runs SET state = 'succeeded'").WithArgs(pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	res, err := exec.Up(ctx, up)
	if err != nil || res.Skipped || res.Applied != 0 {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidIndexesNoneOnFreshDatabase(t *testing.T) {
	t.Parallel()
	got, err := InvalidIndexes(context.Background(), newTestDB(t)())
	if err != nil || got != nil {
		t.Fatalf("got %v, err %v", got, err)
	}
}
