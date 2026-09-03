package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

const (
	statsV1 = "CREATE OR REPLACE VIEW stats AS SELECT 1 AS n;"
	statsV2 = "CREATE OR REPLACE VIEW stats AS SELECT 2 AS n;"
)

func repeatableFiles(up string) []*godwitv1.MigrationFile {
	return []*godwitv1.MigrationFile{
		{Name: "20260901120001_t.up.sql", Body: "CREATE TABLE t (id int);"},
		{Name: "20260901120001_t.down.sql", Body: "DROP TABLE t;"},
		{Name: "R__stats.up.sql", Body: up},
		{Name: "R__stats.down.sql", Body: "DROP VIEW IF EXISTS stats;"},
	}
}

func targetStatus(t *testing.T, client godwitv1connect.GodwitServiceClient, files []*godwitv1.MigrationFile) *godwitv1.GetTargetStatusResponse {
	t.Helper()
	res, err := client.GetTargetStatus(context.Background(), connect.NewRequest(&godwitv1.GetTargetStatusRequest{
		Target: "app", Files: files,
	}))
	if err != nil {
		t.Fatal(err)
	}

	return res.Msg
}

func TestRepeatableRunAndStatus(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))

	first := repeatableFiles(statsV1)
	st := targetStatus(t, client, first)
	if len(st.Pending) != 2 || !st.Pending[1].Repeatable || st.Pending[1].Name != "stats" || st.Pending[1].Version != 0 {
		t.Fatalf("pending before the run = %+v", st.Pending)
	}

	runToSuccess(t, client, first, nil)
	st = targetStatus(t, client, first)
	if len(st.Pending) != 0 {
		t.Fatalf("an unchanged repeatable must not be pending: %+v", st.Pending)
	}
	var recorded *godwitv1.AppliedMigration
	for _, a := range st.Applied {
		if a.Repeatable {
			recorded = a
		}
	}
	if recorded == nil || recorded.Name != "stats" || recorded.Version != 0 || recorded.Checksum == "" ||
		recorded.AppliedAt == nil || recorded.ChecksumMismatch {
		t.Fatalf("applied = %+v", st.Applied)
	}

	edited := repeatableFiles(statsV2)
	st = targetStatus(t, client, edited)
	if len(st.Pending) != 1 || !st.Pending[0].Repeatable {
		t.Fatalf("an edited repeatable must be pending: %+v", st.Pending)
	}
	runToSuccess(t, client, edited, nil)
	after := targetStatus(t, client, edited)
	if len(after.Pending) != 0 {
		t.Fatalf("pending after re-apply = %+v", after.Pending)
	}
	for _, a := range after.Applied {
		if a.Repeatable && a.Checksum == recorded.Checksum {
			t.Fatal("re-applying must record the new content")
		}
	}
}

// Editing a repeatable changes the pending set, so a plan taken before the edit no longer covers it.
func TestRepeatablePlanGoesStaleAfterEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: newDatabase(t, "tg"), RequirePlan: true,
	})); err != nil {
		t.Fatal(err)
	}

	first := repeatableFiles(statsV1)
	plan := persistPlan(t, client, first, nil)
	if len(plan.Migrations) != 2 || !plan.Migrations[1].Repeatable {
		t.Fatalf("plan = %+v", plan.Migrations)
	}

	_, err := createRun(t, client, repeatableFiles(statsV2), false)
	_, required := planDetail(t, err)
	if required == nil || required.Key == plan.PlanKey {
		t.Fatalf("required = %+v, err = %v", required, err)
	}
	if !strings.Contains(required.FilesDiff, "R__stats (up checksum differs)") {
		t.Fatalf("files diff = %q", required.FilesDiff)
	}
}

// A repeatable re-recorded on the target by something other than a run refuses the bind.
func TestRepeatablePlanGoesStaleWhenTargetMoves(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	targetDSN := newDatabase(t, "tg")
	registerTarget(t, client, targetDSN)

	runToSuccess(t, client, repeatableFiles(statsV1), nil)
	edited := repeatableFiles(statsV2)
	plan := persistPlan(t, client, edited, nil)

	execStore(t, targetDSN, "UPDATE godwit.repeatables SET checksum = 'by-hand' WHERE name = 'stats'")

	_, err := createRun(t, client, edited, false)
	stale, _ := planDetail(t, err)
	if stale == nil || stale.PlanId != plan.PlanId || stale.Reason != controlplane.StaleHistory {
		t.Fatalf("stale = %+v, err = %v", stale, err)
	}
	if !strings.Contains(strings.Join(stale.HistoryAdded, ","), "R__stats") ||
		!strings.Contains(strings.Join(stale.HistoryRemoved, ","), "R__stats") {
		t.Fatalf("history = %v / %v", stale.HistoryAdded, stale.HistoryRemoved)
	}
}

func TestRepeatableIsNeverOutOfOrder(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	registerTarget(t, client, newDatabase(t, "tg"))

	all := orderedFiles()
	runToSuccess(t, client, all, nil)
	runToSuccess(t, client, append(all[:0:0], repeatableFiles(statsV1)[2:]...), nil)
}
