package controlplane

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
)

// stubEngine answers the two calls adoption makes on a target and nothing else.
type stubEngine struct {
	Engine
	marked []engine.Migration
	obs    Observation
	err    error
}

func (e stubEngine) MarkApplied(context.Context, string, []engine.Migration) ([]engine.Migration, error) {
	return e.marked, e.err
}

func (e stubEngine) Observe(context.Context, string) (Observation, error) {
	return e.obs, e.err
}

func (e stubEngine) Snapshot(context.Context, string) (string, string, error) {
	return "", "", e.err
}

func mockScheduler(t *testing.T, eng Engine) (pgxmock.PgxPoolIface, *Scheduler) {
	t.Helper()
	mock, s := newMockStore(t)
	providers := map[string]creds.Provider{"plain": plainProvider{}}

	return mock, NewScheduler(s, providers, eng, Policies(), Config{Holder: "h"}, testLog)
}

func expectTargetRow(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery("SELECT provider, config FROM cp_targets").WithArgs("app").
		WillReturnRows(pgxmock.NewRows([]string{"provider", "config"}).AddRow("plain", []byte(`{"dsn":"postgres:///x"}`)))
}

func expectAppliedQuery(mock pgxmock.PgxPoolIface, versions ...int64) {
	rows := pgxmock.NewRows([]string{"version"})
	for _, v := range versions {
		rows.AddRow(v)
	}
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(rows)
	mock.ExpectQuery("SELECT DISTINCT ON \\(a.migration\\)").WithArgs("app").
		WillReturnRows(pgxmock.NewRows([]string{"migration", "body"}))
}

var oneMigration = []engine.Migration{{Version: 20260101000000, Name: "a", Checksum: "c1", UpSQL: "SELECT 1;", DownSQL: "SELECT 1;"}}

func TestBaselineStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, sched := mockScheduler(t, stubEngine{marked: oneMigration})
	b := NewBaseliner(sched)

	expectTargetRow(mock)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnError(errBoom)
	if err := b.Baseline(ctx, "r1", "app", oneMigration, Provenance{}); err == nil ||
		!strings.Contains(err.Error(), "list applied versions") {
		t.Fatalf("err = %v", err)
	}
}

func TestReconcileStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	obs := Observation{Applied: []engine.Applied{{Version: 20260101000000, Name: "a", Checksum: "c1"}}}

	mock, sched := mockScheduler(t, stubEngine{err: errBoom})
	expectTargetRow(mock)
	if _, err := NewReconciler(sched).Reconcile(ctx, "r1", "app", oneMigration, Provenance{}); err != errBoom {
		t.Fatalf("observe error = %v", err)
	}

	mock, sched = mockScheduler(t, stubEngine{obs: obs})
	r := NewReconciler(sched)
	expectTargetRow(mock)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnError(errBoom)
	if _, err := r.Reconcile(ctx, "r1", "app", oneMigration, Provenance{}); err == nil ||
		!strings.Contains(err.Error(), "list applied versions") {
		t.Fatalf("applied error = %v", err)
	}

	expectTargetRow(mock)
	expectAppliedQuery(mock)
	mock.ExpectExec("INSERT INTO cp_runs").WithArgs(anyArgs(8)...).WillReturnError(errBoom)
	if _, err := r.Reconcile(ctx, "r1", "app", oneMigration, Provenance{}); err == nil ||
		!strings.Contains(err.Error(), "create reconcile run") {
		t.Fatalf("adoption error = %v", err)
	}
}

func TestSchedulerAdoptStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, sched := mockScheduler(t, stubEngine{})

	// A revert skips a down plan for a migration the target never recorded: nothing to write either way.
	if err := sched.record(Run{ID: "r1", Reverts: "r0"}, nil, AppliedSet{})(ctx, engine.Result{Skipped: true}); err != nil {
		t.Fatalf("skipped down plan = %v", err)
	}

	plans, err := buildPlans(oneMigration, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("INSERT INTO cp_run_applied").WithArgs("r1", "20260101000000_a").WillReturnError(errBoom)
	res := engine.Result{Migration: "20260101000000_a", Skipped: true, Recorded: true}
	if err := sched.record(Run{ID: "r1", Target: "app"}, plans, AppliedSet{})(ctx, res); err == nil ||
		!strings.Contains(err.Error(), "adopt applied") {
		t.Fatalf("adopt error = %v", err)
	}

	mock.ExpectQuery("SELECT name, body FROM cp_run_files").WithArgs("r1").
		WillReturnRows(pgxmock.NewRows([]string{"name", "body"}).
			AddRow("20260101000000_a.up.sql", "SELECT 1;").
			AddRow("20260101000000_a.down.sql", "SELECT 1;"))
	expectTargetRow(mock)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnError(errBoom)
	if _, err := sched.applyRun(ctx, Run{ID: "r1", Target: "app", Rollout: RolloutDirect}); err == nil ||
		!strings.Contains(err.Error(), "list applied versions") {
		t.Fatalf("applyRun = %v", err)
	}
}

// Repeatables are named, not numbered, so they diverge by content rather than by absence.
func TestDivergedOnRepeatables(t *testing.T) {
	t.Parallel()
	obs := Observation{Repeatables: []engine.Repeatable{{Name: "stats", Checksum: "new"}}}
	ledger := AppliedSet{Repeatables: map[string]string{"stats": "old"}}

	if got := Unreconciled(obs, ledger); !slices.Equal(got, []string{"R__stats"}) {
		t.Fatalf("unreconciled = %v", got)
	}
	migs := []engine.Migration{{Repeatable: true, Name: "stats", Checksum: "new", UpSQL: "SELECT 1;"}}
	d := Diverged(obs, ledger, migs)
	if !slices.Equal(d.Adopt, []string{"R__stats"}) || !slices.Equal(d.Withdrawn, []string{"R__stats"}) {
		t.Fatalf("divergence = %+v", d)
	}
	if err := d.Err("app"); err == nil || !strings.Contains(err.Error(), "absent from the target: R__stats") {
		t.Fatalf("err = %v", err)
	}
}
