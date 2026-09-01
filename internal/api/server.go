// Package api serves the godwit control-plane API over connect (gRPC + JSON).
package api

import (
	"context"
	"errors"
	"net/http"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
)

// Server implements godwit.v1.GodwitService over the control-plane store.
type Server struct {
	store         *controlplane.Store
	masterKey     []byte
	watchInterval time.Duration
	newID         func() string
}

// NewServer wires a Server.
func NewServer(store *controlplane.Store, masterKey []byte) *Server {
	return &Server{store: store, masterKey: masterKey, watchInterval: 500 * time.Millisecond, newID: uuid.NewString}
}

// Handler mounts the connect service (gRPC and JSON) with bearer-token auth.
// Serve it with an http.Server whose Protocols enable unencrypted HTTP/2.
func Handler(s *Server, tokens []string) http.Handler {
	mux := http.NewServeMux()
	path, h := godwitv1connect.NewGodwitServiceHandler(s, connect.WithInterceptors(newAuth(tokens)))
	mux.Handle(path, h)

	return mux
}

func rpcErr(err error) *connect.Error {
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, controlplane.ErrNotResumable):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

func invalid(msg string) *connect.Error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

// RegisterTarget stores a target with its credential provider config.
func (s *Server) RegisterTarget(ctx context.Context, req *connect.Request[godwitv1.RegisterTargetRequest]) (*connect.Response[godwitv1.RegisterTargetResponse], error) {
	m := req.Msg
	if m.Name == "" {
		return nil, invalid("name is required")
	}
	var config map[string]string
	switch m.Provider {
	case "static":
		if m.Dsn == "" {
			return nil, invalid("static provider requires dsn")
		}
		enc, err := creds.Encrypt(s.masterKey, m.Dsn)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		config = map[string]string{"dsn": enc}
	case "kubernetes":
		if m.SecretPath == "" {
			return nil, invalid("kubernetes provider requires secret_path")
		}
		config = map[string]string{"path": m.SecretPath}
	default:
		return nil, invalid("unknown provider " + m.Provider)
	}
	if err := s.store.RegisterTarget(ctx, m.Name, m.Provider, config); err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.RegisterTargetResponse{}), nil
}

// CreateRun validates the migration files and queues a run.
func (s *Server) CreateRun(ctx context.Context, req *connect.Request[godwitv1.CreateRunRequest]) (*connect.Response[godwitv1.CreateRunResponse], error) {
	m := req.Msg
	if m.Target == "" {
		return nil, invalid("target is required")
	}
	if len(m.Files) == 0 {
		return nil, invalid("at least one migration file is required")
	}
	fsys := fstest.MapFS{}
	files := map[string]string{}
	for _, f := range m.Files {
		fsys[f.Name] = &fstest.MapFile{Data: []byte(f.Body)}
		files[f.Name] = f.Body
	}
	migs, err := engine.LoadFS(fsys)
	if err != nil {
		return nil, invalid(err.Error())
	}
	for _, mig := range migs {
		if _, err := engine.BuildPlan(mig, engine.DirectionUp); err != nil {
			return nil, invalid(err.Error())
		}
	}

	id := s.newID()
	if err := s.store.CreateRun(ctx, id, m.Target, files); err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.CreateRunResponse{RunId: id}), nil
}

func toProto(r controlplane.Run) *godwitv1.Run {
	states := map[string]godwitv1.RunState{
		controlplane.StateQueued:         godwitv1.RunState_RUN_STATE_QUEUED,
		controlplane.StateRunning:        godwitv1.RunState_RUN_STATE_RUNNING,
		controlplane.StateSucceeded:      godwitv1.RunState_RUN_STATE_SUCCEEDED,
		controlplane.StateFailed:         godwitv1.RunState_RUN_STATE_FAILED,
		controlplane.StateNeedsAttention: godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION,
	}
	out := &godwitv1.Run{
		Id:        r.ID,
		Target:    r.Target,
		State:     states[r.State],
		Error:     r.Error,
		Attempts:  int32(r.Attempts),
		CreatedAt: timestamppb.New(r.CreatedAt),
	}
	if r.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*r.FinishedAt)
	}

	return out
}

// GetRun returns one run.
func (s *Server) GetRun(ctx context.Context, req *connect.Request[godwitv1.GetRunRequest]) (*connect.Response[godwitv1.GetRunResponse], error) {
	r, err := s.store.Run(ctx, req.Msg.RunId)
	if err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.GetRunResponse{Run: toProto(r)}), nil
}

// ListRuns returns recent runs, optionally filtered by target.
func (s *Server) ListRuns(ctx context.Context, req *connect.Request[godwitv1.ListRunsRequest]) (*connect.Response[godwitv1.ListRunsResponse], error) {
	runs, err := s.store.ListRuns(ctx, req.Msg.Target)
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &godwitv1.ListRunsResponse{}
	for _, r := range runs {
		resp.Runs = append(resp.Runs, toProto(r))
	}

	return connect.NewResponse(resp), nil
}

func terminal(state string) bool {
	return state == controlplane.StateSucceeded ||
		state == controlplane.StateFailed ||
		state == controlplane.StateNeedsAttention
}

// WatchRun streams run snapshots until the run reaches a terminal state.
func (s *Server) WatchRun(ctx context.Context, req *connect.Request[godwitv1.WatchRunRequest], stream *connect.ServerStream[godwitv1.WatchRunResponse]) error {
	for {
		r, err := s.store.Run(ctx, req.Msg.RunId)
		if err != nil {
			return rpcErr(err)
		}
		// A send failure surfaces as a ctx error on the next store read.
		_ = stream.Send(&godwitv1.WatchRunResponse{Run: toProto(r)})
		if terminal(r.State) {
			return nil
		}
		if err := sleepCtx(ctx, s.watchInterval); err != nil {
			return err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ResumeRun requeues a failed or parked run.
func (s *Server) ResumeRun(ctx context.Context, req *connect.Request[godwitv1.ResumeRunRequest]) (*connect.Response[godwitv1.ResumeRunResponse], error) {
	if err := s.store.Resume(ctx, req.Msg.RunId); err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.ResumeRunResponse{}), nil
}

// ParkRun moves a run to needs_attention.
func (s *Server) ParkRun(ctx context.Context, req *connect.Request[godwitv1.ParkRunRequest]) (*connect.Response[godwitv1.ParkRunResponse], error) {
	if err := s.store.Finish(ctx, req.Msg.RunId, controlplane.StateNeedsAttention, req.Msg.Reason); err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.ParkRunResponse{}), nil
}
