package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/limits"
)

func directory(n int, body string) []*godwitv1.MigrationFile {
	out := make([]*godwitv1.MigrationFile, 0, 2*n)
	for i := range n {
		id := "2026090112" + strconv.Itoa(i) + "_t"
		out = append(out,
			&godwitv1.MigrationFile{Name: id + ".up.sql", Body: body},
			&godwitv1.MigrationFile{Name: id + ".down.sql", Body: body})
	}

	return out
}

func TestCheckFilesMeasuresBodiesAndRefusesAsInvalidArgument(t *testing.T) {
	t.Parallel()

	s := &Server{}
	if err := s.checkFiles(directory(200, strings.Repeat("-- migration\n", 600))); err != nil {
		t.Fatalf("a 200-migration directory must be admitted: %v", err)
	}
	err := s.checkFiles([]*godwitv1.MigrationFile{
		{Name: "a.up.sql", Body: strings.Repeat("x", limits.DefaultFileBytes+1)},
	})
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "bytes, limit") {
		t.Fatalf("err = %v", err)
	}
}

func TestGateAdmitsAndRefuses(t *testing.T) {
	t.Parallel()

	g := newGate(limits.Limits{HeavyCalls: 1, HeavyWait: 20 * time.Millisecond}.WithDefaults())
	leave, err := g.enter(context.Background(), godwitv1connect.GodwitServiceListRunsProcedure)
	if err != nil {
		t.Fatal(err)
	}
	leave()

	held, err := g.enter(context.Background(), godwitv1connect.GodwitServiceDiffProcedure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.enter(context.Background(), godwitv1connect.GodwitServiceDiffProcedure); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("a full gate must refuse: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.enter(ctx, godwitv1connect.GodwitServicePlanRunProcedure); connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("a cancelled caller must not wait out the gate: %v", err)
	}
	if _, err := g.enter(context.Background(), godwitv1connect.GodwitServiceCheckpointProcedure); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("Checkpoint must queue with the other scratch-database calls: %v", err)
	}
	held()
	if _, err := g.enter(context.Background(), godwitv1connect.GodwitServiceDiffProcedure); err != nil {
		t.Fatalf("the slot must be free again: %v", err)
	}
}

func TestGateInterceptor(t *testing.T) {
	t.Parallel()

	g := newGate(limits.Limits{HeavyCalls: 1, HeavyWait: 20 * time.Millisecond}.WithDefaults())
	blocked := make(chan struct{})
	release := make(chan struct{})
	unary := g.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		close(blocked)
		<-release

		return connect.NewResponse(&struct{}{}), nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := unary(context.Background(), specRequest{procedure: godwitv1connect.GodwitServiceDiffProcedure})
		done <- err
	}()
	<-blocked
	_, err := g.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		t.Error("the handler must not run while the gate is full")

		return nil, nil
	})(context.Background(), specRequest{procedure: godwitv1connect.GodwitServiceDiffProcedure})
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("err = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	called := false
	g.WrapStreamingClient(func(context.Context, connect.Spec) connect.StreamingClientConn { return nil })(context.Background(), connect.Spec{})
	if err := g.WrapStreamingHandler(func(context.Context, connect.StreamingHandlerConn) error {
		called = true

		return nil
	})(context.Background(), streamConn{}); err != nil || !called {
		t.Fatalf("streaming must pass through: called=%v err=%v", called, err)
	}
}
