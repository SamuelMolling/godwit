package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

type stubInspector struct {
	status controlplane.TargetStatus
	obs    controlplane.Observation
	err    error
}

func (i stubInspector) Status(context.Context, string) (controlplane.TargetStatus, error) {
	return i.status, i.err
}

func (i stubInspector) Observe(context.Context, string) (controlplane.Observation, error) {
	return i.obs, i.err
}

func TestGetTargetStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewServer(nil, nil, nil, nil)
	now := time.Now()
	finished := now.Add(time.Minute)
	s.Inspector = stubInspector{status: controlplane.TargetStatus{
		Target: "app", Provider: "static", Timeouts: controlplane.Timeouts{Lock: "3s", Statement: "1m"},
		Applied: []engine.Applied{
			{Version: 1, Name: "init", Checksum: "old", AppliedAt: now},
			{Version: 2, Name: "extra", Checksum: "x", AppliedAt: now},
		},
		LastRun:   &controlplane.Run{ID: "r1", Kind: controlplane.KindMigrate, State: controlplane.StateSucceeded, FinishedAt: &finished},
		Snapshot:  &controlplane.Snapshot{RunID: "r1", TakenAt: now},
		OpenDrift: true,
	}}
	files := []*godwitv1.MigrationFile{
		{Name: "00000000000001_init.up.sql", Body: "CREATE TABLE t (id int);"},
		{Name: "00000000000001_init.down.sql", Body: "DROP TABLE t;"},
		{Name: "00000000000003_next.up.sql", Body: "SELECT 1;"},
		{Name: "00000000000003_next.down.sql", Body: "SELECT 1;"},
	}

	if _, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "target is required") {
		t.Fatalf("no target: %v", err)
	}
	if _, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{
		Target: "app", Files: files[:1],
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad files: %v", err)
	}

	res, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app", Files: files}))
	if err != nil {
		t.Fatal(err)
	}
	got := res.Msg
	if got.Target != "app" || got.Provider != "static" || got.LockTimeout != "3s" || got.StatementTimeout != "1m" {
		t.Fatalf("head = %+v", got)
	}
	if len(got.Applied) != 2 || !got.Applied[0].ChecksumMismatch || got.Applied[1].ChecksumMismatch ||
		got.Applied[0].Checksum != "old" || got.Applied[0].AppliedAt.AsTime().IsZero() {
		t.Fatalf("applied = %+v", got.Applied)
	}
	if len(got.Pending) != 1 || got.Pending[0].Version != 3 || got.Pending[0].Name != "next" {
		t.Fatalf("pending = %+v", got.Pending)
	}
	if got.LastRun == nil || got.LastRun.Id != "r1" || got.LastRun.State != godwitv1.RunState_RUN_STATE_SUCCEEDED || got.LastRun.FinishedAt == nil {
		t.Fatalf("last run = %+v", got.LastRun)
	}
	if got.DriftBaseline == nil || got.DriftBaseline.RunId != "r1" || !got.DriftBaseline.UnresolvedDrift || got.DriftBaseline.TakenAt == nil {
		t.Fatalf("baseline = %+v", got.DriftBaseline)
	}

	s.Inspector = stubInspector{status: controlplane.TargetStatus{Target: "bare", Provider: "static"}}
	res, err = s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "bare"}))
	if err != nil || len(res.Msg.Applied) != 0 || len(res.Msg.Pending) != 0 || res.Msg.LastRun != nil || res.Msg.DriftBaseline != nil {
		t.Fatalf("bare = %+v, err = %v", res, err)
	}

	s.Inspector = stubInspector{err: controlplane.ErrNotFound}
	if _, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "ghost"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown target: %v", err)
	}
	s.Inspector = stubInspector{err: errors.New("boom")}
	if _, err := s.GetTargetStatus(ctx, connect.NewRequest(&godwitv1.GetTargetStatusRequest{Target: "app"})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("internal: %v", err)
	}
}
