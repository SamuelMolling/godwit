package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

// Admission defaults. RequestBytes holds a directory an order of magnitude larger than any real one
// (200 migrations of 8 KiB is under 2 MiB); FileBytes holds a generated schema dump; HeavyCalls is the
// number of requests allowed to build scratch databases at once.
const (
	DefaultRequestBytes = 32 << 20
	DefaultFiles        = 2000
	DefaultFileBytes    = 4 << 20
	DefaultHeavyCalls   = 4
	DefaultHeavyWait    = 30 * time.Second
	maxNameBytes        = 512
)

// Limits are the admission bounds a Server applies to every request; a zero field takes its default.
type Limits struct {
	// RequestBytes caps one decoded request body.
	RequestBytes int
	// Files caps how many migration files one request may carry.
	Files int
	// FileBytes caps one migration body and one desired schema.
	FileBytes int
	// HeavyCalls caps concurrent Diff, PlanRun, CreateRun, RevertRun and Checkpoint calls.
	HeavyCalls int
	// HeavyWait is how long a call queues for a free slot before it is refused.
	HeavyWait time.Duration
}

func (l Limits) withDefaults() Limits {
	if l.RequestBytes <= 0 {
		l.RequestBytes = DefaultRequestBytes
	}
	if l.Files <= 0 {
		l.Files = DefaultFiles
	}
	if l.FileBytes <= 0 {
		l.FileBytes = DefaultFileBytes
	}
	if l.HeavyCalls <= 0 {
		l.HeavyCalls = DefaultHeavyCalls
	}
	if l.HeavyWait <= 0 {
		l.HeavyWait = DefaultHeavyWait
	}

	return l
}

func (l Limits) checkFiles(in []*godwitv1.MigrationFile) error {
	if len(in) > l.Files {
		return invalid(fmt.Sprintf("too many migration files: %d, limit %d", len(in), l.Files))
	}
	for _, f := range in {
		if len(f.Name) > maxNameBytes {
			return invalid(fmt.Sprintf("migration file name is %d bytes, limit %d", len(f.Name), maxNameBytes))
		}
		if len(f.Body) > l.FileBytes {
			return invalid(fmt.Sprintf("migration file %s is %d bytes, limit %d", f.Name, len(f.Body), l.FileBytes))
		}
	}

	return nil
}

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

func newGate(l Limits) *gate {
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
