package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func directivePlan(t *testing.T, dir Direction) Plan {
	t.Helper()
	m := Migration{
		Version: 1, Name: "m", Checksum: "c",
		UpSQL: "-- godwit: change-type t.a bigint\n", DownSQL: "-- godwit: revert\n",
	}
	if err := m.loadDirectives(); err != nil {
		t.Fatal(err)
	}

	return buildPlanT(t, m, dir)
}

func TestApplyRefusesUnexpandedDirectives(t *testing.T) {
	t.Parallel()
	_, exec := newMockExec(t)
	for _, dir := range []Direction{DirectionUp, DirectionDown} {
		p := directivePlan(t, dir)
		if len(p.Statements) != 0 {
			t.Fatalf("%s: a directive body has no statements of its own", dir)
		}
		var err error
		if dir == DirectionUp {
			_, err = exec.Up(context.Background(), p)
		} else {
			_, err = exec.Down(context.Background(), p)
		}
		if err == nil || !strings.Contains(err.Error(), "never expanded") {
			t.Fatalf("%s err = %v", dir, err)
		}
	}
}

func downPlan(t *testing.T) Plan {
	t.Helper()

	return buildPlanT(t, Migration{Version: 1, Name: "m", Checksum: "c", UpSQL: "SELECT 1;", DownSQL: "SELECT 2;"}, DirectionDown)
}

func expectHeldLookup(mock pgxmock.PgxConnIface) *pgxmock.ExpectedQuery {
	return mock.ExpectQuery("SELECT id FROM godwit.runs\\s+WHERE direction = 'up'").
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg())
}

func TestDownHeldRunErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, exec := newMockExec(t)
	expectLock(mock)
	expectBootstrap(mock)
	expectNotApplied(mock)
	expectHeldLookup(mock).WillReturnError(errBoom)
	if _, err := exec.Down(ctx, downPlan(t)); err == nil || !strings.Contains(err.Error(), "find held run for") {
		t.Fatalf("err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// heldDown drives a down over a migration with no history row but an unfinished up run, which is the
// state a run parked between its phases leaves behind.
func heldDown(t *testing.T, mock pgxmock.PgxConnIface) {
	t.Helper()
	expectLock(mock)
	expectBootstrap(mock)
	expectNotApplied(mock)
	expectHeldLookup(mock).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("held-1"))
	mock.ExpectQuery("SELECT id FROM godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec("INSERT INTO godwit.runs").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectBegin()
	mock.ExpectExec("SET LOCAL lock_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
	mock.ExpectExec("SET LOCAL statement_timeout").WillReturnResult(pgxmock.NewResult("SET", 0))
	mock.ExpectExec("SELECT 2").WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectExec("INSERT INTO godwit.journal").WithArgs(pgxmock.AnyArg(), 0, pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM godwit.migrations").WithArgs(pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("DELETE", 0))
}

func TestDownDiscardsHeldRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	mock, exec := newMockExec(t)
	heldDown(t, mock)
	mock.ExpectExec("DELETE FROM godwit.journal").WithArgs("held-1").WillReturnError(errBoom)
	mock.ExpectRollback()
	if _, err := exec.Down(ctx, downPlan(t)); err == nil || !strings.Contains(err.Error(), "discard held journal") {
		t.Fatalf("journal err = %v", err)
	}

	mock2, exec2 := newMockExec(t)
	heldDown(t, mock2)
	mock2.ExpectExec("DELETE FROM godwit.journal").WithArgs("held-1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock2.ExpectExec("DELETE FROM godwit.runs").WithArgs("held-1").WillReturnError(errBoom)
	mock2.ExpectRollback()
	if _, err := exec2.Down(ctx, downPlan(t)); err == nil || !strings.Contains(err.Error(), "discard held run") {
		t.Fatalf("run err = %v", err)
	}
}
