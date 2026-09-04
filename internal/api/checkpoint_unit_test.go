package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/pashagolub/pgxmock/v4"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

type stubCheckpointer struct {
	cp  controlplane.Checkpoint
	err error
	at  int64
}

func (s *stubCheckpointer) Generate(_ context.Context, _ map[string]string, at int64, name string, _ time.Time) (controlplane.Checkpoint, error) {
	s.at = at
	s.cp.Name = name

	return s.cp, s.err
}

func checkpointServer(t *testing.T, gen CheckpointGenerator) *Server {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectExec("INSERT INTO cp_audit").WithArgs(anyArgs(5)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1)).Maybe()
	s := NewServer(controlplane.NewStore(mock), nil, nil, nil)
	s.Checkpointer = gen

	return s
}

func checkpointRequest() *godwitv1.CheckpointRequest {
	return &godwitv1.CheckpointRequest{
		Files: []*godwitv1.MigrationFile{{Name: "20260101000000_a.up.sql", Body: "SELECT 1;"}},
		At:    20260101000000, Name: "squash",
	}
}

func TestCheckpointHandler(t *testing.T) {
	t.Parallel()
	gen := &stubCheckpointer{cp: controlplane.Checkpoint{
		Version: 20260901000000, Through: 20260101000000, Covers: []string{"20260101000000_a"}, Body: "-- godwit: checkpoint through=20260101000000\n",
	}}
	s := checkpointServer(t, gen)

	res, err := s.Checkpoint(context.Background(), connect.NewRequest(checkpointRequest()))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg; got.Version != 20260901000000 || got.Name != "squash" || got.Through != 20260101000000 ||
		len(got.Covers) != 1 || !strings.HasPrefix(got.Body, "-- godwit: checkpoint") {
		t.Fatalf("response = %+v", got)
	}
	if gen.at != 20260101000000 {
		t.Fatalf("at = %d", gen.at)
	}
}

func TestCheckpointHandlerRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	empty := checkpointRequest()
	empty.Files = nil
	if _, err := checkpointServer(t, &stubCheckpointer{}).Checkpoint(ctx, connect.NewRequest(empty)); err == nil ||
		!strings.Contains(err.Error(), "files is required") {
		t.Fatalf("err = %v", err)
	}
	bad := checkpointRequest()
	bad.Name = "Bad Name"
	if _, err := checkpointServer(t, &stubCheckpointer{}).Checkpoint(ctx, connect.NewRequest(bad)); err == nil ||
		!strings.Contains(err.Error(), "snake_case") {
		t.Fatalf("err = %v", err)
	}
	if _, err := checkpointServer(t, nil).Checkpoint(ctx, connect.NewRequest(checkpointRequest())); err == nil ||
		connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("err = %v", err)
	}

	refused := &stubCheckpointer{err: controlplane.ErrCheckpoint}
	if _, err := checkpointServer(t, refused).Checkpoint(ctx, connect.NewRequest(checkpointRequest())); err == nil ||
		connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err = %v", err)
	}
	broke := &stubCheckpointer{err: errors.New("boom")}
	if _, err := checkpointServer(t, broke).Checkpoint(ctx, connect.NewRequest(checkpointRequest())); err == nil ||
		connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("err = %v", err)
	}
}
