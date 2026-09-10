package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/limits"
)

// heavy names the procedures that build scratch databases, one or more per call.
var heavy = map[string]bool{
	godwitv1connect.GodwitServiceDiffProcedure:       true,
	godwitv1connect.GodwitServicePlanRunProcedure:    true,
	godwitv1connect.GodwitServiceCreateRunProcedure:  true,
	godwitv1connect.GodwitServiceRevertRunProcedure:  true,
	godwitv1connect.GodwitServiceCheckpointProcedure: true,
}

var errBusy = connect.NewError(connect.CodeResourceExhausted,
	errors.New("too many concurrent validation requests; retry shortly"))

// gate caps how many scratch-database requests run at once, so a burst cannot exhaust the scratch server.
type gate struct {
	slots chan struct{}
	wait  time.Duration
}

func newGate(l limits.Limits) *gate {
	return &gate{slots: make(chan struct{}, l.HeavyCalls), wait: l.HeavyWait}
}

func (g *gate) enter(ctx context.Context, procedure string) (func(), error) {
	if !heavy[procedure] {
		return func() {}, nil
	}
	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, nil
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
	case <-timer.C:
		return nil, errBusy
	}
}

// WrapUnary implements connect.Interceptor.
func (g *gate) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		leave, err := g.enter(ctx, req.Spec().Procedure)
		if err != nil {
			return nil, err
		}
		defer leave()

		return next(ctx, req)
	}
}

// WrapStreamingClient implements connect.Interceptor.
func (*gate) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor; no streaming procedure is heavy.
func (*gate) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
