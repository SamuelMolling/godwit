package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestAPIValidationAndErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	client := newClient(startService(t, storeDSN, "r1", nil), "")

	invalidCases := []struct {
		name string
		call func() error
	}{
		{"register empty name", func() error {
			_, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{Provider: "static", Dsn: "x"}))

			return err
		}},
		{"register static without dsn", func() error {
			_, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{Name: "a", Provider: "static"}))

			return err
		}},
		{"register kubernetes without path", func() error {
			_, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{Name: "a", Provider: "kubernetes"}))

			return err
		}},
		{"register unknown provider", func() error {
			_, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{Name: "a", Provider: "vault"}))

			return err
		}},
		{"create without target", func() error {
			_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Files: migrationFiles()}))

			return err
		}},
		{"create without files", func() error {
			_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{Target: "a"}))

			return err
		}},
		{"create with bad file name", func() error {
			_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
				Target: "a", Files: []*godwitv1.MigrationFile{{Name: "junk.txt", Body: "x"}},
			}))

			return err
		}},
		{"create with unparsable sql", func() error {
			_, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
				Target: "a", Files: []*godwitv1.MigrationFile{
					{Name: "20260901120000_x.up.sql", Body: "NOT SQL"},
					{Name: "20260901120000_x.down.sql", Body: "SELECT 1;"},
				},
			}))

			return err
		}},
	}
	for _, tc := range invalidCases {
		if code := connect.CodeOf(tc.call()); code != connect.CodeInvalidArgument {
			t.Fatalf("%s: code = %v, want InvalidArgument", tc.name, code)
		}
	}

	// Kubernetes provider registration succeeds.
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "k8s-app", Provider: "kubernetes", SecretPath: "/var/run/secret/dsn",
	})); err != nil {
		t.Fatal(err)
	}

	// Not-found mappings.
	ghost := "88888888-8888-8888-8888-888888888888"
	if _, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: ghost})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("get ghost: %v", err)
	}
	stream, err := client.WatchRun(ctx, connect.NewRequest(&godwitv1.WatchRunRequest{RunId: ghost}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	if connect.CodeOf(stream.Err()) != connect.CodeNotFound {
		t.Fatalf("watch ghost: %v", stream.Err())
	}
	if _, err := client.ParkRun(ctx, connect.NewRequest(&godwitv1.ParkRunRequest{RunId: ghost})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("park ghost: %v", err)
	}
	if _, err := client.ResumeRun(ctx, connect.NewRequest(&godwitv1.ResumeRunRequest{RunId: ghost})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("resume ghost: %v", err)
	}
}

func TestAPIResumeAndParkFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	client := newClient(startService(t, storeDSN, "r1", nil), "")

	targetDSN := newDatabase(t, "tg")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: targetDSN,
	})); err != nil {
		t.Fatal(err)
	}

	// A failing migration; wait for failed, resume with... same files fail again, then park.
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: []*godwitv1.MigrationFile{
			{Name: "20260901120000_bad.up.sql", Body: "SELECT 1/0;"},
			{Name: "20260901120000_bad.down.sql", Body: "SELECT 1;"},
		},
		SkipValidation: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Msg.RunId

	waitFor := func(want godwitv1.RunState) *godwitv1.Run {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			r, err := client.GetRun(ctx, connect.NewRequest(&godwitv1.GetRunRequest{RunId: runID}))
			if err != nil {
				t.Fatal(err)
			}
			if r.Msg.Run.State == want {
				return r.Msg.Run
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("run never reached %v", want)

		return nil
	}

	waitFor(godwitv1.RunState_RUN_STATE_FAILED)
	if _, err := client.ResumeRun(ctx, connect.NewRequest(&godwitv1.ResumeRunRequest{RunId: runID})); err != nil {
		t.Fatal(err)
	}
	waitFor(godwitv1.RunState_RUN_STATE_FAILED)
	if _, err := client.ParkRun(ctx, connect.NewRequest(&godwitv1.ParkRunRequest{RunId: runID, Reason: "manual hold"})); err != nil {
		t.Fatal(err)
	}
	parked := waitFor(godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION)
	if parked.Error != "manual hold" {
		t.Fatalf("parked error = %q", parked.Error)
	}
}

func TestAPIStreamingAuthAndCancel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	baseURL := startService(t, storeDSN, "r1", []string{"tok1"})

	// Streaming without a token is rejected by the streaming-handler interceptor.
	noAuth := newClient(baseURL, "")
	stream, err := noAuth.WatchRun(ctx, connect.NewRequest(&godwitv1.WatchRunRequest{RunId: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	if connect.CodeOf(stream.Err()) != connect.CodeUnauthenticated {
		t.Fatalf("stream err = %v", stream.Err())
	}

	// Cancelling a watch of a non-terminal run surfaces the context error.
	client := newClient(baseURL, "tok1")
	targetDSN := newDatabase(t, "tg")
	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: targetDSN,
	})); err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: []*godwitv1.MigrationFile{
			{Name: "20260901120000_slow.up.sql", Body: "SELECT pg_sleep(10);"},
			{Name: "20260901120000_slow.down.sql", Body: "SELECT 1;"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	watchCtx, cancelWatch := context.WithCancel(ctx)
	stream, err = client.WatchRun(watchCtx, connect.NewRequest(&godwitv1.WatchRunRequest{RunId: created.Msg.RunId}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("first snapshot missing: %v", stream.Err())
	}
	cancelWatch()
	for stream.Receive() {
	}
	if stream.Err() == nil {
		t.Fatal("cancelled watch must error")
	}
}

func TestAPIRegisterTargetEncryptFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A service booted with a bad master key cannot encrypt static DSNs.
	storeDSN := newDatabase(t, "st")
	baseURL := startServiceWithKey(t, storeDSN, []byte("short"))
	client := newClient(baseURL, "")
	_, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: "postgres://x",
	}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIInternalErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storeDSN := newDatabase(t, "st")
	client := newClient(startService(t, storeDSN, "r1", nil), "")

	// Breaking the store schema turns handler queries into internal errors.
	dropStoreTables(t, storeDSN)

	if _, err := client.RegisterTarget(ctx, connect.NewRequest(&godwitv1.RegisterTargetRequest{
		Name: "app", Provider: "static", Dsn: "postgres://x",
	})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("register: %v", err)
	}
	if _, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: migrationFiles(),
	})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("create via validator: %v", err)
	}
	if _, err := client.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{
		Target: "app", Files: migrationFiles(), SkipValidation: true,
	})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("create via store: %v", err)
	}
	if _, err := client.ListRuns(ctx, connect.NewRequest(&godwitv1.ListRunsRequest{})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("list: %v", err)
	}
	if _, err := client.ListDriftEvents(ctx, connect.NewRequest(&godwitv1.ListDriftEventsRequest{})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("list drift: %v", err)
	}
	if _, err := client.ConfirmRollout(ctx, connect.NewRequest(&godwitv1.ConfirmRolloutRequest{RunId: "x"})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("confirm: %v", err)
	}
}

func TestAPIAcceptBaselineUnknownTarget(t *testing.T) {
	t.Parallel()
	client := newClient(startService(t, newDatabase(t, "st"), "r1", nil), "")
	_, err := client.AcceptBaseline(context.Background(),
		connect.NewRequest(&godwitv1.AcceptBaselineRequest{Target: "ghost"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("err = %v", err)
	}
}
