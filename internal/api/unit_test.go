package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/SamuelMolling/godwit/internal/controlplane"
)

func TestRPCErrMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  error
		code connect.Code
	}{
		{controlplane.ErrNotFound, connect.CodeNotFound},
		{controlplane.ErrNotResumable, connect.CodeFailedPrecondition},
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

func TestTerminal(t *testing.T) {
	t.Parallel()

	for state, want := range map[string]bool{
		controlplane.StateQueued:         false,
		controlplane.StateRunning:        false,
		controlplane.StateSucceeded:      true,
		controlplane.StateFailed:         true,
		controlplane.StateNeedsAttention: true,
	} {
		if terminal(state) != want {
			t.Fatalf("terminal(%s) != %v", state, want)
		}
	}
}
