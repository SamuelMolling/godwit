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
)

func TestLimitsDefaults(t *testing.T) {
	t.Parallel()

	l := Limits{}.withDefaults()
	if l.RequestBytes != DefaultRequestBytes || l.Files != DefaultFiles || l.FileBytes != DefaultFileBytes ||
		l.HeavyCalls != DefaultHeavyCalls || l.HeavyWait != DefaultHeavyWait {
		t.Fatalf("defaults = %+v", l)
	}
	set := Limits{RequestBytes: 1, Files: 2, FileBytes: 3, HeavyCalls: 4, HeavyWait: time.Second}
	if got := set.withDefaults(); got != set {
		t.Fatalf("explicit limits = %+v, want %+v", got, set)
	}
}

// A real 200-migration directory must pass; the counts and sizes above it must not.
func TestCheckFiles(t *testing.T) {
	t.Parallel()

	l := Limits{}.withDefaults()
	real := make([]*godwitv1.MigrationFile, 0, 400)
	for i := range 200 {
		body := strings.Repeat("-- migration\n", 600)
		real = append(real,
			&godwitv1.MigrationFile{Name: "2026090112" + strconv.Itoa(i) + "_t.up.sql", Body: body},
			&godwitv1.MigrationFile{Name: "2026090112" + strconv.Itoa(i) + "_t.down.sql", Body: body})
	}
	if err := l.checkFiles(real); err != nil {
		t.Fatalf("a 200-migration directory must be admitted: %v", err)
	}

	cases := []struct {
		name string
		in   []*godwitv1.MigrationFile
		want string
	}{
		{"too many", make([]*godwitv1.MigrationFile, l.Files+1), "too many migration files"},
		{"long name", []*godwitv1.MigrationFile{{Name: strings.Repeat("n", maxNameBytes+1)}}, "file name is"},
		{"big body", []*godwitv1.MigrationFile{{Name: "a.up.sql", Body: strings.Repeat("x", l.FileBytes+1)}}, "bytes, limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := l.checkFiles(tc.in)
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestGateAdmitsAndRefuses(t *testing.T) {
	t.Parallel()

	g := newGate(Limits{HeavyCalls: 1, HeavyWait: 20 * time.Millisecond}.withDefaults())
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
	// Checkpoint needs only read and builds two scratch databases per call.
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

	g := newGate(Limits{HeavyCalls: 1, HeavyWait: 20 * time.Millisecond}.withDefaults())
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
