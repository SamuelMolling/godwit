package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	"github.com/SamuelMolling/godwit/internal/limits"
)

var errBusy = connect.NewError(connect.CodeResourceExhausted,
	errors.New("too many concurrent validation requests; retry shortly"))

// scratchGate caps how many scratch-database requests run at once, so a burst cannot exhaust the scratch server.
type scratchGate struct {
	slots chan struct{}
	wait  time.Duration
}

func newGate(l limits.Limits) *scratchGate {
	return &scratchGate{slots: make(chan struct{}, l.HeavyCalls), wait: l.HeavyWait}
}

func (g *scratchGate) enter(ctx context.Context) (func(), error) {
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

func (s *Server) enterScratch(ctx context.Context) (func(), error) {
	s.scratchOnce.Do(func() { s.scratch = newGate(s.limits()) })

	return s.scratch.enter(ctx)
}
