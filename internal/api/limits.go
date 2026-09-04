package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

// Admission defaults. A directory is counted in migrations because a file count halves in practice; 2000
// of them is 4000 files and, at the 8 KiB a file RequestBytes was sized for, 32 MiB — so none of the three
// stops a directory the others admit. FileBytes holds a generated schema dump; HeavyCalls is how many
// requests may build scratch databases at once.
const (
	DefaultRequestBytes = 32 << 20
	DefaultMigrations   = 2000
	DefaultFiles        = 5000
	DefaultFileBytes    = 4 << 20
	DefaultHeavyCalls   = 4
	DefaultHeavyWait    = 30 * time.Second
	maxNameBytes        = 512
	upSuffix            = ".up.sql"
)

// Limits are the admission bounds a Server applies to every request; a zero field takes its default.
type Limits struct {
	// RequestBytes caps one decoded request body.
	RequestBytes int
	// Migrations caps how many migrations one request may carry; the up half is what names one.
	Migrations int
	// Files caps how many files one request may carry, migration halves and everything else alike.
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
	if l.Migrations <= 0 {
		l.Migrations = DefaultMigrations
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
	migrations := 0
	for _, f := range in {
		if len(f.Name) > maxNameBytes {
			return invalid(fmt.Sprintf("migration file name is %d bytes, limit %d", len(f.Name), maxNameBytes))
		}
		if len(f.Body) > l.FileBytes {
			return invalid(fmt.Sprintf("migration file %s is %d bytes, limit %d", f.Name, len(f.Body), l.FileBytes))
		}
		if strings.HasSuffix(f.Name, upSuffix) {
			migrations++
		}
	}
	if migrations > l.Migrations {
		return invalid(fmt.Sprintf("too many migrations: %d, limit %d", migrations, l.Migrations))
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
