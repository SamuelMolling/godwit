package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/engine"
)

func columnPlans(t *testing.T) []engine.Plan {
	t.Helper()
	plans, err := buildPlans([]engine.Migration{
		{Version: 20260901120001, Name: "a", Checksum: "ca", UpSQL: "ALTER TABLE t ADD COLUMN a int;", DownSQL: "ALTER TABLE t DROP COLUMN a;"},
		{Version: 20260901120002, Name: "b", Checksum: "cb", UpSQL: "ALTER TABLE t ADD COLUMN b int;", DownSQL: "ALTER TABLE t DROP COLUMN b;"},
	}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}

	return plans
}

func TestValidateReportsEffects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, _ := newScheduler(t, s, Config{Holder: "h"})
	queueRun(t, s, "dddddddd-0000-0000-0000-000000000001", goodFiles())
	sched.Tick(ctx)
	waitState(t, s, "dddddddd-0000-0000-0000-000000000001", StateSucceeded)

	v := NewValidator(pool, s, func() string { return "effects" })
	val, err := v.Validate(ctx, "app", columnPlans(t), "public")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(val.Base, "column public.t.id integer") {
		t.Fatalf("base = %q", val.Base)
	}
	if len(val.Effects) != 2 || len(val.Fingerprints) != 3 {
		t.Fatalf("validation = %+v", val)
	}
	if len(val.Effects[0]) != 1 || !strings.Contains(val.Effects[0][0], "+ column") || !strings.Contains(val.Effects[0][0], ".t.a integer") {
		t.Fatalf("effect 1 = %v", val.Effects[0])
	}
	if len(val.Effects[1]) != 1 || !strings.Contains(val.Effects[1][0], ".t.b integer") {
		t.Fatalf("effect 2 = %v", val.Effects[1])
	}
	if val.Fingerprints[0] == val.Fingerprints[1] || val.Fingerprints[1] == val.Fingerprints[2] {
		t.Fatalf("fingerprints = %v", val.Fingerprints)
	}
}

func TestMirrorSearchPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := mirrorSearchPath(ctx, mock, ""); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(`CREATE SCHEMA IF NOT EXISTS "app"`).WillReturnResult(pgxmock.NewResult("CREATE", 0))
	mock.ExpectExec(`SET search_path TO "app", "pg_catalog"`).WillReturnError(errBoom)
	if err := mirrorSearchPath(ctx, mock, "app,pg_catalog"); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "mirror search path") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSnapshotFails(t *testing.T) {
	ctx := context.Background()
	s, pool := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "plain", map[string]string{"dsn": "x"}); err != nil {
		t.Fatal(err)
	}
	orig := snapshotScratch
	defer func() { snapshotScratch = orig }()

	calls := 0
	failOn := 1
	snapshotScratch = func(ctx context.Context, db engine.DB) (string, string, error) {
		calls++
		if calls == failOn {
			return "", "", errBoom
		}

		return orig(ctx, db)
	}
	v := NewValidator(pool, s, func() string { return "snapfail" })
	plans, err := buildPlans([]engine.Migration{{Version: 1, Name: "t", Checksum: "c", UpSQL: upBody, DownSQL: "DROP TABLE t;"}}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(ctx, "app", plans, ""); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "snapshot scratch database") {
		t.Fatalf("err = %v", err)
	}

	calls, failOn = 0, 2
	if _, err := v.Validate(ctx, "app", plans, ""); !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "snapshot scratch database") {
		t.Fatalf("err = %v", err)
	}
}

func detectFixture(t *testing.T, sql ...string) ([]PlanMigration, []engine.Plan, Validation) {
	t.Helper()
	migs := make([]engine.Migration, 0, len(sql))
	for i, s := range sql {
		migs = append(migs, engine.Migration{Version: int64(i + 1), Name: "m", Checksum: "c", UpSQL: s, DownSQL: "SELECT 1;"})
	}
	plans, err := buildPlans(migs, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	val := Validation{Base: "base", Fingerprints: []string{"f0"}}
	for i := range plans {
		val.Effects = append(val.Effects, []string{"+ line" + string(rune('a'+i))})
		val.Fingerprints = append(val.Fingerprints, "f"+string(rune('1'+i)))
	}

	return BuildPlanMigrations(RolloutDirect, plans, AppliedSet{}), plans, val
}

func TestDetectPrefix(t *testing.T) {
	t.Parallel()
	ddl := "ALTER TABLE t ADD COLUMN a int;"
	migs, plans, val := detectFixture(t, ddl, ddl, ddl)

	if drift := Detect(migs, plans, val, Observation{Fingerprint: "f2"}); drift != "" {
		t.Fatalf("drift = %q", drift)
	}
	for i, want := range []bool{true, true, false} {
		if migs[i].AlreadyApplied != want || migs[i].Note != "" {
			t.Fatalf("migration %d = %+v", i, migs[i])
		}
	}
	if migs[0].Effect != "+ linea" || migs[1].Effect != "+ lineb" || migs[2].Effect != "" {
		t.Fatalf("effects = %q %q %q", migs[0].Effect, migs[1].Effect, migs[2].Effect)
	}

	migs, plans, val = detectFixture(t, ddl, ddl)
	if drift := Detect(migs, plans, val, Observation{Fingerprint: "f0"}); drift != "" || migs[0].AlreadyApplied || migs[1].AlreadyApplied {
		t.Fatalf("k=0: drift = %q, migs = %+v", drift, migs)
	}
}

func TestDetectRefusals(t *testing.T) {
	t.Parallel()
	ddl := "ALTER TABLE t ADD COLUMN a int;"

	migs, plans, val := detectFixture(t, "INSERT INTO t VALUES (1);", ddl)
	if drift := Detect(migs, plans, val, Observation{Fingerprint: "f2"}); drift != "" {
		t.Fatalf("drift = %q", drift)
	}
	if migs[0].AlreadyApplied || migs[0].Note != engine.OpaqueDML || migs[1].AlreadyApplied || migs[1].Note != "" {
		t.Fatalf("dml: %+v", migs)
	}

	migs, plans, val = detectFixture(t, "CREATE FUNCTION f() RETURNS int AS 'SELECT 1' LANGUAGE sql;", ddl)
	Detect(migs, plans, val, Observation{Fingerprint: "f2"})
	if migs[0].AlreadyApplied || migs[0].Note != engine.OpaqueUnknown || migs[1].AlreadyApplied {
		t.Fatalf("function: %+v", migs)
	}

	migs, plans, val = detectFixture(t, ddl, ddl)
	val.Effects[0] = nil
	Detect(migs, plans, val, Observation{Fingerprint: "f2"})
	if migs[0].AlreadyApplied || migs[0].Note != engine.OpaqueUnknown || migs[1].AlreadyApplied {
		t.Fatalf("empty effect: %+v", migs)
	}

	migs, plans, val = detectFixture(t, ddl, ddl)
	migs[0].Applied = true
	Detect(migs, plans, val, Observation{Fingerprint: "f2"})
	if migs[0].AlreadyApplied || migs[0].Note != "" || !migs[1].AlreadyApplied {
		t.Fatalf("applied in prefix: %+v", migs)
	}
}

func TestDetectNoPrefixIsDrift(t *testing.T) {
	t.Parallel()
	ddl := "ALTER TABLE t ADD COLUMN a int;"
	migs, plans, val := detectFixture(t, ddl, ddl, ddl, ddl)
	val.Base = "base\nkept"
	val.Effects[3] = nil
	migs[2].Applied = true
	obs := Observation{Fingerprint: "other", Definition: "kept\nlineb\nlinec"}

	drift := Detect(migs, plans, val, obs)
	if drift != "- base\n+ lineb\n+ linec" {
		t.Fatalf("drift = %q", drift)
	}
	for _, m := range migs {
		if m.AlreadyApplied || m.Effect != "" {
			t.Fatalf("marked without prefix: %+v", m)
		}
	}
	if migs[0].Note != "" || migs[1].Note != "effect is present but not as a prefix" || migs[2].Note != "" || migs[3].Note != "" {
		t.Fatalf("notes = %q %q %q %q", migs[0].Note, migs[1].Note, migs[2].Note, migs[3].Note)
	}
}

type observeFails struct{ Engine }

func (observeFails) Observe(context.Context, string) (Observation, error) {
	return Observation{}, errBoom
}

func markRun(t *testing.T, s *Store, id, planID string) {
	t.Helper()
	if err := s.CreateRun(context.Background(), id, "app", RolloutDirect, goodFiles(), Timeouts{}, Provenance{}, planID); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerMarkOnlyFromPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, targetDSN := newScheduler(t, s, Config{Holder: "h", MaxAttempts: 1})
	tg, err := pgx.Connect(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tg.Close(context.Background()) })
	if _, err := tg.Exec(ctx, upBody); err != nil {
		t.Fatal(err)
	}
	obs, err := PGEngine{}.Observe(ctx, targetDSN)
	if err != nil {
		t.Fatal(err)
	}

	plan := storedPlan(planA)
	plan.SchemaFingerprint = "stale"
	plan.Migrations = []PlanMigration{{Version: 20260901120000, Name: "t", Checksum: "c", Phase: PhaseExpand, AlreadyApplied: true}}
	if err := s.SavePlan(ctx, plan, goodFiles()); err != nil {
		t.Fatal(err)
	}
	markRun(t, s, "eeeeeeee-0000-0000-0000-000000000001", planA)
	sched.Tick(ctx)
	if r := waitState(t, s, "eeeeeeee-0000-0000-0000-000000000001", StateFailed); !strings.Contains(r.Error, "target schema changed since plan "+planA) {
		t.Fatalf("error = %q", r.Error)
	}

	plan.ID, plan.Key, plan.SchemaFingerprint = planB, "k2", obs.Fingerprint
	if err := s.SavePlan(ctx, plan, goodFiles()); err != nil {
		t.Fatal(err)
	}
	markRun(t, s, "eeeeeeee-0000-0000-0000-000000000002", planB)
	sched.Tick(ctx)
	waitState(t, s, "eeeeeeee-0000-0000-0000-000000000002", StateSucceeded)
	var stmts int
	if err := tg.QueryRow(ctx, `SELECT r.stmt_count FROM godwit.runs r JOIN godwit.migrations m USING (version) WHERE r.state = 'succeeded'`).Scan(&stmts); err != nil || stmts != 0 {
		t.Fatalf("stmt_count = %d, err = %v", stmts, err)
	}

	if _, err := tg.Exec(ctx, "ALTER TABLE t ADD COLUMN later int"); err != nil {
		t.Fatal(err)
	}
	plan.ID, plan.Key = planC, "k3"
	if err := s.SavePlan(ctx, plan, goodFiles()); err != nil {
		t.Fatal(err)
	}
	markRun(t, s, "eeeeeeee-0000-0000-0000-000000000003", planC)
	sched.Tick(ctx)
	waitState(t, s, "eeeeeeee-0000-0000-0000-000000000003", StateSucceeded)

	plan.ID, plan.Key, plan.Migrations[0].AlreadyApplied = "aaaaaaaa-0000-0000-0000-000000000004", "k4", false
	if err := s.SavePlan(ctx, plan, goodFiles()); err != nil {
		t.Fatal(err)
	}
	markRun(t, s, "eeeeeeee-0000-0000-0000-000000000004", plan.ID)
	sched.Tick(ctx)
	waitState(t, s, "eeeeeeee-0000-0000-0000-000000000004", StateSucceeded)

	plan.ID, plan.Key, plan.Migrations[0].AlreadyApplied = "aaaaaaaa-0000-0000-0000-000000000005", "k5", true
	if err := s.SavePlan(ctx, plan, goodFiles()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE cp_plans SET plan = '"corrupt"' WHERE id = $1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	markRun(t, s, "eeeeeeee-0000-0000-0000-000000000005", plan.ID)
	sched.Tick(ctx)
	if r := waitState(t, s, "eeeeeeee-0000-0000-0000-000000000005", StateFailed); !strings.Contains(r.Error, "bound plan "+plan.ID) {
		t.Fatalf("error = %q", r.Error)
	}

	plan.ID, plan.Key = "aaaaaaaa-0000-0000-0000-000000000006", "k6"
	if err := s.SavePlan(ctx, plan, goodFiles()); err != nil {
		t.Fatal(err)
	}
	markRun(t, s, "eeeeeeee-0000-0000-0000-000000000006", plan.ID)
	sched.engine = observeFails{sched.engine}
	sched.Tick(ctx)
	if r := waitState(t, s, "eeeeeeee-0000-0000-0000-000000000006", StateFailed); r.Error != errBoom.Error() {
		t.Fatalf("error = %q", r.Error)
	}
}
