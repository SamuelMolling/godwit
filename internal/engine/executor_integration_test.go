package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func buildPlanT(t *testing.T, m Migration, dir Direction) Plan {
	t.Helper()
	p, err := BuildPlan(m, dir)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func countRows(t *testing.T, conn *pgx.Conn, table string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

// crashOn closes the session at one hook point, simulating a dead executor.
func crashOn(conn *pgx.Conn, point HookPoint, idx int) Option {
	fired := false

	return WithHook(func(p HookPoint, i int) {
		if !fired && p == point && i == idx {
			fired = true
			_ = conn.Close(context.Background())
		}
	})
}

func TestUpDownHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := newTestDB(t)
	conn := connect()

	m := Migration{
		Version: 1, Name: "users", Checksum: "c1",
		UpSQL:   "CREATE TABLE users (id int);\nINSERT INTO users VALUES (1);",
		DownSQL: "DROP TABLE users;",
	}

	exec := New(conn, Options{})
	res, err := exec.Up(ctx, buildPlanT(t, m, DirectionUp))
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || res.Applied != 2 {
		t.Fatalf("res = %+v", res)
	}

	res, err = exec.Up(ctx, buildPlanT(t, m, DirectionUp))
	if err != nil || !res.Skipped {
		t.Fatalf("re-up: res = %+v, err = %v", res, err)
	}

	changed := m
	changed.Checksum = "other"
	if _, err := exec.Up(ctx, buildPlanT(t, changed, DirectionUp)); err == nil ||
		!strings.Contains(err.Error(), "different content") {
		t.Fatalf("checksum drift: err = %v", err)
	}

	rows, err := exec.Status(ctx, []Migration{m, {Version: 2, Name: "pending"}})
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0].Applied || rows[0].Drifted || rows[1].Applied {
		t.Fatalf("status = %+v", rows)
	}

	res, err = exec.Down(ctx, buildPlanT(t, m, DirectionDown))
	if err != nil || res.Skipped || res.Applied != 1 {
		t.Fatalf("down: res = %+v, err = %v", res, err)
	}
	res, err = exec.Down(ctx, buildPlanT(t, m, DirectionDown))
	if err != nil || !res.Skipped {
		t.Fatalf("re-down: res = %+v, err = %v", res, err)
	}
}

func TestDirectionMismatch(t *testing.T) {
	t.Parallel()

	exec := New(nil, Options{})
	if _, err := exec.Up(context.Background(), Plan{Direction: DirectionDown}); err == nil {
		t.Fatal("want error")
	}
	if _, err := exec.Down(context.Background(), Plan{Direction: DirectionUp}); err == nil {
		t.Fatal("want error")
	}
}

func TestStatementFailureMarksRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := newTestDB(t)
	conn := connect()

	m := Migration{
		Version: 1, Name: "bad", Checksum: "c",
		UpSQL: "CREATE TABLE ok (id int);\nSELECT 1/0;", DownSQL: "DROP TABLE ok;",
	}
	var events []StatementEvent
	exec := New(conn, Options{}, WithObserver(func(ev StatementEvent) { events = append(events, ev) }))
	if _, err := exec.Up(ctx, buildPlanT(t, m, DirectionUp)); err == nil ||
		!strings.Contains(err.Error(), "statement 1") {
		t.Fatalf("err = %v", err)
	}
	if len(events) != 2 || events[0].Err != nil || events[0].Version != 1 || events[1].Index != 1 || events[1].Err == nil {
		t.Fatalf("events = %+v", events)
	}

	var state, errText string
	if err := conn.QueryRow(ctx,
		`SELECT state, error FROM godwit.runs WHERE version = 1`).Scan(&state, &errText); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || errText == "" {
		t.Fatalf("run state = %s, error = %q", state, errText)
	}

	tampered := m
	tampered.UpSQL = "CREATE TABLE tampered (id int);\nSELECT 1/0;"
	if _, err := exec.Up(ctx, buildPlanT(t, tampered, DirectionUp)); err == nil ||
		!strings.Contains(err.Error(), "changed since run") {
		t.Fatalf("tamper guard: err = %v", err)
	}

	// Fixing the failed statement resumes from it without re-running statement 0.
	fixed := m
	fixed.UpSQL = "CREATE TABLE ok (id int);\nSELECT 1;"
	res, err := exec.Up(ctx, buildPlanT(t, fixed, DirectionUp))
	if err != nil || res.Applied != 1 {
		t.Fatalf("fixed resume: res = %+v, err = %v", res, err)
	}
}

func TestCrashResumeTx(t *testing.T) {
	t.Parallel()

	points := []struct {
		point HookPoint
		idx   int
	}{
		{HookBeforeStatement, 0},
		{HookInsideTx, 0},
		{HookBeforeStatement, 1},
		{HookInsideTx, 1},
		{HookInsideTx, 2},
	}
	m := Migration{
		Version: 1, Name: "steps", Checksum: "c",
		UpSQL:   "CREATE TABLE t1 (id int);\nCREATE TABLE t2 (id int);\nINSERT INTO t1 VALUES (1);",
		DownSQL: "DROP TABLE t1; DROP TABLE t2;",
	}

	for _, tc := range points {
		t.Run(string(tc.point)+"/"+string(rune('0'+tc.idx)), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			connect := newTestDB(t)

			crashed := connect()
			exec := New(crashed, Options{}, crashOn(crashed, tc.point, tc.idx))
			if _, err := exec.Up(ctx, buildPlanT(t, m, DirectionUp)); err == nil {
				t.Fatal("crash run must fail")
			}

			fresh := connect()
			res, err := New(fresh, Options{}).Up(ctx, buildPlanT(t, m, DirectionUp))
			if err != nil {
				t.Fatalf("resume: %v", err)
			}
			if res.Skipped {
				t.Fatalf("resume skipped: %+v", res)
			}
			if n := countRows(t, fresh, "t1"); n != 1 {
				t.Fatalf("t1 rows = %d, want exactly 1 (journal lied)", n)
			}
			if n := countRows(t, fresh, "t2"); n != 0 {
				t.Fatalf("t2 rows = %d", n)
			}
		})
	}
}

func TestCrashResumeConcurrentIndex(t *testing.T) {
	t.Parallel()

	m := Migration{
		Version: 1, Name: "idx", Checksum: "c",
		UpSQL:   "CREATE INDEX CONCURRENTLY idx_ct ON ct (v);",
		DownSQL: "DROP INDEX idx_ct;",
	}

	for _, point := range []HookPoint{HookAfterIntent, HookAfterExec} {
		t.Run(string(point), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			connect := newTestDB(t)

			setup := connect()
			if _, err := setup.Exec(ctx, "CREATE TABLE ct (v int)"); err != nil {
				t.Fatal(err)
			}

			crashed := connect()
			exec := New(crashed, Options{}, crashOn(crashed, point, 0))
			if _, err := exec.Up(ctx, buildPlanT(t, m, DirectionUp)); err == nil {
				t.Fatal("crash run must fail")
			}

			fresh := connect()
			if _, err := New(fresh, Options{}).Up(ctx, buildPlanT(t, m, DirectionUp)); err != nil {
				t.Fatalf("resume: %v", err)
			}
			var valid bool
			if err := fresh.QueryRow(ctx,
				`SELECT indisvalid FROM pg_index WHERE indexrelid = 'idx_ct'::regclass`).Scan(&valid); err != nil {
				t.Fatal(err)
			}
			if !valid {
				t.Fatal("index not valid after resume")
			}
		})
	}
}

func TestInvalidIndexRepair(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := newTestDB(t)

	setup := connect()
	if _, err := setup.Exec(ctx, "CREATE TABLE dup (v int); INSERT INTO dup VALUES (1), (1)"); err != nil {
		t.Fatal(err)
	}

	m := Migration{
		Version: 1, Name: "uidx", Checksum: "c",
		UpSQL:   "CREATE UNIQUE INDEX CONCURRENTLY uidx_dup ON dup (v);",
		DownSQL: "DROP INDEX uidx_dup;",
	}

	exec := New(connect(), Options{})
	if _, err := exec.Up(ctx, buildPlanT(t, m, DirectionUp)); err == nil {
		t.Fatal("unique index over duplicates must fail")
	}

	var valid bool
	if err := setup.QueryRow(ctx,
		`SELECT indisvalid FROM pg_index WHERE indexrelid = 'uidx_dup'::regclass`).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if valid {
		t.Fatal("expected an INVALID index left behind")
	}

	if _, err := setup.Exec(ctx, "DELETE FROM dup WHERE ctid IN (SELECT ctid FROM dup LIMIT 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(connect(), Options{}).Up(ctx, buildPlanT(t, m, DirectionUp)); err != nil {
		t.Fatalf("repair run: %v", err)
	}
	if err := setup.QueryRow(ctx,
		`SELECT indisvalid FROM pg_index WHERE indexrelid = 'uidx_dup'::regclass`).Scan(&valid); err != nil {
		t.Fatal(err)
	}
	if !valid {
		t.Fatal("index still invalid after repair")
	}
}

func TestCrashResumeDropIndexConcurrently(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := newTestDB(t)

	setup := connect()
	if _, err := setup.Exec(ctx, "CREATE TABLE dt (v int); CREATE INDEX idx_dt ON dt (v)"); err != nil {
		t.Fatal(err)
	}

	m := Migration{
		Version: 1, Name: "dropidx", Checksum: "c",
		UpSQL:   "DROP INDEX CONCURRENTLY idx_dt;",
		DownSQL: "CREATE INDEX idx_dt ON dt (v);",
	}

	crashed := connect()
	exec := New(crashed, Options{}, crashOn(crashed, HookAfterExec, 0))
	if _, err := exec.Up(ctx, buildPlanT(t, m, DirectionUp)); err == nil {
		t.Fatal("crash run must fail")
	}

	if _, err := New(connect(), Options{}).Up(ctx, buildPlanT(t, m, DirectionUp)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var exists bool
	if err := setup.QueryRow(ctx, `SELECT to_regclass('idx_dt') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("index still exists after resume")
	}
}

func TestLockBlocksSecondExecutor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connect := newTestDB(t)

	holder := connect()
	release, err := acquireLock(ctx, holder)
	if err != nil {
		t.Fatal(err)
	}

	m := Migration{
		Version: 1, Name: "locked", Checksum: "c",
		UpSQL: "SELECT 1;", DownSQL: "SELECT 1;",
	}
	blockedCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := New(connect(), Options{}).Up(blockedCtx, buildPlanT(t, m, DirectionUp)); err == nil {
		t.Fatal("second executor must block until timeout")
	}

	release()
	if _, err := New(connect(), Options{}).Up(ctx, buildPlanT(t, m, DirectionUp)); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestReconcileRerunKind(t *testing.T) {
	t.Parallel()

	done, err := reconcile(context.Background(), nil, Statement{Verifier: VerifierRerun})
	if err != nil || done {
		t.Fatalf("rerun reconcile = %v, %v", done, err)
	}
}
