package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
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

func TestCheckFilesRefusesABodySetOverTheAggregateBound(t *testing.T) {
	t.Parallel()

	s := &Server{Limits: limits.Limits{RequestBytes: 4 << 20}}
	err := s.checkFiles(directory(4, strings.Repeat("x", 1<<20)))
	if connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "over the 4194304 bytes a request may hold in total") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheScratchGateAdmitsAndRefuses(t *testing.T) {
	t.Parallel()

	g := newGate(limits.Limits{HeavyCalls: 1, HeavyWait: 20 * time.Millisecond}.WithDefaults())
	held, err := g.enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.enter(context.Background()); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("a full gate must refuse: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.enter(ctx); connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("a cancelled caller must not wait out the gate: %v", err)
	}
	held()
	leave, err := g.enter(context.Background())
	if err != nil {
		t.Fatalf("the slot must be free again: %v", err)
	}
	leave()
}

func TestOneGateCountsEveryCallerOfAScratchProcedureInProcess(t *testing.T) {
	t.Parallel()

	s := &Server{Limits: limits.Limits{HeavyCalls: 1, HeavyWait: 20 * time.Millisecond}}
	held, err := s.enterScratch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"PlanRun": func() error {
			_, err := s.PlanRun(ctx, connect.NewRequest(&godwitv1.PlanRunRequest{}))

			return err
		},
		"CreateRun": func() error {
			_, err := s.CreateRun(ctx, connect.NewRequest(&godwitv1.CreateRunRequest{}))

			return err
		},
		"RevertRun": func() error {
			_, err := s.RevertRun(ctx, connect.NewRequest(&godwitv1.RevertRunRequest{}))

			return err
		},
		"Diff": func() error {
			_, err := s.Diff(ctx, connect.NewRequest(&godwitv1.DiffRequest{}))

			return err
		},
		"Checkpoint": func() error {
			_, err := s.Checkpoint(ctx, connect.NewRequest(&godwitv1.CheckpointRequest{}))

			return err
		},
	} {
		if err := call(); connect.CodeOf(err) != connect.CodeResourceExhausted {
			t.Errorf("%s ran past a full gate: %v", name, err)
		}
	}
	held()
}
