// Package api serves the godwit control-plane API over connect (gRPC + JSON).
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
)

// DriftOps is the drift surface the API exposes (implemented by the monitor).
type DriftOps interface {
	Check(ctx context.Context, target string) (controlplane.Drift, error)
	AcceptBaseline(ctx context.Context, target string) error
}

// Validator checks migrations before admission.
type Validator interface {
	Validate(ctx context.Context, target string, plans []engine.Plan) error
}

// Server implements godwit.v1.GodwitService over the control-plane store.
type Server struct {
	// Metrics receives admission and API events; replace it before Handler to share a registry.
	Metrics *metrics.Metrics

	store         *controlplane.Store
	drift         DriftOps
	validator     Validator
	masterKey     []byte
	watchInterval time.Duration
	newID         func() string
}

// NewServer wires a Server; drift and validator are optional (nil disables).
func NewServer(store *controlplane.Store, drift DriftOps, validator Validator, masterKey []byte) *Server {
	return &Server{
		Metrics:       metrics.New(),
		store:         store,
		drift:         drift,
		validator:     validator,
		masterKey:     masterKey,
		watchInterval: 500 * time.Millisecond,
		newID:         uuid.NewString,
	}
}

// Handler mounts the connect service with bearer-token auth and /metrics; serve it with h2c enabled.
func Handler(s *Server, tokens []string) http.Handler {
	mux := http.NewServeMux()
	path, h := godwitv1connect.NewGodwitServiceHandler(s, connect.WithInterceptors(s.Metrics.Interceptor(), newAuth(tokens)))
	mux.Handle(path, h)
	mux.Handle("/metrics", s.Metrics.Handler())

	return mux
}

func rpcErr(err error) *connect.Error {
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, controlplane.ErrNotResumable), errors.Is(err, controlplane.ErrNotAwaitingContract),
		errors.Is(err, controlplane.ErrNotRevertable):
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
	case "vault":
		if m.VaultPath == "" {
			return nil, invalid("vault provider requires vault_path")
		}
		config = map[string]string{"path": m.VaultPath}
		if m.VaultTemplate != "" {
			config["template"] = m.VaultTemplate
		}
	default:
		return nil, invalid("unknown provider " + m.Provider)
	}
	if err := s.store.RegisterTarget(ctx, m.Name, m.Provider, config); err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.RegisterTargetResponse{}), nil
}

// CreateRun validates and queues a run.
func (s *Server) CreateRun(ctx context.Context, req *connect.Request[godwitv1.CreateRunRequest]) (*connect.Response[godwitv1.CreateRunResponse], error) {
	m := req.Msg
	if m.Target == "" {
		return nil, invalid("target is required")
	}
	if len(m.Files) == 0 {
		return nil, invalid("at least one migration file is required")
	}
	rollout := m.Rollout
	if rollout == "" {
		rollout = controlplane.RolloutDirect
	}
	if _, ok := controlplane.Policies()[rollout]; !ok {
		return nil, invalid("unknown rollout policy " + rollout)
	}
	files := map[string]string{}
	for _, f := range m.Files {
		files[f.Name] = f.Body
	}
	plans, err := controlplane.PlansFromFiles(files, engine.DirectionUp)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if err := s.admit(ctx, m.Target, plans, m.AcknowledgeHazards, m.SkipValidation); err != nil {
		return nil, err
	}

	id := s.newID()
	if err := s.store.CreateRun(ctx, id, m.Target, rollout, files); err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.CreateRunResponse{RunId: id}), nil
}

// RevertRun queues a run applying the down side of an earlier run's migrations.
func (s *Server) RevertRun(ctx context.Context, req *connect.Request[godwitv1.RevertRunRequest]) (*connect.Response[godwitv1.RevertRunResponse], error) {
	m := req.Msg
	orig, err := s.store.Run(ctx, m.RunId)
	if err != nil {
		return nil, rpcErr(err)
	}
	files, err := s.store.RunFiles(ctx, m.RunId)
	if err != nil {
		return nil, rpcErr(err)
	}
	plans, err := controlplane.PlansFromFiles(files, engine.DirectionDown)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if err := s.admit(ctx, orig.Target, plans, m.AcknowledgeHazards, m.SkipValidation); err != nil {
		return nil, err
	}

	id := s.newID()
	if err := s.store.CreateRevert(ctx, id, m.RunId); err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.RevertRunResponse{RunId: id}), nil
}

// admit refuses unacknowledged hazards and plans that fail on the scratch database.
func (s *Server) admit(ctx context.Context, target string, plans []engine.Plan, acked []string, skipValidation bool) error {
	if err := s.checkHazards(plans, acked); err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if s.validator == nil || skipValidation {
		return nil
	}
	if err := s.validator.Validate(ctx, target, plans); err != nil {
		if errors.Is(err, controlplane.ErrValidationFailed) {
			s.Metrics.ValidationFailed(target)

			return connect.NewError(connect.CodeInvalidArgument, err)
		}

		return rpcErr(err)
	}

	return nil
}

func toProto(r controlplane.Run) *godwitv1.Run {
	states := map[string]godwitv1.RunState{
		controlplane.StateQueued:           godwitv1.RunState_RUN_STATE_QUEUED,
		controlplane.StateRunning:          godwitv1.RunState_RUN_STATE_RUNNING,
		controlplane.StateSucceeded:        godwitv1.RunState_RUN_STATE_SUCCEEDED,
		controlplane.StateFailed:           godwitv1.RunState_RUN_STATE_FAILED,
		controlplane.StateNeedsAttention:   godwitv1.RunState_RUN_STATE_NEEDS_ATTENTION,
		controlplane.StateAwaitingContract: godwitv1.RunState_RUN_STATE_AWAITING_CONTRACT,
		controlplane.StateReverted:         godwitv1.RunState_RUN_STATE_REVERTED,
	}
	out := &godwitv1.Run{
		Id:        r.ID,
		Target:    r.Target,
		State:     states[r.State],
		Error:     r.Error,
		Attempts:  int32(r.Attempts),
		Rollout:   r.Rollout,
		Phase:     r.Phase,
		Reverts:   r.Reverts,
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

// settled reports whether a run stopped moving on its own.
func settled(state string) bool {
	switch state {
	case controlplane.StateQueued, controlplane.StateRunning:
		return false
	default:
		return true
	}
}

// WatchRun streams run snapshots until the run settles.
func (s *Server) WatchRun(ctx context.Context, req *connect.Request[godwitv1.WatchRunRequest], stream *connect.ServerStream[godwitv1.WatchRunResponse]) error {
	for {
		r, err := s.store.Run(ctx, req.Msg.RunId)
		if err != nil {
			return rpcErr(err)
		}
		// A send failure surfaces as a ctx error on the next store read.
		_ = stream.Send(&godwitv1.WatchRunResponse{Run: toProto(r)})
		if settled(r.State) {
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
	run, err := s.store.Resume(ctx, req.Msg.RunId)
	if err != nil {
		return nil, rpcErr(err)
	}
	s.Metrics.RunResumed(run.Target)

	return connect.NewResponse(&godwitv1.ResumeRunResponse{}), nil
}

// ParkRun moves a run to needs_attention.
func (s *Server) ParkRun(ctx context.Context, req *connect.Request[godwitv1.ParkRunRequest]) (*connect.Response[godwitv1.ParkRunResponse], error) {
	if err := s.store.Finish(ctx, req.Msg.RunId, controlplane.StateNeedsAttention, req.Msg.Reason); err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.ParkRunResponse{}), nil
}

// ConfirmRollout releases a run's contract phase.
func (s *Server) ConfirmRollout(ctx context.Context, req *connect.Request[godwitv1.ConfirmRolloutRequest]) (*connect.Response[godwitv1.ConfirmRolloutResponse], error) {
	if err := s.store.Confirm(ctx, req.Msg.RunId); err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.ConfirmRolloutResponse{}), nil
}

// checkHazards refuses plans carrying hazard codes the author did not accept.
func (s *Server) checkHazards(plans []engine.Plan, acked []string) error {
	ackSet := map[string]bool{}
	for _, code := range acked {
		ackSet[code] = true
	}
	var pending []string
	for _, p := range plans {
		for _, st := range p.Statements {
			for _, h := range st.Hazards {
				s.Metrics.Hazard(h.Code, ackSet[h.Code])
				if !ackSet[h.Code] {
					pending = append(pending, fmt.Sprintf("%s: %s", h.Code, h.Detail))
				}
			}
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("unacknowledged hazards (pass acknowledge_hazards to accept):\n%s",
			strings.Join(pending, "\n"))
	}

	return nil
}

var errDriftDisabled = connect.NewError(connect.CodeUnimplemented, errors.New("drift detection is not enabled"))

// CheckDrift compares a target's live schema against its baseline now.
func (s *Server) CheckDrift(ctx context.Context, req *connect.Request[godwitv1.CheckDriftRequest]) (*connect.Response[godwitv1.CheckDriftResponse], error) {
	if s.drift == nil {
		return nil, errDriftDisabled
	}
	d, err := s.drift.Check(ctx, req.Msg.Target)
	if err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.CheckDriftResponse{Drifted: d.Drifted, Diff: d.Diff}), nil
}

// ListDriftEvents returns recent drift events, optionally filtered by target.
func (s *Server) ListDriftEvents(ctx context.Context, req *connect.Request[godwitv1.ListDriftEventsRequest]) (*connect.Response[godwitv1.ListDriftEventsResponse], error) {
	events, err := s.store.ListDriftEvents(ctx, req.Msg.Target)
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &godwitv1.ListDriftEventsResponse{}
	for _, e := range events {
		pb := &godwitv1.DriftEvent{
			Id:         e.ID,
			Target:     e.Target,
			Diff:       e.Diff,
			DetectedAt: timestamppb.New(e.DetectedAt),
		}
		if e.ResolvedAt != nil {
			pb.ResolvedAt = timestamppb.New(*e.ResolvedAt)
		}
		resp.Events = append(resp.Events, pb)
	}

	return connect.NewResponse(resp), nil
}

// AcceptBaseline records the live schema as the new expected state.
func (s *Server) AcceptBaseline(ctx context.Context, req *connect.Request[godwitv1.AcceptBaselineRequest]) (*connect.Response[godwitv1.AcceptBaselineResponse], error) {
	if s.drift == nil {
		return nil, errDriftDisabled
	}
	if err := s.drift.AcceptBaseline(ctx, req.Msg.Target); err != nil {
		return nil, rpcErr(err)
	}

	return connect.NewResponse(&godwitv1.AcceptBaselineResponse{}), nil
}
