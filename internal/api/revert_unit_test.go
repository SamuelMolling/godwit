package api

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/pashagolub/pgxmock/v4"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

// TestRevertRunWithoutInspector plans a revert on a server that cannot reach its targets: no schema to
// probe, so no data-loss verdict and no observed search path.
func TestRevertRunWithoutInspector(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)

	mock.ExpectQuery("FROM cp_runs WHERE id").WithArgs("r1").WillReturnRows(runRow())
	mock.ExpectQuery("FROM cp_run_applied").WithArgs("r1").WillReturnRows(
		pgxmock.NewRows([]string{"migration", "applied_at", "coalesce", "held", "expansion"}).
			AddRow("20260101000000_a", time.Now(), "", false, (*controlplane.Expansion)(nil)))
	mock.ExpectQuery("FROM cp_run_files").WithArgs("r1").WillReturnRows(
		pgxmock.NewRows([]string{"name", "body"}).
			AddRow("20260101000000_a.up.sql", "CREATE TABLE a (id int);").
			AddRow("20260101000000_a.down.sql", "DROP TABLE a;"))
	mock.ExpectQuery("SELECT EXISTS").WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	expectApplied(mock)

	res, err := s.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{
		RunId: "r1", AcknowledgeHazards: []string{"H002"}, DryRun: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg; got.RunId != "" || got.Reverts != "r1" || len(got.Migrations) != 1 || len(got.DataLoss) != 0 || got.Forced {
		t.Fatalf("plan = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
