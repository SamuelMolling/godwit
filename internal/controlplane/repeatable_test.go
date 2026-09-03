package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/engine"
)

const (
	viewV1 = "CREATE OR REPLACE VIEW stats AS SELECT 1 AS n;"
	viewV2 = "CREATE OR REPLACE VIEW stats AS SELECT 2 AS n;"
)

func rep(name, up string) engine.Migration {
	m := engine.Migration{Name: name, Repeatable: true, UpSQL: up, DownSQL: "DROP VIEW IF EXISTS " + name + ";"}
	m.Checksum = sum(up)

	return m
}

func recordedRep(m engine.Migration, at time.Time) engine.Repeatable {
	return engine.Repeatable{Name: m.Name, Checksum: m.Checksum, AppliedAt: at}
}

func repFiles(name, up string) map[string]string {
	m := rep(name, up)

	return map[string]string{m.UpFile(): m.UpSQL, m.DownFile(): m.DownSQL}
}

func TestPendingRepeatable(t *testing.T) {
	t.Parallel()
	a := mig(1, "a", "SELECT 1;")
	stats := rep("stats", viewV1)
	now := time.Now()

	pending, err := Pending([]engine.Migration{stats, a}, nil, nil)
	if err != nil || len(pending) != 2 || pending[0].Version != 1 || !pending[1].Repeatable {
		t.Fatalf("never recorded = %+v, err = %v", pending, err)
	}
	pending, err = Pending([]engine.Migration{stats}, []engine.Applied{applied(a, now)}, []engine.Repeatable{recordedRep(stats, now)})
	if err != nil || len(pending) != 0 {
		t.Fatalf("unchanged = %+v, err = %v", pending, err)
	}
	pending, err = Pending([]engine.Migration{rep("stats", viewV2)}, nil, []engine.Repeatable{recordedRep(stats, now)})
	if err != nil || len(pending) != 1 {
		t.Fatalf("edited = %+v, err = %v", pending, err)
	}
}

func TestPlanKeyChangesWithRepeatableContent(t *testing.T) {
	t.Parallel()
	one := PlanKey("app", RolloutDirect, []engine.Migration{rep("stats", viewV1)})
	if one == PlanKey("app", RolloutDirect, []engine.Migration{rep("stats", viewV2)}) {
		t.Fatal("editing a repeatable must change the plan key")
	}
	if one != PlanKey("app", RolloutDirect, []engine.Migration{rep("stats", viewV1)}) {
		t.Fatal("same content must keep the plan key")
	}
	if one == PlanKey("app", RolloutDirect, nil) {
		t.Fatal("a pending repeatable must be part of the key")
	}
}

func TestHistoryHashCoversRepeatables(t *testing.T) {
	t.Parallel()
	now := time.Now()
	a, b := rep("a", viewV1), rep("b", viewV1)
	one := HistoryHash(nil, []engine.Repeatable{recordedRep(a, now), recordedRep(b, now)})
	if one != HistoryHash(nil, []engine.Repeatable{recordedRep(b, now), recordedRep(a, now)}) {
		t.Fatal("hash must not depend on order")
	}
	if one == HistoryHash(nil, []engine.Repeatable{recordedRep(rep("a", viewV2), now), recordedRep(b, now)}) {
		t.Fatal("recorded content must change the hash")
	}
	if one == HistoryHash(nil, nil) {
		t.Fatal("recorded repeatables must change the hash")
	}
}

func TestBuildPlanMigrationsRepeatable(t *testing.T) {
	t.Parallel()
	stats := rep("stats", viewV1)
	plans, err := buildPlans([]engine.Migration{mig(1, "a", "SELECT 1;"), stats}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}

	ms := BuildPlanMigrations(RolloutDirect, plans, AppliedSet{Versions: []int64{1}})
	if len(ms) != 2 || !ms[1].Repeatable || ms[1].ID() != "R__stats" || ms[1].Applied {
		t.Fatalf("migrations = %+v", ms)
	}
	if len(Plan{Migrations: ms}.Pending()) != 1 {
		t.Fatal("a repeatable the target has not recorded is pending")
	}

	ms = BuildPlanMigrations(RolloutDirect, plans, AppliedSet{Repeatables: map[string]string{"stats": stats.Checksum}})
	if !ms[1].Applied || ms[0].Applied {
		t.Fatalf("migrations = %+v", ms)
	}
	if len(Plan{Migrations: ms}.Pending()) != 1 {
		t.Fatal("an unchanged repeatable is not pending")
	}
}

func TestStaleDiffRepeatable(t *testing.T) {
	t.Parallel()
	now := time.Now()
	stats := rep("stats", viewV1)
	plan := Plan{Repeatables: []engine.Repeatable{recordedRep(stats, now)}, SchemaFingerprint: "f"}

	same := StaleDiff(plan, Observation{Repeatables: plan.Repeatables, Fingerprint: "f"})
	if len(same.Added) != 0 || len(same.Removed) != 0 || !same.Explained("f", "f") {
		t.Fatalf("unchanged = %+v", same)
	}

	edited := StaleDiff(plan, Observation{
		Repeatables: []engine.Repeatable{recordedRep(rep("stats", viewV2), now)}, Fingerprint: "f",
	})
	if len(edited.Added) != 1 || edited.Added[0].String() != "R__stats" || len(edited.Removed) != 1 {
		t.Fatalf("edited = %+v", edited)
	}
	if edited.Explained("f", "f") || edited.Reason() != StaleHistory {
		t.Fatal("a repeatable re-recorded by nothing must refuse the bind")
	}
	edited.Added[0].RunID = "r1"
	if !edited.Explained("f", "f") || edited.Reason() != StaleSchema {
		t.Fatal("a repeatable re-recorded by a run is explained")
	}

	gone := StaleDiff(plan, Observation{Fingerprint: "f"})
	if len(gone.Removed) != 1 || gone.Explained("f", "f") || gone.Reason() != StaleHistory {
		t.Fatalf("removed = %+v", gone)
	}
}

func TestStaleDiffOrdersRepeatablesLast(t *testing.T) {
	t.Parallel()
	now := time.Now()
	obs := Observation{
		Applied:     []engine.Applied{applied(mig(2, "b", "SELECT 2;"), now), applied(mig(1, "a", "SELECT 1;"), now)},
		Repeatables: []engine.Repeatable{recordedRep(rep("z", viewV1), now), recordedRep(rep("a", viewV1), now)},
	}
	d := StaleDiff(Plan{}, obs)
	var ids []string
	for _, c := range d.Added {
		ids = append(ids, c.String())
	}
	want := "00000000000001_a,00000000000002_b,R__a,R__z"
	if strings.Join(ids, ",") != want {
		t.Fatalf("added = %v, want %s", ids, want)
	}
}

func TestRecordedOn(t *testing.T) {
	t.Parallel()
	now := time.Now()
	stats := rep("stats", viewV1)
	obs := Observation{
		Applied:     []engine.Applied{applied(mig(1, "a", "SELECT 1;"), now)},
		Repeatables: []engine.Repeatable{recordedRep(stats, now)},
	}
	if !recordedOn(obs, mig(1, "a", "SELECT 1;")) || recordedOn(obs, mig(2, "b", "SELECT 2;")) {
		t.Fatal("versioned lookup")
	}
	if !recordedOn(obs, stats) || recordedOn(obs, rep("stats", viewV2)) {
		t.Fatal("repeatable lookup must compare content")
	}
}

// A repeatable is CREATE OR REPLACE-shaped: scratch replay must apply it in the same order as the run,
// or the fingerprints admission compares would not match the target.
func TestValidateReplaysRepeatables(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, pool := newStore(t)
	sched, dsn := newScheduler(t, s, Config{Holder: "h"})

	files := goodFiles()
	for name, body := range repFiles("stats", viewV1) {
		files[name] = body
	}
	queueRun(t, s, "eeeeeeee-0000-0000-0000-000000000001", files)
	sched.Tick(ctx)
	waitState(t, s, "eeeeeeee-0000-0000-0000-000000000001", StateSucceeded)

	live, err := PGEngine{}.Observe(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Repeatables) != 1 || live.Repeatables[0].Name != "stats" {
		t.Fatalf("target repeatables = %+v", live.Repeatables)
	}

	next, err := buildPlans([]engine.Migration{rep("stats", viewV2)}, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	val, err := NewValidator(pool, s, func() string { return "reps" }).Validate(ctx, "app", next, live.SearchPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(val.Base, ".stats ") {
		t.Fatalf("history replay must create the view: %q", val.Base)
	}
	if len(val.Effects) != 1 || len(val.Effects[0]) != 2 {
		t.Fatalf("effects = %v", val.Effects)
	}
	if val.Fingerprints[0] != live.Fingerprint {
		t.Fatalf("replay diff = %v", engine.DiffSchemas(val.Base, live.Definition))
	}
}

func TestSchedulerAppliesRepeatableAfterVersions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	sched, dsn := newScheduler(t, s, Config{Holder: "h"})

	files := repFiles("stats", viewV1)
	files["20260901120000_t.up.sql"] = "CREATE TABLE t (id int);"
	files["20260901120000_t.down.sql"] = "DROP TABLE t;"
	queueRun(t, s, "eeeeeeee-0000-0000-0000-000000000011", files)
	sched.Tick(ctx)
	waitState(t, s, "eeeeeeee-0000-0000-0000-000000000011", StateSucceeded)

	applied, reps, err := PGEngine{}.Applied(ctx, dsn)
	if err != nil || len(applied) != 1 || len(reps) != 1 || reps[0].Name != "stats" {
		t.Fatalf("applied = %v, reps = %v, err = %v", applied, reps, err)
	}
	set, err := s.Applied(ctx, "app")
	if err != nil || len(set.Versions) != 1 || set.Repeatables["stats"] != sum(viewV1) {
		t.Fatalf("store applied = %+v, err = %v", set, err)
	}
}

func TestListAppliedErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock, err := pgxmock.NewConn()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT to_regclass").WithArgs("godwit.migrations").WillReturnError(errBoom)
	if _, _, err := listApplied(ctx, mock); err == nil || !strings.Contains(err.Error(), "probe godwit schema") {
		t.Fatalf("migrations err = %v", err)
	}

	mock.ExpectQuery("SELECT to_regclass").WithArgs("godwit.migrations").
		WillReturnRows(pgxmock.NewRows([]string{"present"}).AddRow(false))
	mock.ExpectQuery("SELECT to_regclass").WithArgs("godwit.repeatables").WillReturnError(errBoom)
	if _, _, err := listApplied(ctx, mock); err == nil || !strings.Contains(err.Error(), "probe godwit schema") {
		t.Fatalf("repeatables err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAppliedRepeatablesQueryError(t *testing.T) {
	t.Parallel()
	mock, s := newMockStore(t)

	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(pgxmock.NewRows([]string{"version"}))
	mock.ExpectQuery("SELECT DISTINCT ON").WithArgs("app").WillReturnError(errBoom)
	if _, err := s.Applied(context.Background(), "app"); err == nil ||
		!strings.Contains(err.Error(), "list applied repeatables") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCompareChangesOrders(t *testing.T) {
	t.Parallel()

	versioned := HistoryChange{Version: 1}
	repeated := HistoryChange{Name: "a", Repeatable: true}
	if compareChanges(repeated, versioned) <= 0 || compareChanges(versioned, repeated) >= 0 {
		t.Fatal("versioned changes come first")
	}
	if compareChanges(versioned, HistoryChange{Version: 2}) >= 0 {
		t.Fatal("versioned changes sort by version")
	}
}
