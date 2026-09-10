package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/pashagolub/pgxmock/v4"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func toFiles() []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120000_a.up.sql", Body: "CREATE TABLE a (id int);"},
		{Name: "20260901120000_a.down.sql", Body: "DROP TABLE a;"},
		{Name: "20260901120100_b.up.sql", Body: "CREATE TABLE b (id int);"},
		{Name: "20260901120100_b.down.sql", Body: "DROP TABLE b;"},
		{Name: "R__v.up.sql", Body: "CREATE OR REPLACE VIEW v AS SELECT 1;"},
		{Name: "R__v.down.sql", Body: "DROP VIEW v;"},
	}
}

func expectAppliedVersions(mock pgxmock.PgxPoolIface, versions ...int64) {
	rows := pgxmock.NewRows([]string{"version"})
	for _, v := range versions {
		rows.AddRow(v)
	}
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(rows)
	expectNoRepeatables(mock)
}

func TestPlanRunReportsWhatTheTargetWithheld(t *testing.T) {
	t.Parallel()
	s, mock := planServer(t, controlplane.Observation{}, nil)
	expectAppliedVersions(mock)
	expectApplied(mock)

	res, err := s.PlanRun(context.Background(), connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: toFiles(), ToVersion: 20260901120000,
	}))
	if err != nil {
		t.Fatal(err)
	}
	migs := res.Msg.Migrations
	if len(migs) != 3 || migs[0].Withheld || len(migs[0].Statements) == 0 {
		t.Fatalf("migrations = %+v", migs)
	}
	if !migs[1].Withheld || !migs[2].Withheld || len(migs[1].Statements) != 0 {
		t.Fatalf("withheld rows = %+v; they are reported, never run", migs[1:])
	}
}

func TestVersionTargetRefusalReachesBothEntryPoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := planServer(t, controlplane.Observation{}, nil)

	if _, err := s.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{
		Target: "app", Files: toFiles(), ToVersion: 1,
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("plan err = %v", err)
	}
	if _, err := s.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: toFiles(), ToVersion: 1,
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("create err = %v", err)
	}
}

func TestCreateRunRefusesAVersionTargetOnAStoredPlan(t *testing.T) {
	t.Parallel()
	s, _ := planServer(t, controlplane.Observation{}, nil)

	_, err := s.CreateRun(context.Background(), connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: toFiles(), PlanId: planID, ToVersion: 20260901120000,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "already fixes the set it covers") {
		t.Fatalf("err = %v", err)
	}
}
