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

func TestRPCErrMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  error
		code connect.Code
	}{
		{controlplane.ErrNotFound, connect.CodeNotFound},
		{controlplane.ErrNotResumable, connect.CodeFailedPrecondition},
		{controlplane.ErrNotAwaitingContract, connect.CodeFailedPrecondition},
		{controlplane.ErrNotRevertable, connect.CodeFailedPrecondition},
		{errors.New("other"), connect.CodeInternal},
	}
	for _, tc := range cases {
		if got := connect.CodeOf(rpcErr(tc.err)); got != tc.code {
			t.Fatalf("rpcErr(%v) = %v, want %v", tc.err, got, tc.code)
		}
	}
}

func TestAuthAllowed(t *testing.T) {
	t.Parallel()

	open := newAuth(nil)
	if !open.allowed("") {
		t.Fatal("empty token set must allow everything")
	}
	locked := newAuth([]string{"t1"})
	if locked.allowed("") || locked.allowed("Bearer nope") || locked.allowed("t1") {
		t.Fatal("bad credentials must be rejected")
	}
	if !locked.allowed("Bearer t1") {
		t.Fatal("valid token must pass")
	}
}

func TestAuthWrapStreamingClientPassthrough(t *testing.T) {
	t.Parallel()

	called := false
	next := connect.StreamingClientFunc(func(context.Context, connect.Spec) connect.StreamingClientConn {
		called = true

		return nil
	})
	newAuth(nil).WrapStreamingClient(next)(context.Background(), connect.Spec{})
	if !called {
		t.Fatal("must pass through")
	}
}

func TestToProtoFinished(t *testing.T) {
	t.Parallel()

	now := time.Now()
	pb := toProto(controlplane.Run{ID: "r", State: controlplane.StateSucceeded, FinishedAt: &now})
	if pb.FinishedAt == nil {
		t.Fatal("finished_at must be set")
	}
}

func TestSleepCtxCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestDriftDisabled(t *testing.T) {
	t.Parallel()

	s := NewServer(nil, nil, nil, nil)
	ctx := context.Background()
	if _, err := s.CheckDrift(ctx, connect.NewRequest(&godwitv1.CheckDriftRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.AcceptBaseline(ctx, connect.NewRequest(&godwitv1.AcceptBaselineRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckHazards(t *testing.T) {
	t.Parallel()

	m := engine.Migration{Version: 1, Name: "d", UpSQL: "DROP TABLE x;", DownSQL: "SELECT 1;"}
	p, err := engine.BuildPlan(m, engine.DirectionUp)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewServer(nil, nil, nil, nil).checkHazards([]engine.Plan{p}, nil); err == nil || !strings.Contains(err.Error(), "H002") {
		t.Fatalf("err = %v", err)
	}
	if err := NewServer(nil, nil, nil, nil).checkHazards([]engine.Plan{p}, []string{"H002"}); err != nil {
		t.Fatalf("acked: %v", err)
	}
}

func TestSettled(t *testing.T) {
	t.Parallel()

	for state, want := range map[string]bool{
		controlplane.StateQueued:           false,
		controlplane.StateRunning:          false,
		controlplane.StateSucceeded:        true,
		controlplane.StateFailed:           true,
		controlplane.StateNeedsAttention:   true,
		controlplane.StateAwaitingContract: true,
	} {
		if settled(state) != want {
			t.Fatalf("settled(%s) != %v", state, want)
		}
	}
}
