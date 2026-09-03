//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func columnExists(t *testing.T, dsn, table, column string) bool {
	t.Helper()

	return query[bool](t, dsn,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)`, table, column)
}

func TestLockTimeoutRetriesThenSucceeds(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	r.addTarget("t")
	r.mustMigrate(migrationDir(t, migration{v1, "t", "CREATE TABLE t (id int);", "DROP TABLE t;"}))

	ctx := context.Background()
	holder := connectDB(t, r.appDSN)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE t IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}

	addX := migration{v2, "add_x", "ALTER TABLE t ADD COLUMN x int;", "ALTER TABLE t DROP COLUMN x;"}
	dir := migrationDir(t, addX)
	r.mustCLI("plan", "--target", r.target, "--dir", dir)
	first, err := r.client().CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: r.target, Files: files(addX), LockTimeout: "500ms"}))
	if err != nil || first.Msg.PlanId == "" || first.Msg.Reattached {
		t.Fatalf("first = %+v, err = %v", first, err)
	}
	id := first.Msg.RunId
	waitUntil(t, 20*time.Second, "first transient retry", func() bool { return r.getRun(id).Retries >= 1 })
	retrying := r.getRun(id)
	expectContains(t, retrying.Error, "transient:", "SQLSTATE 55P03")
	if retrying.State == godwitv1.RunState_RUN_STATE_FAILED || retrying.State == godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION {
		t.Fatalf("state = %s while retrying", retrying.State)
	}

	again, err := r.client().CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: r.target, Files: files(addX), LockTimeout: "500ms"}))
	if err != nil || !again.Msg.Reattached || again.Msg.RunId != id {
		t.Fatalf("again = %+v, err = %v", again, err)
	}
	if runs := r.listRuns(); len(runs) != 2 {
		t.Fatalf("runs = %d, want the table run and one retried run", len(runs))
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	expectContains(t, r.mustCLI("run", "watch", id), "succeeded")
	run := r.getRun(id)
	if run.Retries < 1 || run.NotBefore != nil || run.Error != "" {
		t.Fatalf("run = %+v", run)
	}
	if !columnExists(t, r.appDSN, "t", "x") {
		t.Fatal("column x missing after retry")
	}
	expectContains(t, r.metrics(),
		`godwit_statement_failures_total{reason="lock_timeout",target="t"}`,
		`godwit_run_retries_total{code="55P03",target="t"}`)
}

func TestExpandContractConfirm(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("app")
	r.mustMigrate(migrationDir(t, migration{
		v1, "users",
		"CREATE TABLE users (id bigint PRIMARY KEY, email text, plan text);", "DROP TABLE users;",
	}))

	dir := migrationDir(t,
		migration{v2, "add_plan_v2", "ALTER TABLE users ADD COLUMN plan_v2 text;", "ALTER TABLE users DROP COLUMN plan_v2;"},
		migration{v3, "drop_plan", "ALTER TABLE users DROP COLUMN plan;", "ALTER TABLE users ADD COLUMN plan text;"},
	)
	expectContains(t, r.mustMigrate(dir, "--rollout", "expand-contract", "--ack", "H003"), "awaiting_contract")
	run := r.latestRun()
	if run.State != godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT {
		t.Fatalf("state = %s, want awaiting_contract", run.State)
	}
	if !columnExists(t, r.appDSN, "users", "plan") || !columnExists(t, r.appDSN, "users", "plan_v2") {
		t.Fatal("expand phase must keep plan and add plan_v2")
	}

	r.mustCLI("run", "confirm", run.Id)
	expectContains(t, r.mustCLI("run", "watch", run.Id), "succeeded")
	if columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("contract phase must drop plan")
	}
}

func TestRevertLatest(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("app")
	r.mustMigrate(migrationDir(t, usersTable))
	first := r.latestRun()
	r.mustMigrate(migrationDir(t, migration{
		v2, "plan",
		"ALTER TABLE users ADD COLUMN plan text;", "ALTER TABLE users DROP COLUMN plan;",
	}))
	second := r.latestRun()

	code, _, errOut := r.cli("revert", first.Id, "--ack", "H002")
	if code != 1 {
		t.Fatalf("revert of a non-latest run exit = %d, want 1", code)
	}
	expectContains(t, errOut, "is newer and still stands")
	_, err := r.client().RevertRun(context.Background(), connect.NewRequest(&godwitv1.RevertRunRequest{
		RunId: first.Id, AcknowledgeHazards: []string{"H002"},
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("revert code = %v, want failed_precondition", connect.CodeOf(err))
	}

	expectContains(t, r.mustCLI("revert", second.Id, "--ack", "H003", "--dry-run"),
		"1 migration(s), reverse order of application", "20260901120001_plan (down)")
	if !columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("a dry run must not touch the target")
	}
	expectContains(t, r.mustCLI("revert", second.Id, "--ack", "H003"), "succeeded")
	r.expectRun(second.Id, godwitv1.RunState_RUN_STATE_REVERTED, 1)
	expectContains(t, r.mustCLI("run", "get", second.Id), "created_by: "+actor)
	expectContains(t, r.mustCLI("audit", "--target", "app", "--limit", "2"),
		actor+"    run.revert", "reverts="+second.Id+" migrations=1 acked=H003", actor+"    run.create", "migrations=1")
	if !r.alive().logs.has("api call", "actor", actor) {
		t.Fatal("access log must carry the actor")
	}
	if columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("revert must drop plan")
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations WHERE version = $1`, v2); n != 0 {
		t.Fatalf("version %d still recorded after revert", v2)
	}
	if !columnExists(t, r.appDSN, "users", "email") {
		t.Fatal("the revert must leave the first run's migration alone")
	}
}

func TestDriftDetectAndAccept(t *testing.T) {
	t.Parallel()
	r := newRig(t, 2)
	r.addTarget("app")
	r.mustMigrate(migrationDir(t, usersTable))

	execSQL(t, r.appDSN, "CREATE TABLE rogue (id int)")
	waitUntil(t, 10*time.Second, "drift event", func() bool { return len(r.driftEvents()) == 1 })
	expectContains(t, r.mustCLI("drift", "check", "app"), "drifted", "rogue")

	r.mustCLI("drift", "accept", "app")
	expectContains(t, r.mustCLI("drift", "check", "app"), "no drift")
	waitUntil(t, 10*time.Second, "every drift event resolved", func() bool {
		events := r.driftEvents()
		for _, ev := range events {
			if ev.ResolvedAt == nil {
				return false
			}
		}

		return len(events) == 1
	})
}

func TestHazardGateAndValidation(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	r.addTarget("app")
	r.mustMigrate(migrationDir(t, usersTable))

	drop := migration{v2, "drop_users", "DROP TABLE users;", "SELECT 1;"}
	code, _, errOut := r.migrate(migrationDir(t, drop))
	if code != 1 {
		t.Fatalf("unacknowledged DROP TABLE exit = %d, want 1", code)
	}
	expectContains(t, errOut, "H002")
	_, err := r.client().CreateRun(context.Background(), connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: files(drop)}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("hazard code = %v, want failed_precondition", connect.CodeOf(err))
	}
	if n := len(r.listRuns()); n != 1 {
		t.Fatalf("runs after refusal = %d, want 1", n)
	}

	expectContains(t, r.mustMigrate(migrationDir(t, drop), "--ack", "H002"), "succeeded")
	if query[bool](t, r.appDSN, `SELECT to_regclass('users') IS NOT NULL`) {
		t.Fatal("users still exists after acknowledged DROP TABLE")
	}

	broken := migration{v3, "broken", "ALTER TABLE nope ADD COLUMN x int;", "SELECT 1;"}
	code, _, errOut = r.migrate(migrationDir(t, broken))
	if code != 1 {
		t.Fatalf("invalid migration exit = %d, want 1", code)
	}
	expectContains(t, errOut, "failed validation", `relation "nope" does not exist`)
	_, err = r.client().CreateRun(context.Background(), connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: files(broken)}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("validation code = %v, want invalid_argument", connect.CodeOf(err))
	}
	if n := len(r.listRuns()); n != 2 {
		t.Fatalf("runs after validation refusal = %d, want 2", n)
	}
}

func TestBaselineExistingDatabase(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	r.addTarget("app")
	execSQL(t, r.appDSN, usersTable.up)
	dir := migrationDir(t, usersTable, migration{v2, "plan", "ALTER TABLE users ADD COLUMN plan text;", "ALTER TABLE users DROP COLUMN plan;"})

	out := r.mustCLI("target", "baseline", "app", "--dir", dir, "--version", "20260901120000")
	expectContains(t, out, "baselined to version 20260901120000")
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations`); n != 1 {
		t.Fatalf("applied versions after baseline = %d, want 1", n)
	}
	if columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("baseline must not execute migrations")
	}
	run := r.latestRun()
	if run.Kind != "baseline" || run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED {
		t.Fatalf("baseline run = %+v", run)
	}
	expectContains(t, r.mustCLI("runs", "--target", "app"), "baseline")

	expectContains(t, r.mustMigrate(dir), "succeeded")
	if !columnExists(t, r.appDSN, "users", "plan") {
		t.Fatal("migration after baseline must apply the newer version")
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations`); n != 2 {
		t.Fatalf("applied versions after migrate = %d, want 2", n)
	}

	code, _, errOut := r.cli("target", "baseline", "app", "--dir", dir, "--version", "20260901120000")
	if code != 1 {
		t.Fatalf("second baseline exit = %d, want 1", code)
	}
	expectContains(t, errOut, "already has applied migrations")
	code, _, errOut = r.cli("revert", run.Id)
	if code != 1 {
		t.Fatalf("revert baseline exit = %d, want 1", code)
	}
	expectContains(t, errOut, "baseline runs cannot be reverted")
}

func TestOutOfOrderRefusedUnlessAllowed(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	r.addTarget("app")
	r.mustMigrate(migrationDir(t, usersTable,
		migration{v3, "plan", "ALTER TABLE users ADD COLUMN plan text;", "ALTER TABLE users DROP COLUMN plan;"}))

	dir := migrationDir(t, usersTable,
		migration{v2, "name", "ALTER TABLE users ADD COLUMN name text;", "ALTER TABLE users DROP COLUMN name;"},
		migration{v3, "plan", "ALTER TABLE users ADD COLUMN plan text;", "ALTER TABLE users DROP COLUMN plan;"})
	code, _, errOut := r.migrate(dir)
	if code != 1 {
		t.Fatalf("out-of-order migrate exit = %d, want 1", code)
	}
	expectContains(t, errOut, "out-of-order migrations 20260901120001", "newest applied version on app is 20260901120002")
	if n := len(r.listRuns()); n != 1 {
		t.Fatalf("runs after refusal = %d, want 1", n)
	}

	expectContains(t, r.mustMigrate(dir, "--allow-out-of-order"), "succeeded")
	if !columnExists(t, r.appDSN, "users", "name") {
		t.Fatal("column name missing after the allowed out-of-order run")
	}
	if n := query[int](t, r.appDSN, `SELECT count(*) FROM godwit.migrations`); n != 3 {
		t.Fatalf("applied versions = %d, want 3", n)
	}
}

func TestDryRunPlansWithoutQueueing(t *testing.T) {
	t.Parallel()
	r := newRig(t, 1)
	r.addTarget("app")
	r.mustMigrate(migrationDir(t, usersTable))

	dir := migrationDir(t, usersTable,
		migration{v2, "name", "ALTER TABLE users ADD COLUMN name text;", "ALTER TABLE users DROP COLUMN name;"},
		migration{v3, "drop_id", "ALTER TABLE users DROP COLUMN id;", "ALTER TABLE users ADD COLUMN id int;"})
	out := r.mustMigrate(dir, "--dry-run", "--rollout", "expand-contract", "--ack", "H003", "--format", "markdown")
	expectContains(t, out, "## godwit dry run", "Target `app`, rollout `expand-contract`, validated on a scratch database.",
		"| `20260901120000_users` | up | 0 | tx | `CREATE TABLE users", "| expand | applied |",
		"| `20260901120001_name` | up | 0 | tx | `ALTER TABLE users ADD COLUMN name text` |  | expand | pending |",
		"| `20260901120002_drop_id` | up | 0 | tx | `ALTER TABLE users DROP COLUMN id` | H003", "| contract | pending |")
	if columnExists(t, r.appDSN, "users", "name") {
		t.Fatal("dry run must not touch the target")
	}
	if n := len(r.listRuns()); n != 1 {
		t.Fatalf("runs after dry run = %d, want 1", n)
	}

	code, _, errOut := r.migrate(dir, "--dry-run")
	if code != 1 {
		t.Fatalf("unacknowledged dry run exit = %d, want 1", code)
	}
	expectContains(t, errOut, "H003")
	if n := len(r.listRuns()); n != 1 {
		t.Fatalf("runs after refused dry run = %d, want 1", n)
	}
}
