package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func TestAuditEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	baseURL := startService(t, storeDSN, "r1", []string{"ci:ci-secret", "samuel:ops-secret", "legacy-secret"})
	ci, samuel, legacy := newClient(baseURL, "ci-secret"), newClient(baseURL, "ops-secret"), newClient(baseURL, "legacy-secret")

	registerTarget(t, samuel, newDatabase(t, "tg"))
	if _, err := ci.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "legacy", Provider: "static", Dsn: newDatabase(t, "lg"),
	})); err != nil {
		t.Fatal(err)
	}

	source := "github.com/org/repo@abc123:db/migrations"
	created, err := ci.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: migrationFiles(), Source: source}))
	if err != nil {
		t.Fatal(err)
	}
	run := watchToEnd(t, ci, created.Msg.RunId)
	if run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED || run.CreatedBy != "ci" || run.Source != source {
		t.Fatalf("run = %+v", run)
	}

	reverted, err := legacy.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: run.Id, AcknowledgeHazards: []string{"H002"}}))
	if err != nil {
		t.Fatal(err)
	}
	if rv := watchToEnd(t, legacy, reverted.Msg.RunId); rv.CreatedBy != "anonymous" || rv.Source != "" {
		t.Fatalf("revert = %+v", rv)
	}
	baselined, err := samuel.BaselineTarget(ctx, connect.NewRequest(&godwitv1.BaselineTargetRequest{
		Target: "legacy", Files: baselineFiles(), Version: 1,
	}))
	if err != nil {
		t.Fatal(err)
	}
	list, err := samuel.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{Target: "legacy"}))
	if err != nil || len(list.Msg.Runs) != 1 || list.Msg.Runs[0].CreatedBy != "samuel" {
		t.Fatalf("baseline runs = %+v, err = %v", list, err)
	}

	all, err := legacy.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range all.Msg.Entries {
		if e.Id == 0 || e.At == nil || strings.Contains(e.Detail, "secret") || strings.Contains(e.Detail, "CREATE TABLE") {
			t.Fatalf("entry = %+v", e)
		}
		got = append(got, e.Actor+" "+e.Action+" "+e.Target)
	}
	want := "samuel target.baseline legacy|anonymous run.revert app|ci run.create app|ci target.register legacy|samuel target.register app"
	if strings.Join(got, "|") != want {
		t.Fatalf("audit:\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
	createEntry, revertEntry := all.Msg.Entries[2], all.Msg.Entries[1]
	if createEntry.RunId != run.Id || createEntry.Detail != "rollout=direct migrations=1 acked= source="+source+" plan=" {
		t.Fatalf("create entry = %+v", createEntry)
	}
	if revertEntry.RunId != reverted.Msg.RunId || revertEntry.Detail != "reverts="+run.Id+" migrations=1 acked=H002 forced=false allow_data_loss=false" {
		t.Fatalf("revert entry = %+v", revertEntry)
	}
	if all.Msg.Entries[0].RunId != baselined.Msg.RunId || all.Msg.Entries[0].Detail != "version=1 migrations=1" {
		t.Fatalf("baseline entry = %+v", all.Msg.Entries[0])
	}
	if all.Msg.Entries[4].Detail != "provider=static lock_timeout= statement_timeout= require_plan=false search_path=" {
		t.Fatalf("register entry = %+v", all.Msg.Entries[4])
	}

	byTarget, err := ci.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{Target: "app", Limit: 2}))
	if err != nil || len(byTarget.Msg.Entries) != 2 || byTarget.Msg.Entries[0].Action != controlplane.AuditRunRevert {
		t.Fatalf("by target = %+v, err = %v", byTarget, err)
	}
	byRun, err := ci.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{RunId: run.Id}))
	if err != nil || len(byRun.Msg.Entries) != 1 || byRun.Msg.Entries[0].Action != controlplane.AuditRunCreate {
		t.Fatalf("by run = %+v, err = %v", byRun, err)
	}
	if _, err := ci.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{RunId: "not-a-uuid"})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("bad run filter: %v", err)
	}

	execStore(t, storeDSN, "DROP TABLE cp_audit")
	if _, err := samuel.AcceptBaseline(ctx, connect.NewRequest(&godwitv1.AcceptBaselineRequest{Target: "legacy"})); err != nil {
		t.Fatalf("mutation must survive a lost audit table: %v", err)
	}
	if _, err := ci.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("list without table: %v", err)
	}
}

func TestRunRejectsBadTokenSpec(t *testing.T) {
	t.Parallel()

	err := Run(context.Background(), Config{Tokens: []string{"ci:"}, Log: testLog})
	if err == nil || !strings.Contains(err.Error(), "token #1: want name:scope:secret, name:secret or a bare secret") {
		t.Fatalf("bad token spec: %v", err)
	}
}
