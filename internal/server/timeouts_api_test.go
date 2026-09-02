package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestAPITimeoutValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")

	cases := []struct {
		name string
		call func() error
	}{
		{"register bad lock", func() error {
			_, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
				Name: "a", Provider: "kubernetes", SecretPath: "/s", LockTimeout: "0",
			}))

			return err
		}},
		{"register bad statement", func() error {
			_, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
				Name: "a", Provider: "kubernetes", SecretPath: "/s", StatementTimeout: "later",
			}))

			return err
		}},
		{"create bad statement", func() error {
			_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
				Target: "a", Files: migrationFiles(), StatementTimeout: "-1s",
			}))

			return err
		}},
		{"revert bad lock", func() error {
			_, err := client.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{RunId: "x", LockTimeout: "5"}))

			return err
		}},
	}
	for _, tc := range cases {
		err := tc.call()
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "_timeout: ") {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
}

func TestAPITimeoutsEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	baseURL := startService(t, newDatabase(t, "st"), "r1", nil)
	client := newClient(baseURL, "")

	targetDSN := newDatabase(t, "tg")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: targetDSN, LockTimeout: "2s", StatementTimeout: "1m",
	})); err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: []*godwitv1.MigrationFile{
			{Name: "20260901120000_slow.up.sql", Body: "SELECT pg_sleep(30);"},
			{Name: "20260901120000_slow.down.sql", Body: "SELECT 1;"},
		},
		StatementTimeout: "200ms", SkipValidation: true,
	}))
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(20 * time.Second)
	var run *godwitv1.Run
	for run == nil || run.State != godwitv1.RunState_RUN_STATE_FAILED {
		if time.Now().After(deadline) {
			t.Fatalf("run = %+v", run)
		}
		got, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: created.Msg.RunId}))
		if err != nil {
			t.Fatal(err)
		}
		run = got.Msg.Run
		time.Sleep(50 * time.Millisecond)
	}
	if run.LockTimeout != "" || run.StatementTimeout != "200ms" || !strings.Contains(run.Error, "statement timeout") {
		t.Fatalf("run = %+v", run)
	}
	if body := scrapeMetrics(t, baseURL); !strings.Contains(body, `godwit_statement_failures_total{reason="statement_timeout",target="app"} 1`) {
		t.Fatalf("metrics:\n%s", body)
	}
}
