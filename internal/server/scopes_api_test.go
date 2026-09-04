package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

func TestTokenScopesEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	baseURL := startService(t, newDatabase(t, "st"), "r1", []string{
		"root:admin:root-secret", "ops:operator:ops-secret", "deploy:pipeline:deploy-secret", "bot:read:bot-secret",
	})
	root, ops, deploy, bot := newClient(baseURL, "root-secret"), newClient(baseURL, "ops-secret"),
		newClient(baseURL, "deploy-secret"), newClient(baseURL, "bot-secret")
	targetDSN := newDatabase(t, "tg")

	denied := func(name string, err error, want string) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want permission denied naming %q", name, err, want)
		}
	}
	register := func(c godwitv1connect.GodwitServiceClient) error {
		_, err := c.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{Name: "app", Provider: "static", Dsn: targetDSN}))

		return err
	}
	denied("operator registers", register(ops), "RegisterTarget requires scope admin; token ops has scope operator")
	denied("pipeline registers", register(deploy), "RegisterTarget requires scope admin; token deploy has scope pipeline")
	denied("read registers", register(bot), "RegisterTarget requires scope admin; token bot has scope read")
	if err := register(root); err != nil {
		t.Fatal(err)
	}

	_, err := bot.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: migrationFiles()}))
	denied("read creates", err, "CreateRun requires scope pipeline")
	created, err := deploy.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "app", Files: migrationFiles()}))
	if err != nil {
		t.Fatal(err)
	}
	if run := watchToEnd(t, bot, created.Msg.RunId); run.State != godwitv1.RunState_RUN_STATE_SUCCEEDED || run.CreatedBy != "deploy" {
		t.Fatalf("run = %+v", run)
	}

	_, err = deploy.ResumeRun(ctx, connect.NewRequest(&godwitv1.ResumeRunRequest{RunId: created.Msg.RunId}))
	denied("pipeline resumes", err, "ResumeRun requires scope operator")
	if _, err := ops.ResumeRun(ctx, connect.NewRequest(&godwitv1.ResumeRunRequest{RunId: created.Msg.RunId})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("operator resumes a finished run: %v", err)
	}
	_, err = bot.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "app"}))
	denied("read checks drift", err, "CheckDrift requires scope operator")
	if _, err := ops.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{Target: "app"})); err != nil {
		t.Fatalf("operator checks drift: %v", err)
	}
	for name, err := range map[string]error{
		"read lists runs":    call(bot.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{}))),
		"read gets status":   call(bot.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app"}))),
		"read lists targets": call(bot.ListTargets(ctx, connect.NewRequest(&godwitv1.ListTargetsRequest{}))),
		"read lists fleet":   call(bot.ListMigrations(ctx, connect.NewRequest(&godwitv1.ListMigrationsRequest{}))),
		"read plans":         call(bot.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{Target: "app", Files: migrationFiles()}))),
		"read lists drift":   call(bot.ListDriftEvents(ctx, connect.NewRequest(&godwitv1.ListDriftEventsRequest{Target: "app"}))),
		"read lists audit":   call(bot.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{}))),
		"read lists plans":   call(bot.ListPlans(ctx, connect.NewRequest(&godwitv1.ListPlansRequest{Target: "app"}))),
		"read gets plan":     call(bot.GetPlan(ctx, connect.NewRequest(&godwitv1.GetPlanRequest{PlanId: "missing"}))),
		"read diffs":         call(bot.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{Target: "app", Schema: "CREATE TABLE t (id int);"}))),
		"pipeline confirms":  call(deploy.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: created.Msg.RunId}))),
	} {
		if connect.CodeOf(err) == connect.CodePermissionDenied || connect.CodeOf(err) == connect.CodeUnauthenticated {
			t.Fatalf("%s: %v", name, err)
		}
	}

	audit, err := bot.ListAudit(ctx, connect.NewRequest(&godwitv1.ListAuditRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range audit.Msg.Entries {
		got = append(got, e.Actor+" "+e.Action)
	}
	if want := "deploy run.create|root target.register"; strings.Join(got, "|") != want {
		t.Fatalf("audit:\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
}

func call[T any](_ *connect.Response[T], err error) error {
	return err
}
