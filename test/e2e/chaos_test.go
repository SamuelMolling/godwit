//go:build chaos

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func (r *rig) createRunFull(req *godwitv1.CreateRunRequest) string {
	r.t.Helper()
	req.Target = r.target
	resp, err := r.client().CreateRun(context.Background(), connect.NewRequest(req))
	if err != nil {
		r.t.Fatal(err)
	}

	return resp.Msg.RunId
}

func journalRows(t *testing.T, dsn string, version int64, state string) int {
	t.Helper()

	return query[int](t, dsn, `
		SELECT count(*) FROM godwit.journal j JOIN godwit.runs r ON r.id = j.run_id
		WHERE r.version = $1 AND j.state = $2`, version, state)
}

func waitJournal(t *testing.T, dsn string, version int64, idx int, state string) {
	t.Helper()
	waitUntil(t, settle, fmt.Sprintf("journal %s for statement %d", state, idx), func() bool {
		return query[int](t, dsn, `
			SELECT count(*) FROM godwit.journal j JOIN godwit.runs r ON r.id = j.run_id
			WHERE r.version = $1 AND j.stmt_idx = $2 AND j.state = $3`, version, idx, state) == 1
	})
}

// reclaimer attributes a second claim of a run every replica has already claimed once.
func reclaimer(t *testing.T, r *rig, id string, fn func()) *replica {
	t.Helper()
	before := map[*replica]int{}
	for _, rep := range r.all() {
		before[rep] = rep.logs.countField("run claimed", "run", id)
	}
	fn()

	return await(t, settle, "run "+id+" claimed again", func() (*replica, bool) {
		for _, rep := range r.all() {
			if !rep.dead && rep.logs.countField("run claimed", "run", id) > before[rep] {
				return rep, true
			}
		}

		return nil, false
	})
}

// reap kills a replica and the backends it left: one waiting on a lock never notices a closed client socket.
func (r *rig) reap(rep *replica, like string) {
	r.t.Helper()
	r.kill(rep)
	waitUntil(r.t, settle, "orphan backend gone", func() bool {
		terminate(r.t, r.appDB, like)

		return query[int](r.t, adminDSN,
			`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND query LIKE $2`, r.appDB, like) == 0
	})
}

func TestChaosKillMidBatch(t *testing.T) {
	t.Parallel()
	const rows, batch = 400_000, 2_000
	r := newRig(t, 2)
	r.addTarget("batchy")
	r.mustMigrate(migrationDir(t, migration{v1, "bf", bfTable, "DROP TABLE bf;"}))
	seedRows(t, r.appDSN, "bf", rows)

	id := r.createRun(backfillMigration(v2, batch, " pause=20ms"))
	victim := r.claimer(id)
	waitUntil(t, settle, "the backfill is past its tenth batch", func() bool {
		return probe[int64](r.appDSN, `SELECT coalesce(max(rows_done), 0) FROM godwit.journal`) > int64(10*batch)
	})
	before := query[int64](t, r.appDSN, `SELECT cursor::bigint FROM godwit.journal WHERE cursor IS NOT NULL`)
	doneBefore := query[int64](t, r.appDSN, `SELECT rows_done FROM godwit.journal WHERE cursor IS NOT NULL`)
	r.kill(victim)

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if n := query[int64](t, r.appDSN, `SELECT count(*) FROM bf WHERE w IS DISTINCT FROM v * 2`); n != 0 {
		t.Fatalf("%d rows left unbackfilled after the resume", n)
	}
	after := query[int64](t, r.appDSN, `SELECT cursor::bigint FROM godwit.journal WHERE cursor IS NOT NULL`)
	if after < before {
		t.Fatalf("cursor went backwards over the crash: %d then %d", before, after)
	}
	done := query[int64](t, r.appDSN, `SELECT rows_done FROM godwit.journal WHERE cursor IS NOT NULL`)
	if done != rows {
		t.Fatalf("rows_done = %d, want %d: the crash must cost the in-flight batch and nothing else", done, rows)
	}
	if n := journalRows(t, r.appDSN, v2, "done"); n != 1 {
		t.Fatalf("done journal rows for the backfill = %d, want 1", n)
	}
	report(t, "chaos/kill_mid_batch",
		"rows", rows, "batch", batch, "cursor_at_kill", before, "rows_done_at_kill", doneBefore,
		"final_cursor", after, "final_rows_done", done,
		"attempts", run.Attempts, "converge_seconds", elapsed.Seconds())
}

func TestChaosKillBetweenIntentAndStatement(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("intent")
	r.mustMigrate(migrationDir(t, migration{v1, "big", "CREATE TABLE big (id bigserial PRIMARY KEY, v int);", "DROP TABLE big;"}))
	seedRows(t, r.appDSN, "big", 200_000)

	// SHARE blocks the CREATE INDEX CONCURRENTLY without blocking the schema snapshot admission takes.
	release := holdLock(t, r.appDSN, "LOCK TABLE big IN SHARE MODE")
	id := r.createRunFull(&godwitv1.CreateRunRequest{
		Files: files(migration{
			v2, "idx",
			"CREATE INDEX CONCURRENTLY big_v_idx ON big (v);", "DROP INDEX CONCURRENTLY big_v_idx;",
		}),
		LockTimeout: "120s",
	})
	victim := r.claimer(id)
	waitJournal(t, r.appDSN, v2, 0, "intent")
	r.waitActive("CREATE INDEX CONCURRENTLY")
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM pg_indexes WHERE tablename = 'big'`); n != 1 {
		t.Fatalf("indexes on big before the kill = %d, want only the primary key", n)
	}
	r.reap(victim, "CREATE INDEX CONCURRENTLY%")
	release()

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if !query[bool](t, r.appDSN, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'big_v_idx'::regclass`) {
		t.Fatal("big_v_idx is not valid")
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM pg_indexes WHERE tablename = 'big'`); n != 2 {
		t.Fatalf("indexes on big = %d, want the primary key and big_v_idx", n)
	}
	if n := journalRows(t, r.appDSN, v2, "done"); n != 1 {
		t.Fatalf("done journal rows = %d, want 1", n)
	}
	report(t, "chaos/kill_between_intent_and_statement",
		"attempts", run.Attempts, "converge_seconds", elapsed.Seconds())
}

func TestChaosKillBetweenStatementAndJournal(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("gap")
	r.mustMigrate(migrationDir(t, migration{v1, "big", "CREATE TABLE big (id bigserial PRIMARY KEY, v int);", "DROP TABLE big;"}))
	seedRows(t, r.appDSN, "big", 300_000)

	id := r.createRunFull(&godwitv1.CreateRunRequest{
		Files: files(migration{
			v2, "idx",
			"CREATE INDEX CONCURRENTLY big_v_idx ON big (v);", "DROP INDEX CONCURRENTLY big_v_idx;",
		}),
		LockTimeout: "120s",
	})
	victim := r.claimer(id)
	waitJournal(t, r.appDSN, v2, 0, "intent")
	release := holdLock(t, r.appDSN, "LOCK TABLE godwit.journal IN ACCESS EXCLUSIVE MODE")
	waitUntil(t, settle, "the index is built and valid", func() bool {
		return probe[bool](r.appDSN, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'big_v_idx'::regclass`)
	})
	oid := query[int64](t, r.appDSN, `SELECT 'big_v_idx'::regclass::oid::bigint`)
	r.reap(victim, "INSERT INTO godwit.journal%")
	release()

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if again := query[int64](t, r.appDSN, `SELECT 'big_v_idx'::regclass::oid::bigint`); again != oid {
		t.Fatalf("index oid %d became %d: the survivor rebuilt an index it already had", oid, again)
	}
	if n := journalRows(t, r.appDSN, v2, "done"); n != 1 {
		t.Fatalf("done journal rows = %d, want 1", n)
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations WHERE version = $1`, v2); n != 1 {
		t.Fatalf("godwit.migrations rows = %d, want 1", n)
	}
	report(t, "chaos/kill_between_statement_and_journal",
		"attempts", run.Attempts, "converge_seconds", elapsed.Seconds())
}

func TestChaosKillDuringFinalize(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("final")
	r.mustMigrate(migrationDir(t, migration{v1, "mark", "CREATE TABLE mark (id int);", "DROP TABLE mark;"}))

	id := r.createRun(migration{
		v2, "two",
		"INSERT INTO mark VALUES (1); SELECT pg_sleep(6);", "DELETE FROM mark;",
	})
	victim := r.claimer(id)
	waitJournal(t, r.appDSN, v2, 0, "done")
	release := holdLock(t, r.appDSN, "LOCK TABLE godwit.migrations IN ACCESS EXCLUSIVE MODE")
	waitJournal(t, r.appDSN, v2, 1, "done")
	waitUntil(t, settle, "finalize blocked on godwit.migrations", func() bool {
		return probe[int](r.appDSN, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE 'INSERT INTO godwit.migrations%'`) == 1
	})
	r.reap(victim, "INSERT INTO godwit.migrations%")
	release()

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM mark`); n != 1 {
		t.Fatalf("mark rows = %d, want 1: a resume past the last done statement must not re-run it", n)
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations WHERE version = $1`, v2); n != 1 {
		t.Fatalf("godwit.migrations rows = %d, want 1", n)
	}
	report(t, "chaos/kill_during_finalize", "attempts", run.Attempts, "converge_seconds", elapsed.Seconds())
}

func TestChaosKillDuringContractPhase(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("rollout")
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text, plan text);", "DROP TABLE users;",
	}))

	dir := migrationDir(t,
		migration{v2, "add_plan_v2", "ALTER TABLE users ADD COLUMN plan_v2 text;", "ALTER TABLE users DROP COLUMN plan_v2;"},
		migration{v3, "drop_plan", "ALTER TABLE users DROP COLUMN plan; SELECT pg_sleep(6);", "ALTER TABLE users ADD COLUMN plan text;"})
	expectContains(t, r.mustMigrate(dir, "--rollout", "expand-contract", "--ack", "H003"), "awaiting_contract")
	held := r.latestRun()

	victim := reclaimer(t, r, held.Id, func() { r.mustCLI("run", "confirm", held.Id) })
	r.waitActive("SELECT pg_sleep")
	r.kill(victim)

	run, elapsed := watchRun(t, r, held.Id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", held.Id, run.State, run.Error)
	}
	if columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("the contract phase must drop plan")
	}
	if !columnExists(t, r.appDSN, "users", "plan_v2") {
		t.Fatal("the resume must not undo the expand phase")
	}
	for _, v := range []int64{v2, v3} {
		if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations WHERE version = $1`, v); n != 1 {
			t.Fatalf("godwit.migrations rows for %d = %d, want 1", v, n)
		}
	}
	report(t, "chaos/kill_during_contract", "attempts", run.Attempts, "converge_seconds", elapsed.Seconds())
}

func TestChaosKillDuringRevert(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("undo")
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text);", "DROP TABLE users;",
	}))
	r.mustMigrate(migrationDir(t, migration{
		v2, "plan",
		"ALTER TABLE users ADD COLUMN plan text;",
		"ALTER TABLE users DROP COLUMN plan; SELECT pg_sleep(6);",
	}))
	applied := r.latestRun()

	resp, err := r.client().RevertRun(context.Background(), connect.NewRequest(&godwitv1.RevertRunRequest{
		RunId: applied.Id, AcknowledgeHazards: []string{"H003"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	id := resp.Msg.RunId
	victim := r.claimer(id)
	r.waitActive("SELECT pg_sleep")
	r.kill(victim)

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("revert %s: state %s, error %s", id, run.State, run.Error)
	}
	if columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("the revert must drop plan")
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations WHERE version = $1`, v2); n != 0 {
		t.Fatalf("version %d still recorded after the revert", v2)
	}
	if r.getRun(applied.Id).State != godwitv1.RunState_RUN_STATE_REVERTED {
		t.Fatalf("original run state = %s, want reverted", r.getRun(applied.Id).State)
	}
	report(t, "chaos/kill_during_revert", "attempts", run.Attempts, "converge_seconds", elapsed.Seconds())
}

func TestChaosStoreOutageMidRun(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	cut, cutDSN := newProxy(t, r.storeDSN)
	victim := r.startWith("--store-dsn", cutDSN)
	r.addTarget("islanded")
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text);", "DROP TABLE users;",
	}))

	id := r.createRun(migration{
		v2, "plan",
		"ALTER TABLE users ADD COLUMN plan text; SELECT pg_sleep(20);", "ALTER TABLE users DROP COLUMN plan;",
	})
	if r.claimer(id) != victim {
		t.Fatal("the islanded replica must be the one that claimed")
	}
	spare := r.startWith()
	r.useReplica(spare)
	r.waitActive("SELECT pg_sleep")
	cut.set(proxyCut)

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if !columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("column plan missing after the store outage")
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations WHERE version = $1`, v2); n != 1 {
		t.Fatalf("godwit.migrations rows = %d, want 1", n)
	}
	if n := journalRows(t, r.appDSN, v2, "done"); n != 2 {
		t.Fatalf("done journal rows = %d, want 2", n)
	}
	report(t, "chaos/store_outage",
		"attempts", run.Attempts, "converge_seconds", elapsed.Seconds(),
		"heartbeat_losses_on_the_islanded_replica", victim.logs.count("heartbeat lost"),
		"claims_by_the_spare", spare.logs.count("run claimed"))
}

func TestChaosTargetCutMidStatement(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	// Retrying past the fault is the point; the attempt budget must not end the run first.
	r.startWith("--max-attempts", "10")
	cut, cutDSN := newProxy(t, r.appDSN)
	r.addTargetDSN("severed", cutDSN)
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text);", "DROP TABLE users;",
	}))

	id := r.createRun(migration{
		v2, "plan",
		"ALTER TABLE users ADD COLUMN plan text; SELECT pg_sleep(10);", "ALTER TABLE users DROP COLUMN plan;",
	})
	r.claimer(id)
	r.waitActive("SELECT pg_sleep")
	cut.set(proxyCut)
	waitUntil(t, settle, "the severed statement is reported", func() bool {
		return r.getRun(id).Error != ""
	})
	severed := r.getRun(id)
	report(t, "chaos/target_cut_first_failure", "state", severed.State, "retries", severed.Retries, "error", severed.Error)
	cut.set(proxyPass)

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if !columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("column plan missing after the reconnect")
	}
	if n := journalRows(t, r.appDSN, v2, "done"); n != 2 {
		t.Fatalf("done journal rows = %d, want 2", n)
	}
	expectContains(t, severed.Error, "transient:")
	report(t, "chaos/target_cut", "retries", run.Retries, "converge_seconds", elapsed.Seconds())
}

func TestChaosTargetHangsMidStatement(t *testing.T) {
	t.Parallel()
	const timeout = 8 * time.Second
	r := newRig(t, 0)
	r.startWith("--run-timeout", timeout.String(), "--max-attempts", "10")
	hung, hungDSN := newProxy(t, r.appDSN)
	r.addTargetDSN("blackhole", hungDSN)
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text);", "DROP TABLE users;",
	}))
	other := createDatabase(t, r.appDB+"_other")
	r.mustCLI("target", "add", "sibling", "--provider", "static", "--dsn", other)

	id := r.createRun(migration{
		v2, "plan",
		"ALTER TABLE users ADD COLUMN plan text; SELECT pg_sleep(3);", "ALTER TABLE users DROP COLUMN plan;",
	})
	r.claimer(id)
	r.waitActive("SELECT pg_sleep")
	hung.set(proxyHang)

	sibling, err := r.client().CreateRun(context.Background(), connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "sibling",
		Files:  files(migration{v1, "t", "CREATE TABLE t (id int);", "DROP TABLE t;"}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	sib, siblingSeconds := watchRun(t, r, sibling.Msg.RunId)
	if sib.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("sibling run while the other target hung: state %s, error %s", sib.State, sib.Error)
	}
	cutLoose := timed(func() {
		waitUntil(t, settle, "the run timeout cuts the hung run loose", func() bool { return r.getRun(id).Error != "" })
	})
	stuck := r.getRun(id)
	// The black-holed session holds the target's advisory lock until TCP gives up; standing in for that.
	orphans := terminate(t, r.appDB, "%")
	hung.set(proxyPass)

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if n := journalRows(t, r.appDSN, v2, "done"); n != 2 {
		t.Fatalf("done journal rows = %d, want 2", n)
	}
	report(t, "chaos/target_hang",
		"run_timeout", timeout, "sibling_seconds_while_hung", siblingSeconds.Seconds(),
		"seconds_until_cut_loose", cutLoose.Seconds(), "state_when_cut", stuck.State,
		"retries", stuck.Retries, "error", stuck.Error, "orphan_sessions", orphans,
		"converge_seconds_after_heal", elapsed.Seconds())
}

func TestChaosBackendTerminatedMidStatement(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	// Retrying past the fault is the point; the attempt budget must not end the run first.
	r.startWith("--max-attempts", "10")
	r.addTarget("terminated")
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text);", "DROP TABLE users;",
	}))

	id := r.createRun(migration{
		v2, "plan",
		"ALTER TABLE users ADD COLUMN plan text; SELECT pg_sleep(10);", "ALTER TABLE users DROP COLUMN plan;",
	})
	r.claimer(id)
	r.waitActive("SELECT pg_sleep")
	if n := terminate(t, r.appDB, "SELECT pg_sleep%"); n != 1 {
		t.Fatalf("terminated %d backends, want 1", n)
	}
	waitUntil(t, settle, "the terminated statement is reported", func() bool { return r.getRun(id).Retries >= 1 })
	failure := r.getRun(id).Error

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if n := journalRows(t, r.appDSN, v2, "done"); n != 2 {
		t.Fatalf("done journal rows = %d, want 2", n)
	}
	expectContains(t, failure, "transient:")
	report(t, "chaos/backend_terminated", "retries", run.Retries, "converge_seconds", elapsed.Seconds(), "first_error", failure)
}

func TestChaosLockTimeoutInContractPhase(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	// Retrying past the fault is the point; the attempt budget must not end the run first.
	r.startWith("--max-attempts", "10")
	r.addTarget("contended")
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text, plan text);", "DROP TABLE users;",
	}))

	dir := migrationDir(t,
		migration{v2, "add_plan_v2", "ALTER TABLE users ADD COLUMN plan_v2 text;", "ALTER TABLE users DROP COLUMN plan_v2;"},
		migration{v3, "drop_plan", "ALTER TABLE users DROP COLUMN plan;", "ALTER TABLE users ADD COLUMN plan text;"})
	expectContains(t, r.mustMigrate(dir, "--rollout", "expand-contract", "--ack", "H003", "--lock-timeout", "500ms"), "awaiting_contract")
	held := r.latestRun()

	release := holdLock(t, r.appDSN, "LOCK TABLE users IN ACCESS EXCLUSIVE MODE")
	r.mustCLI("run", "confirm", held.Id)
	waitUntil(t, settle, "the contract phase hits the lock timeout", func() bool { return r.getRun(held.Id).Retries >= 1 })
	blocked := r.getRun(held.Id)
	expectContains(t, blocked.Error, "transient:", "55P03")
	if !columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("plan must survive a contract phase that never got its lock")
	}
	release()

	run, elapsed := watchRun(t, r, held.Id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", held.Id, run.State, run.Error)
	}
	if columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("the contract phase must drop plan once the lock frees")
	}
	report(t, "chaos/lock_timeout_contract",
		"retries", run.Retries, "state_while_blocked", blocked.State, "converge_seconds", elapsed.Seconds())
}

func TestChaosConcurrentDDLBreaksAStatement(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	r.addTarget("raced")
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text); CREATE TABLE other (id bigint);",
		"DROP TABLE users; DROP TABLE other;",
	}))

	id := r.createRun(migration{
		v2, "two",
		"ALTER TABLE users ADD COLUMN plan text; SELECT pg_sleep(6); ALTER TABLE other ADD COLUMN note text;",
		"ALTER TABLE users DROP COLUMN plan;",
	})
	r.claimer(id)
	r.waitActive("SELECT pg_sleep")
	execSQL(t, r.appDSN, "DROP TABLE other")

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_FAILED {
		t.Fatalf("run %s: state %s, want failed (error: %s)", id, run.State, run.Error)
	}
	expectContains(t, run.Error, "sql:", `relation "other" does not exist`)
	if n := journalRows(t, r.appDSN, v2, "done"); n != 2 {
		t.Fatalf("done journal rows = %d, want the two statements that did run", n)
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations WHERE version = $1`, v2); n != 0 {
		t.Fatalf("godwit.migrations rows = %d, want 0: a failed migration is not applied", n)
	}
	if !columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("the statements that committed must stand")
	}
	report(t, "chaos/concurrent_ddl", "attempts", run.Attempts, "seconds", elapsed.Seconds(), "error", run.Error)
}

func TestChaosPrivilegeLostBetweenPlanAndApply(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	execSQL(t, adminDSN, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'godwit_limited') THEN
			CREATE ROLE godwit_limited LOGIN PASSWORD 'limited';
		END IF;
	END $$`)
	execSQL(t, r.appDSN, "GRANT ALL ON SCHEMA public TO godwit_limited")
	execSQL(t, r.appDSN, "GRANT CREATE ON DATABASE "+r.appDB+" TO godwit_limited")
	limited := strings.Replace(r.appDSN, "godwit:godwit@", "godwit_limited:limited@", 1)
	r.addTargetDSN("limited", limited)

	dir := migrationDir(t, migration{v1, "users", "CREATE TABLE users (id bigint PRIMARY KEY);", "DROP TABLE users;"})
	r.mustCLI("plan", "--target", r.target, "--dir", dir)
	execSQL(t, r.appDSN, "REVOKE CREATE ON SCHEMA public FROM godwit_limited")

	id := r.createRun(migration{v1, "users", "CREATE TABLE users (id bigint PRIMARY KEY);", "DROP TABLE users;"})
	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_FAILED {
		t.Fatalf("run %s: state %s, want failed (error: %s)", id, run.State, run.Error)
	}
	expectContains(t, run.Error, "sql:", "permission denied")
	if run.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1: a lost privilege is not transient", run.Attempts)
	}
	if query[bool](t, r.appDSN, `SELECT to_regclass('users') IS NOT NULL`) {
		t.Fatal("users must not exist after a refused CREATE")
	}
	report(t, "chaos/privilege_lost", "attempts", run.Attempts, "seconds", elapsed.Seconds(), "error", run.Error)
}

func TestChaosIndexOfThatNameIsNotTheOneThePlanBuilds(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("impostor")
	r.mustMigrate(migrationDir(t, migration{v1, "big", "CREATE TABLE big (id bigserial PRIMARY KEY, v int);", "DROP TABLE big;"}))
	seedRows(t, r.appDSN, "big", 50_000)

	release := holdLock(t, r.appDSN, "LOCK TABLE big IN SHARE MODE")
	id := r.createRunFull(&godwitv1.CreateRunRequest{
		Files: files(migration{
			v2, "idx",
			"CREATE INDEX CONCURRENTLY big_v_idx ON big (v);", "DROP INDEX CONCURRENTLY big_v_idx;",
		}),
		LockTimeout: "120s",
	})
	victim := r.claimer(id)
	waitJournal(t, r.appDSN, v2, 0, "intent")
	r.waitActive("CREATE INDEX CONCURRENTLY")
	r.reap(victim, "CREATE INDEX CONCURRENTLY%")
	// Somebody else's index, of the name the plan reserved, over a different column. A plain CREATE INDEX
	// wants only SHARE, so it lands beside the lock the test is holding once the reap clears the queue.
	execSQL(t, r.appDSN, "CREATE INDEX big_v_idx ON big (id)")
	release()

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_FAILED {
		t.Fatalf("run %s: state %s, want failed (error: %s)", id, run.State, run.Error)
	}
	expectContains(t, run.Error, `index "big_v_idx" already exists as`, "is not what this statement builds")
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations WHERE version = $1`, v2); n != 0 {
		t.Fatalf("godwit.migrations rows = %d, want 0: the index the plan promised was never built", n)
	}
	if n := journalRows(t, r.appDSN, v2, "done"); n != 0 {
		t.Fatalf("done journal rows = %d, want 0", n)
	}
	def := query[string](t, r.appDSN, `SELECT pg_get_indexdef('big_v_idx'::regclass)`)
	if !strings.Contains(def, "(id)") {
		t.Fatalf("big_v_idx = %q: godwit must not drop an index it did not build", def)
	}
	report(t, "chaos/foreign_index_same_name",
		"attempts", run.Attempts, "seconds", elapsed.Seconds(), "error", run.Error)
}

func TestChaosStoreBlipMidRun(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	blip, blipDSN := newProxy(t, r.storeDSN)
	rep := r.startWith("--store-dsn", blipDSN)
	r.addTarget("blipped")
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text);", "DROP TABLE users;",
	}))

	id := r.createRun(migration{
		v2, "plan",
		"ALTER TABLE users ADD COLUMN plan text; SELECT pg_sleep(20);", "ALTER TABLE users DROP COLUMN plan;",
	})
	r.claimer(id)
	r.waitActive("SELECT pg_sleep")
	// Longer than one beat (--lease-ttl 5s, so 1.25s), well short of the lease.
	blip.set(proxyCut)
	time.Sleep(2 * time.Second)
	blip.set(proxyPass)

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if run.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1: a store blip must not cost the lease", run.Attempts)
	}
	if n := rep.logs.count("heartbeat lost"); n != 0 {
		t.Fatalf("heartbeat lost %d times, want 0", n)
	}
	retried := rep.logs.count("heartbeat failed; retrying")
	if retried == 0 {
		t.Fatal("the blip must have cost at least one beat")
	}
	if n := journalRows(t, r.appDSN, v2, "done"); n != 2 {
		t.Fatalf("done journal rows = %d, want 2", n)
	}
	report(t, "chaos/store_blip",
		"attempts", run.Attempts, "beats_retried", retried, "converge_seconds", elapsed.Seconds())
}

func TestChaosOrphanHoldsTheAdvisoryLock(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	r.addTarget("orphaned")
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text);", "DROP TABLE users;",
	}))

	release := holdAdvisoryLock(t, r.appDSN, r.appDB)
	id := r.createRun(migration{v2, "plan", "ALTER TABLE users ADD COLUMN plan text;", "ALTER TABLE users DROP COLUMN plan;"})
	// The wait is the executor's own, not the run timeout: without it this never reports at all.
	blocked := timed(func() {
		waitUntil(t, 90*time.Second, "the advisory lock wait is reported", func() bool { return r.getRun(id).Error != "" })
	})
	stuck := r.getRun(id)
	expectContains(t, stuck.Error, "transient:", "acquire advisory lock on "+r.appDB, `application_name "godwit"`, "SQLSTATE 57014")
	release()

	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if !columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("column plan missing after the orphan let go")
	}
	report(t, "chaos/orphan_advisory_lock",
		"seconds_until_reported", blocked.Seconds(), "attempts", run.Attempts,
		"retries", run.Retries, "error", stuck.Error, "converge_seconds_after_release", elapsed.Seconds())
}

func TestChaosDiskFullRetries(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	r.addTarget("nospace")
	r.mustMigrate(migrationDir(t, migration{v1, "seq", "CREATE SEQUENCE attempts;", "DROP SEQUENCE attempts;"}))

	// The scratch database admission validates on is not the target, and only the target runs out of disk.
	id := r.createRun(migration{
		v2, "fills_the_disk",
		fmt.Sprintf(`DO $$ BEGIN IF current_database() = '%s' THEN
			IF nextval('attempts') < 2 THEN
				RAISE EXCEPTION 'could not extend file: No space left on device' USING ERRCODE = '53100';
			END IF;
		END IF; END $$;`, r.appDB),
		"SELECT 1;",
	})
	run, elapsed := watchRun(t, r, id)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("run %s: state %s, error %s", id, run.State, run.Error)
	}
	if run.Retries != 1 {
		t.Fatalf("retries = %d, want 1: a full disk is transient", run.Retries)
	}
	report(t, "chaos/disk_full", "retries", run.Retries, "attempts", run.Attempts, "seconds", elapsed.Seconds())
}

func TestChaosResumeAfterTheMigrationShrank(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	r.addTarget("shrunk")
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text);", "DROP TABLE users;",
	}))
	execSQL(t, r.appDSN, "INSERT INTO users VALUES (1, 'a'), (2, 'a')")

	const first = "ALTER TABLE users ADD COLUMN c1 text;"
	long := migration{v2, "wide", first +
		" ALTER TABLE users ADD COLUMN c2 text;" +
		" ALTER TABLE users ADD CONSTRAINT users_email_uniq UNIQUE (email);", "SELECT 1;"}
	failed, _ := watchRun(t, r, r.createRunFull(&godwitv1.CreateRunRequest{
		Files: files(long), AcknowledgeHazards: []string{"H010"},
	}))
	if failed.State != godwitv1.RunState_RUN_STATE_FAILED {
		t.Fatalf("first run: state %s, want failed (error: %s)", failed.State, failed.Error)
	}
	if n := journalRows(t, r.appDSN, v2, "done"); n != 2 {
		t.Fatalf("done journal rows after the failure = %d, want 2", n)
	}

	short := migration{v2, "wide", first, "SELECT 1;"}
	run, elapsed := watchRun(t, r, r.createRun(short))
	if run.State != godwitv1.RunState_RUN_STATE_FAILED {
		t.Fatalf("shrunk run: state %s, want failed (error: %s)", run.State, run.Error)
	}
	expectContains(t, run.Error, "refusing to resume")
	for _, rep := range r.all() {
		if rep.cmd.ProcessState != nil {
			t.Fatalf("replica exited: %s", rep.cmd.ProcessState)
		}
	}
	report(t, "chaos/migration_shrank", "attempts", run.Attempts, "seconds", elapsed.Seconds(), "error", run.Error)
}
