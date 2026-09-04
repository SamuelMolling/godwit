package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"connectrpc.com/connect"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
)

var errCheckpointDisabled = connect.NewError(connect.CodeUnimplemented, errors.New("checkpoints are not enabled"))

var checkpointName = regexp.MustCompile(`^[a-z0-9_]+$`)

// CheckpointGenerator builds a checkpoint from a migration directory (implemented by controlplane.Checkpointer).
type CheckpointGenerator interface {
	Generate(ctx context.Context, files map[string]string, at int64, name string, now time.Time) (controlplane.Checkpoint, error)
}

// Checkpoint collapses a prefix of the submitted directory into one file: the schema those migrations
// produce on a scratch database, verified by replaying the generated DDL on another one.
func (s *Server) Checkpoint(ctx context.Context, req *connect.Request[godwitv1.CheckpointRequest]) (*connect.Response[godwitv1.CheckpointResponse], error) {
	m := req.Msg
	if len(m.Files) == 0 {
		return nil, invalid("files is required")
	}
	if !checkpointName.MatchString(m.Name) {
		return nil, invalid("name is required and must be snake_case ([a-z0-9_]+)")
	}
	if s.Checkpointer == nil {
		return nil, errCheckpointDisabled
	}
	files := make(map[string]string, len(m.Files))
	for _, f := range m.Files {
		files[f.Name] = f.Body
	}
	cp, err := s.Checkpointer.Generate(ctx, files, m.At, m.Name, time.Now())
	if err != nil {
		if errors.Is(err, controlplane.ErrCheckpoint) || errors.Is(err, controlplane.ErrMigrationFiles) ||
			errors.Is(err, controlplane.ErrDirective) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}

		return nil, rpcErr(err)
	}
	s.Log.Info("checkpoint generated", "version", cp.Version, "through", cp.Through, "covers", len(cp.Covers))
	s.audit(ctx, controlplane.AuditCheckpoint, "", "",
		fmt.Sprintf("file=%s through=%014d covers=%d", cp.UpFile(), cp.Through, len(cp.Covers)))

	return connect.NewResponse(&godwitv1.CheckpointResponse{
		Version: cp.Version, Name: cp.Name, Through: cp.Through, Covers: cp.Covers, Body: cp.Body,
	}), nil
}
