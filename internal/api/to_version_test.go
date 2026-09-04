package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/pashagolub/pgxmock/v4"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
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

func toSpec(t *testing.T, files []*godwitv1.MigrationFile) runSpec {
	t.Helper()
	spec, err := (&Server{}).upSpec("app", "", files)
	if err != nil {
		t.Fatal(err)
	}

	return spec
}

func expectAppliedVersions(mock pgxmock.PgxPoolIface, versions ...int64) {
	rows := pgxmock.NewRows([]string{"version"})
	for _, v := range versions {
		rows.AddRow(v)
	}
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnRows(rows)
	expectNoRepeatables(mock)
}

func TestStopAtUnsetKeepsTheWholeSet(t *testing.T) {
	t.Parallel()
	s, _ := planServer(t, controlplane.Observation{}, nil)
	spec := toSpec(t, toFiles())

	out, err := s.stopAt(context.Background(), "app", spec, 0)
	if err != nil || len(out.plans) != 3 || len(out.withheld) != 0 || len(out.files) != 6 {
		t.Fatalf("spec = %+v, err = %v", out, err)
	}
}

func TestStopAtRefusesAVersionTheSetDoesNotHold(t *testing.T) {
	t.Parallel()
	s, _ := planServer(t, controlplane.Observation{}, nil)

	_, err := s.stopAt(context.Background(), "app", toSpec(t, toFiles()), 20260901119999)
	if connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "20260901120000, 20260901120100") {
		t.Fatalf("err = %v", err)
	}

	only := []*godwitv1.MigrationFile{
		{Name: "R__v.up.sql", Body: "CREATE OR REPLACE VIEW v AS SELECT 1;"},
		{Name: "R__v.down.sql", Body: "DROP VIEW v;"},
	}
	if _, err := s.stopAt(context.Background(), "app", toSpec(t, only), 1); !strings.Contains(err.Error(), "repeatable migrations only") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopAtAppliedError(t *testing.T) {
	t.Parallel()
	s, mock := planServer(t, controlplane.Observation{}, nil)
	mock.ExpectQuery("SELECT DISTINCT left").WithArgs("app").WillReturnError(errors.New("boom"))

	if _, err := s.stopAt(context.Background(), "app", toSpec(t, toFiles()), 20260901120000); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("err = %v", err)
	}
}

func TestStopAtRefusesATargetBehindHistory(t *testing.T) {
	t.Parallel()
	s, mock := planServer(t, controlplane.Observation{}, nil)
	expectAppliedVersions(mock, 20260901120000, 20260901120100)

	_, err := s.stopAt(context.Background(), "app", toSpec(t, toFiles()), 20260901120000)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "it never reverts") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopAtRefusesATargetThatSelectsNothing(t *testing.T) {
	t.Parallel()
	files := append(toFiles(),
		&godwitv1.MigrationFile{Name: "20260901120200_c.up.sql", Body: "CREATE TABLE c (id int);"},
		&godwitv1.MigrationFile{Name: "20260901120200_c.down.sql", Body: "DROP TABLE c;"})
	s, mock := planServer(t, controlplane.Observation{}, nil)
	expectAppliedVersions(mock, 20260901120000, 20260901120100)

	_, err := s.stopAt(context.Background(), "app", toSpec(t, files), 20260901120100)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "the pending set starts at 20260901120200_c") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopAtKeepsWhatIsAtOrBelowTheTarget(t *testing.T) {
	t.Parallel()
	s, mock := planServer(t, controlplane.Observation{}, nil)
	expectAppliedVersions(mock)

	spec, err := s.stopAt(context.Background(), "app", toSpec(t, toFiles()), 20260901120000)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.plans) != 1 || spec.plans[0].Migration.Version != 20260901120000 {
		t.Fatalf("plans = %+v", spec.plans)
	}
	if got := ids(spec.withheld); len(got) != 2 || got[0] != "20260901120100_b" || got[1] != "R__v" {
		t.Fatalf("withheld = %v", got)
	}
	if len(spec.files) != 2 || spec.files["20260901120000_a.up.sql"] == "" || spec.files["20260901120000_a.down.sql"] == "" {
		t.Fatalf("files = %v; the run carries only what it applies", spec.files)
	}
}

func ids(plans []engine.Plan) []string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Migration.ID())
	}

	return out
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
