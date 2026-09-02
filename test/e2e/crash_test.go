//go:build e2e

package e2e

import (
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

const (
	v1 = 20260901120000
	v2 = 20260901120001
	v3 = 20260901120002
)

var usersTable = migration{v1, "users", "CREATE TABLE users (id bigint PRIMARY KEY, email text);", "DROP TABLE users;"}

func TestKillMidTransactionalStatement(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("app")
	r.mustMigrate(migrationDir(t, usersTable))

	id := r.createRun(migration{
		v2, "plan",
		"ALTER TABLE users ADD COLUMN plan text; SELECT pg_sleep(6);",
		"ALTER TABLE users DROP COLUMN plan;",
	})
	victim := r.claimer(id)
	r.waitActive("SELECT pg_sleep")
	r.kill(victim)

	expectContains(t, r.mustCLI("run", "watch", id), "succeeded (attempt 2)")
	r.expectRun(id, godwitv1.RunState_RUN_STATE_SUCCEEDED, 2)
	if n := query[int](t, r.appDSN,
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'plan'`); n != 1 {
		t.Fatalf("plan column count = %d, want 1", n)
	}
	expectJournal(t, r.appDSN, 2)
}

func TestKillBetweenStatements(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("app")
	r.mustMigrate(migrationDir(t, migration{
		v1, "audit",
		"CREATE TABLE audit (id bigserial PRIMARY KEY, note text);", "DROP TABLE audit;",
	}))

	id := r.createRun(migration{
		v2, "seed",
		"INSERT INTO audit (note) VALUES ('once'); SELECT pg_sleep(6);",
		"DELETE FROM audit;",
	})
	victim := r.claimer(id)
	r.waitActive("SELECT pg_sleep")
	if n := query[int](t, r.appDSN,
		`SELECT count(*) FROM godwit.journal j JOIN godwit.runs r ON r.id = j.run_id WHERE r.version = $1 AND j.stmt_idx = 0 AND j.state = 'done'`, v2); n != 1 {
		t.Fatalf("statement 0 done rows before the kill = %d, want 1", n)
	}
	r.kill(victim)

	expectContains(t, r.mustCLI("run", "watch", id), "succeeded (attempt 2)")
	r.expectRun(id, godwitv1.RunState_RUN_STATE_SUCCEEDED, 2)
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM audit`); n != 1 {
		t.Fatalf("audit rows = %d, want 1 (statement 0 re-executed)", n)
	}
	expectJournal(t, r.appDSN, 2)
}

func TestKillDuringConcurrentIndex(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("app")
	r.mustMigrate(migrationDir(t, migration{v1, "big", "CREATE TABLE big (id bigint, v text);", "DROP TABLE big;"}))
	execSQL(t, r.appDSN, `INSERT INTO big SELECT g, md5(g::text) FROM generate_series(1, 2000000) g`)

	id := r.createRun(migration{
		v2, "big_v_idx",
		"CREATE INDEX CONCURRENTLY big_v_idx ON big (v);",
		"DROP INDEX CONCURRENTLY big_v_idx;",
	})
	victim := r.claimer(id)
	r.waitActive("CREATE INDEX CONCURRENTLY")
	r.kill(victim)

	expectContains(t, r.mustCLI("run", "watch", id), "succeeded (attempt 2)")
	r.expectRun(id, godwitv1.RunState_RUN_STATE_SUCCEEDED, 2)
	if !query[bool](t, r.appDSN, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'big_v_idx'::regclass`) {
		t.Fatal("big_v_idx is not valid")
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM pg_indexes WHERE tablename = 'big'`); n != 1 {
		t.Fatalf("indexes on big = %d, want 1", n)
	}
	expectJournal(t, r.appDSN, 1)
}

func TestReplicaRestartRecoversLease(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	r.addTarget("app")
	r.mustMigrate(migrationDir(t, usersTable))

	id := r.createRun(migration{
		v2, "plan",
		"ALTER TABLE users ADD COLUMN plan text; SELECT pg_sleep(6);",
		"ALTER TABLE users DROP COLUMN plan;",
	})
	victim := r.claimer(id)
	r.waitActive("SELECT pg_sleep")
	r.kill(victim)
	r.start()

	expectContains(t, r.mustCLI("run", "watch", id), "succeeded (attempt 2)")
	r.expectRun(id, godwitv1.RunState_RUN_STATE_SUCCEEDED, 2)
	expectJournal(t, r.appDSN, 2)
}

func expectJournal(t *testing.T, dsn string, statements int) {
	t.Helper()
	if n := query[int](t, dsn, `
		SELECT count(*) FROM (
			SELECT j.stmt_idx FROM godwit.journal j JOIN godwit.runs r ON r.id = j.run_id
			WHERE r.version = $1 AND j.state = 'done' GROUP BY j.stmt_idx HAVING count(*) = 1) s`, v2); n != statements {
		t.Fatalf("statements with exactly one done row = %d, want %d", n, statements)
	}
	if n := query[int](t, dsn, `SELECT count(*) FROM godwit.runs WHERE version = $1`, v2); n != 1 {
		t.Fatalf("godwit.runs rows for version %d = %d, want 1", v2, n)
	}
	if state := query[string](t, dsn, `SELECT state FROM godwit.runs WHERE version = $1`, v2); state != "succeeded" {
		t.Fatalf("godwit.runs state = %s, want succeeded", state)
	}
}
