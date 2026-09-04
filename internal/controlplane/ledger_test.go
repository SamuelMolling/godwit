package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// noExpansion is the NULL a ledger row carries when its migration had no directives.
var noExpansion *Expansion

func appliedRows() *pgxmock.Rows {
	return pgxmock.NewRows([]string{"migration", "applied_at", "coalesce", "held", "adopted", "expansion"})
}

func expectNoCheckpoints(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery("ORDER BY a.migration DESC").WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows([]string{"migration", "body"}))
}

func TestLedgerStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, s := newMockStore(t)

	mock.ExpectExec("INSERT INTO cp_run_applied").WithArgs(anyArgs(4)...).WillReturnError(errBoom)
	if err := s.RecordApplied(ctx, "r1", "m", false, nil); err == nil || !strings.Contains(err.Error(), "record applied m") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectExec("UPDATE cp_run_applied SET reverted_by").WithArgs(anyArgs(3)...).WillReturnError(errBoom)
	if err := s.MarkReverted(ctx, "r1", "r2", "m"); err == nil || !strings.Contains(err.Error(), "mark reverted m") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("FROM cp_run_applied").WithArgs("r1").WillReturnError(errBoom)
	if _, err := s.AppliedMigrations(ctx, "r1"); err == nil || !strings.Contains(err.Error(), "list applied migrations") {
		t.Fatalf("err = %v", err)
	}
	mock.ExpectQuery("FROM cp_run_applied").WithArgs("r1").WillReturnRows(
		appliedRows().AddRow("m", time.Now(), "", false, false, noExpansion).RowError(0, errBoom))
	if _, err := s.AppliedMigrations(ctx, "r1"); err == nil || !strings.Contains(err.Error(), "read applied migrations") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectRunRow(mock pgxmock.PgxPoolIface) {
	const id, state = "r1", StateSucceeded
	mock.ExpectQuery("FROM cp_runs WHERE id").WithArgs(id).WillReturnRows(pgxmock.NewRows([]string{
		"id", "target", "state", "coalesce", "attempts", "rollout", "phase", "coalesce", "kind", "coalesce", "coalesce",
		"created_at", "finished_at", "created_by", "source", "coalesce", "retries", "not_before", "progress", "expansions",
	}).AddRow(id, "app", state, "", 1, RolloutDirect, PhaseExpand, "", KindMigrate, "", "",
		time.Now(), nil, "ci", "", "", 0, nil, nil, map[string]Expansion{}))
}

func TestPlanRevertErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, s := newMockStore(t)

	mock.ExpectQuery("FROM cp_runs WHERE id").WithArgs("r1").WillReturnError(errBoom)
	if _, err := s.PlanRevert(ctx, "r1"); err == nil || !strings.Contains(err.Error(), "load run") {
		t.Fatalf("run error = %v", err)
	}

	expectRunRow(mock)
	mock.ExpectQuery("FROM cp_run_applied").WithArgs("r1").WillReturnError(errBoom)
	if _, err := s.PlanRevert(ctx, "r1"); err == nil || !strings.Contains(err.Error(), "list applied migrations") {
		t.Fatalf("ledger error = %v", err)
	}

	expectRunRow(mock)
	mock.ExpectQuery("FROM cp_run_applied").WithArgs("r1").WillReturnRows(
		appliedRows().AddRow("m", time.Now(), "r9", false, false, noExpansion))
	if _, err := s.PlanRevert(ctx, "r1"); !errors.Is(err, ErrNotRevertable) ||
		!strings.Contains(err.Error(), "applied no migration that still stands") {
		t.Fatalf("already reverted = %v", err)
	}

	expectRunRow(mock)
	mock.ExpectQuery("FROM cp_run_applied").WithArgs("r1").WillReturnRows(
		appliedRows().AddRow("20260101000000_a", time.Now(), "", false, false, noExpansion))
	expectNoCheckpoints(mock)
	mock.ExpectQuery("FROM cp_run_files").WithArgs("r1").WillReturnError(errBoom)
	if _, err := s.PlanRevert(ctx, "r1"); err == nil || !strings.Contains(err.Error(), "list run files") {
		t.Fatalf("files error = %v", err)
	}

	expectRunRow(mock)
	mock.ExpectQuery("FROM cp_run_applied").WithArgs("r1").WillReturnRows(
		appliedRows().AddRow("20260101000000_a", time.Now(), "", false, false, noExpansion))
	expectNoCheckpoints(mock)
	mock.ExpectQuery("FROM cp_run_files").WithArgs("r1").WillReturnRows(
		pgxmock.NewRows([]string{"name", "body"}).AddRow("20260101000000_a.up.sql", "SELECT 1;"))
	if _, err := s.PlanRevert(ctx, "r1"); !errors.Is(err, ErrRevertPlan) ||
		!strings.Contains(err.Error(), "did not carry 20260101000000_a.down.sql") {
		t.Fatalf("missing down file = %v", err)
	}

	expectRunRow(mock)
	mock.ExpectQuery("FROM cp_run_applied").WithArgs("r1").WillReturnRows(
		appliedRows().AddRow("20260101000000_a", time.Now(), "", false, false, noExpansion))
	expectNoCheckpoints(mock)
	mock.ExpectQuery("FROM cp_run_files").WithArgs("r1").WillReturnRows(
		pgxmock.NewRows([]string{"name", "body"}).
			AddRow("20260101000000_a.up.sql", "SELECT 1;").
			AddRow("20260101000000_a.down.sql", "SELECT 1;"))
	mock.ExpectQuery("SELECT EXISTS").WithArgs(anyArgs(3)...).WillReturnError(errBoom)
	if _, err := s.PlanRevert(ctx, "r1"); err == nil || !strings.Contains(err.Error(), "check newer runs") {
		t.Fatalf("newer error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRevertGateStoreErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, s := newMockStore(t)
	orig := Run{ID: "r1", Target: "app", State: StateSucceeded, Kind: KindMigrate}

	mock.ExpectQuery("SELECT EXISTS").WithArgs(anyArgs(3)...).WillReturnError(errBoom)
	if err := s.CreateRevert(ctx, "r2", orig, false, Timeouts{}, Provenance{}); err == nil ||
		!strings.Contains(err.Error(), "check revertable") {
		t.Fatalf("gate error = %v", err)
	}
	mock.ExpectQuery("SELECT EXISTS").WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows([]string{"exists", "id"}).AddRow(false, nil))
	mock.ExpectExec("INSERT INTO cp_runs").WithArgs(anyArgs(7)...).WillReturnError(errBoom)
	if err := s.CreateRevert(ctx, "r2", orig, false, Timeouts{}, Provenance{}); err == nil ||
		!strings.Contains(err.Error(), "create revert") {
		t.Fatalf("insert error = %v", err)
	}
	mock.ExpectQuery("SELECT EXISTS").WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows([]string{"exists", "id"}).AddRow(false, nil))
	mock.ExpectExec("INSERT INTO cp_runs").WithArgs(anyArgs(7)...).WillReturnResult(pgxmock.NewResult("INSERT", 0))
	if err := s.CreateRevert(ctx, "r2", orig, false, Timeouts{}, Provenance{}); !errors.Is(err, ErrNotRevertable) {
		t.Fatalf("state moved under us = %v", err)
	}

	if err := s.CreateRevert(ctx, "r2", Run{ID: "b1", Kind: KindBaseline}, false, Timeouts{}, Provenance{}); !errors.Is(err, ErrBaselineRun) {
		t.Fatalf("baseline run = %v", err)
	}

	mock.ExpectQuery("FROM cp_runs r").WithArgs("app").WillReturnError(errBoom)
	if _, err := s.RevertTarget(ctx, "app"); err == nil || !strings.Contains(err.Error(), "load revertable run") {
		t.Fatalf("revert target = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPlanRevertReadsTheLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.RegisterTarget(ctx, "app", "static", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	id := "55555555-0000-0000-0000-000000000001"
	queueRun(t, s, id, map[string]string{
		"20260101000000_a.up.sql": "CREATE TABLE a (id int);", "20260101000000_a.down.sql": "DROP TABLE a;",
		"20260101000001_b.up.sql": "CREATE TABLE b (id int);", "20260101000001_b.down.sql": "DROP TABLE b;",
	})
	ledger(t, s, id, "20260101000000_a", "20260101000001_b")

	rp, err := s.PlanRevert(ctx, id)
	if err != nil || len(rp.Plans) != 2 {
		t.Fatalf("plan = %+v, err = %v", rp, err)
	}
	if rp.Plans[0].Migration.Name != "b" || rp.Plans[1].Migration.Name != "a" {
		t.Fatalf("revert order = %s then %s", rp.Plans[0].Migration.Name, rp.Plans[1].Migration.Name)
	}
	drops := rp.Drops()
	if len(drops) != 2 || drops["20260101000001_b"][0] != (engine.Drop{Table: "b"}) {
		t.Fatalf("drops = %+v", drops)
	}
}
