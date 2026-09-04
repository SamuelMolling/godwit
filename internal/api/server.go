// Package api serves the godwit control-plane API over connect (gRPC + JSON).
package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
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
	"github.com/SamuelMolling/godwit/internal/notify"
)

// DriftOps is the drift surface the API exposes (implemented by the monitor).
type DriftOps interface {
	Check(ctx context.Context, target string) (controlplane.Drift, error)
	AcceptBaseline(ctx context.Context, target string) error
}

// Validator checks migrations before admission.
type Validator interface {
	Validate(ctx context.Context, target string, plans []engine.Plan, searchPath string) (controlplane.Validation, error)
}

// Baseliner marks migrations applied on a target without running them (implemented by the control plane).
type Baseliner interface {
	Baseline(ctx context.Context, runID, target string, migs []engine.Migration, p controlplane.Provenance) error
}

// Inspector reports a target's applied versions, last run and drift baseline, and observes its live history and schema
// (implemented by the control plane).
type Inspector interface {
	Status(ctx context.Context, target string) (controlplane.TargetStatus, error)
	Observe(ctx context.Context, target string) (controlplane.Observation, error)
	DataLoss(ctx context.Context, target string, drops []engine.Drop) ([]engine.Loss, error)
}

// Differ generates the migration between a base schema and a desired DDL (implemented by the control plane).
type Differ interface {
	Diff(ctx context.Context, target, ddl string, base controlplane.DiffBase, files map[string]string) (controlplane.SchemaDiff, error)
}

// Server implements godwit.v1.GodwitService over the control-plane store.
type Server struct {
	// Metrics receives admission and API events; replace it before Handler to share a registry.
	Metrics *metrics.Metrics
	// Log receives admission and operator events plus the access log; replace it before Handler.
	Log *slog.Logger
	// Notifier receives operator-driven run events; replace it before Handler.
	Notifier notify.Notifier
	// Baseliner serves BaselineTarget; nil leaves it unimplemented.
	Baseliner Baseliner
	// Inspector serves GetTargetStatus and stored plans; nil leaves both unimplemented and every run implicit.
	Inspector Inspector
	// Differ serves Diff; nil leaves it unimplemented.
	Differ Differ
	// Checkpointer serves Checkpoint; nil leaves it unimplemented.
	Checkpointer CheckpointGenerator
	// RequirePlan refuses runs without a stored plan on every target, not only those registered with require_plan.
	RequirePlan bool
	// PlanTTL is how long a stored plan stays bindable; zero keeps plans forever.
	PlanTTL time.Duration
	// Limits are the admission bounds; a zero field takes its default. Set them before Handler.
	Limits Limits

	store         *controlplane.Store
	drift         DriftOps
	validator     Validator
	keys          creds.Keyring
	watchInterval time.Duration
	newID         func() string
	ready         func(context.Context) error
}

// NewServer wires a Server; drift and validator are optional (nil disables). An unconfigured keyring
// refuses to register a static target and opens none.
func NewServer(store *controlplane.Store, drift DriftOps, validator Validator, keys creds.Keyring) *Server {
	return &Server{
		Metrics:       metrics.New(),
		Log:           slog.New(slog.DiscardHandler),
		Notifier:      notify.None{},
		store:         store,
		drift:         drift,
		validator:     validator,
		keys:          keys,
		watchInterval: 500 * time.Millisecond,
		newID:         uuid.NewString,
		ready:         store.Ping,
	}
}

// limits are the admission bounds in force, whatever the caller left zero.
func (s *Server) limits() Limits {
	return s.Limits.withDefaults()
}

// Handler mounts the connect service with bearer-token auth, admission limits, plus the unauthenticated
// /metrics, /healthz and /readyz endpoints; serve it with h2c enabled.
func Handler(s *Server, tokens []Token) http.Handler {
	mux := http.NewServeMux()
	a := newAuth(tokens)
	l := s.limits()
	path, h := godwitv1connect.NewGodwitServiceHandler(s,
		connect.WithReadMaxBytes(l.RequestBytes),
		connect.WithInterceptors(s.Metrics.Interceptor(), accessLog{log: s.Log, actor: a.actor}, a, newGate(l)))
	mux.Handle(path, h)
	mux.Handle("/metrics", s.Metrics.Handler())
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("GET /readyz", readyz(s.ready))

	return mux
}

func rpcErr(err error) *connect.Error {
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, controlplane.ErrNotResumable), errors.Is(err, controlplane.ErrNotAwaitingContract),
		errors.Is(err, controlplane.ErrNotRevertable), errors.Is(err, controlplane.ErrBaselineRun),
		errors.Is(err, engine.ErrAlreadyMigrated), errors.Is(err, controlplane.ErrAppliedContent):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		return connect.NewError(connect.CodeInternal, safe(err))
	}
}

var errOutOfOrder = errors.New("out-of-order migrations")

func invalid(msg string) *connect.Error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

func timeouts(lock, statement string) (controlplane.Timeouts, error) {
	t := controlplane.Timeouts{Lock: lock, Statement: statement}
	if _, err := t.Options(); err != nil {
		return controlplane.Timeouts{}, invalid(err.Error())
	}

	return t, nil
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
		if !s.keys.Configured() {
			return nil, invalid("static provider needs a key: set GODWIT_MASTER_KEY, or GODWIT_KEY_PROVIDER with GODWIT_KMS_KEY")
		}
		enc, err := s.keys.Seal(ctx, m.Dsn)
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
	t, err := timeouts(m.LockTimeout, m.StatementTimeout)
	if err != nil {
		return nil, err
	}
	if t.Lock != "" {
		config[controlplane.ConfigLockTimeout] = t.Lock
	}
	if t.Statement != "" {
		config[controlplane.ConfigStatementTimeout] = t.Statement
	}
	if m.RequirePlan {
		config[controlplane.ConfigRequirePlan] = "true"
	}
	if m.KeepOld != nil {
		config[controlplane.ConfigKeepOld] = strconv.FormatBool(*m.KeepOld)
	}
	searchPath, err := controlplane.ParseSearchPath(m.SearchPath)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if searchPath != "" {
		config[controlplane.ConfigSearchPath] = searchPath
	}
	if err := s.store.RegisterTarget(ctx, m.Name, m.Provider, config); err != nil {
		return nil, rpcErr(err)
	}
	s.Log.Info("target registered", "target", m.Name, "provider", m.Provider,
		"lock_timeout", t.Lock, "statement_timeout", t.Statement, "require_plan", m.RequirePlan, "search_path", searchPath)
	s.audit(ctx, controlplane.AuditTargetRegister, "", m.Name,
		fmt.Sprintf("provider=%s lock_timeout=%s statement_timeout=%s require_plan=%t search_path=%s",
			m.Provider, t.Lock, t.Statement, m.RequirePlan, searchPath))

	return connect.NewResponse(&godwitv1.RegisterTargetResponse{}), nil
}

// CreateRun validates and queues a run.
func (s *Server) CreateRun(ctx context.Context, req *connect.Request[godwitv1.CreateRunRequest]) (*connect.Response[godwitv1.CreateRunResponse], error) {
	m := req.Msg
	if m.PlanId != "" && m.ToVersion > 0 {
		return nil, invalid("to_version cannot be combined with plan_id: the stored plan already fixes the set it covers")
	}
	if m.PlanId != "" {
		if err := s.explicitPlan(ctx, m); err != nil {
			return nil, err
		}
	}
	spec, err := s.upSpec(m.Target, m.Rollout, m.Files)
	if err != nil {
		return nil, err
	}
	if spec, err = s.stopAt(ctx, m.Target, spec, m.ToVersion); err != nil {
		return nil, err
	}
	t, err := timeouts(m.LockTimeout, m.StatementTimeout)
	if err != nil {
		return nil, err
	}
	b, err := s.bind(ctx, m, spec)
	if err != nil {
		return nil, err
	}
	if b.reattached != "" {
		return connect.NewResponse(&godwitv1.CreateRunResponse{RunId: b.reattached, PlanId: b.planID, Reattached: true}), nil
	}
	if b.adm == nil {
		adm, err := s.admit(ctx, m.Target, spec.plans, b.acked, m.SkipValidation, b.allowOutOfOrder, b.searchPath)
		if err != nil {
			return nil, err
		}
		b.adm = &adm
	}
	if b.expansions == nil {
		b.expansions = b.adm.expansions
	}
	if err := checkRollout(spec.rollout, b.adm.expanded(spec)); err != nil {
		return nil, err
	}

	id := s.newID()
	p := controlplane.Provenance{CreatedBy: Actor(ctx), Source: m.Source}
	_, err = s.queue(ctx, notify.RunCreated, b.detail(), func(tx *controlplane.Store) (controlplane.Run, error) {
		if err := tx.CreateRun(ctx, id, m.Target, spec.rollout, spec.files, t, p, b.planID, b.expansions); err != nil {
			return controlplane.Run{}, err
		}
		if b.planID != "" {
			if err := tx.BindPlan(ctx, b.planID, id); err != nil {
				return controlplane.Run{}, err
			}
		}

		return controlplane.Run{
			ID: id, Target: m.Target, State: controlplane.StateQueued, PlanID: b.planID,
			Rollout: spec.rollout, Phase: controlplane.PhaseExpand, Timeouts: t, Provenance: p,
		}, nil
	})
	if err != nil {
		return nil, rpcErr(err)
	}
	s.Log.Info("run created", "run", id, "target", m.Target, "rollout", spec.rollout, "source", m.Source, "plan", b.planID,
		"files", len(spec.files), "acked", b.acked, "lock_timeout", t.Lock, "statement_timeout", t.Statement,
		"allow_out_of_order", b.allowOutOfOrder, "to_version", m.ToVersion, "withheld", len(spec.withheld))
	s.audit(ctx, controlplane.AuditRunCreate, id, m.Target,
		join(fmt.Sprintf("rollout=%s migrations=%d acked=%s source=%s plan=%s", spec.rollout, len(spec.plans),
			strings.Join(b.acked, ","), m.Source, b.planID), b.expanded()))

	return connect.NewResponse(&godwitv1.CreateRunResponse{RunId: id, PlanId: b.planID}), nil
}

type runSpec struct {
	rollout string
	files   map[string]string
	plans   []engine.Plan
	// withheld is what a version target kept out of plans and files: reported, never run.
	withheld []engine.Plan
}

func (s *Server) upSpec(target, rollout string, in []*godwitv1.MigrationFile) (runSpec, error) {
	if target == "" {
		return runSpec{}, invalid("target is required")
	}
	if len(in) == 0 {
		return runSpec{}, invalid("at least one migration file is required")
	}
	if err := s.limits().checkFiles(in); err != nil {
		return runSpec{}, err
	}
	if rollout == "" {
		rollout = controlplane.RolloutDirect
	}
	if _, ok := controlplane.Policies()[rollout]; !ok {
		return runSpec{}, invalid("unknown rollout policy " + rollout)
	}
	files := map[string]string{}
	for _, f := range in {
		files[f.Name] = f.Body
	}
	plans, err := controlplane.PlansFromFiles(files, engine.DirectionUp)
	if err != nil {
		return runSpec{}, invalid(err.Error())
	}

	return runSpec{rollout: rollout, files: files, plans: plans}, nil
}

// ErrDataLoss marks a revert plan that would drop a table or column the target still has rows in.
var ErrDataLoss = errors.New("revert would destroy data")

// RevertRun plans, and unless dry_run queues, the down side of what an earlier run actually applied.
// With no run_id it acts on the newest un-reverted run of target, and never on anything wider.
func (s *Server) RevertRun(ctx context.Context, req *connect.Request[godwitv1.RevertRunRequest]) (*connect.Response[godwitv1.RevertRunResponse], error) {
	m := req.Msg
	t, err := timeouts(m.LockTimeout, m.StatementTimeout)
	if err != nil {
		return nil, err
	}
	rp, err := s.revertPlan(ctx, m)
	if err != nil {
		return nil, err
	}
	orig := rp.Run
	loss, err := s.dataLoss(ctx, rp, m.AllowDataLoss)
	if err != nil {
		return nil, err
	}
	searchPath, err := s.observedSearchPath(ctx, orig.Target)
	if err != nil {
		return nil, err
	}
	adm, err := s.admit(ctx, orig.Target, rp.Plans, m.AcknowledgeHazards, m.SkipValidation, true, searchPath)
	if err != nil {
		return nil, err
	}
	out := &godwitv1.RevertRunResponse{
		Reverts: orig.ID, Target: orig.Target, DataLoss: lossToProto(loss), Forced: rp.Newer,
		Migrations: migrationsToProto(controlplane.BuildPlanMigrations(controlplane.RolloutDirect, rp.Plans, adm.applied, nil)),
	}
	if m.DryRun {
		s.Log.Info("revert planned", "target", orig.Target, "reverts", orig.ID, "migrations", len(rp.Plans), "data_loss", len(loss))

		return connect.NewResponse(out), nil
	}

	id := s.newID()
	p := controlplane.Provenance{CreatedBy: Actor(ctx)}
	_, err = s.queue(ctx, notify.RunCreated, "reverts run "+notify.ShortID(orig.ID), func(tx *controlplane.Store) (controlplane.Run, error) {
		if err := tx.CreateRevert(ctx, id, orig, m.Force, t, p); err != nil {
			return controlplane.Run{}, err
		}

		return controlplane.Run{ID: id, Target: orig.Target, State: controlplane.StateQueued, Reverts: orig.ID, Timeouts: t, Provenance: p}, nil
	})
	if err != nil {
		return nil, rpcErr(err)
	}
	s.Log.Info("revert created", "run", id, "target", orig.Target, "reverts", orig.ID, "acked", m.AcknowledgeHazards,
		"migrations", len(rp.Plans), "forced", rp.Newer, "data_loss", len(loss),
		"lock_timeout", t.Lock, "statement_timeout", t.Statement)
	s.audit(ctx, controlplane.AuditRunRevert, id, orig.Target,
		fmt.Sprintf("reverts=%s migrations=%d acked=%s forced=%t allow_data_loss=%t",
			orig.ID, len(rp.Plans), strings.Join(m.AcknowledgeHazards, ","), rp.Newer, m.AllowDataLoss))
	out.RunId = id

	return connect.NewResponse(out), nil
}

// revertPlan resolves which run the request means and builds the down side of what it applied.
func (s *Server) revertPlan(ctx context.Context, m *godwitv1.RevertRunRequest) (controlplane.RevertPlan, error) {
	id := m.RunId
	if id == "" {
		if m.Target == "" {
			return controlplane.RevertPlan{}, invalid("run_id or target is required")
		}
		run, err := s.store.RevertTarget(ctx, m.Target)
		if err != nil {
			return controlplane.RevertPlan{}, rpcErr(err)
		}
		id = run.ID
	}
	rp, err := s.store.PlanRevert(ctx, id)
	if errors.Is(err, controlplane.ErrRevertPlan) {
		return controlplane.RevertPlan{}, invalid(err.Error())
	}
	if err != nil {
		return controlplane.RevertPlan{}, rpcErr(err)
	}
	if rp.Run.Kind == controlplane.KindBaseline {
		return controlplane.RevertPlan{}, rpcErr(fmt.Errorf("run %q: %w", id, controlplane.ErrBaselineRun))
	}
	if m.Target != "" && m.Target != rp.Run.Target {
		return controlplane.RevertPlan{}, invalid(fmt.Sprintf("run %s is on target %s, not %s", id, rp.Run.Target, m.Target))
	}

	return rp, nil
}

// dataLoss refuses a plan that drops a table or column the target still has rows in, unless allowed.
func (s *Server) dataLoss(ctx context.Context, rp controlplane.RevertPlan, allow bool) (map[string][]engine.Loss, error) {
	if s.Inspector == nil {
		return nil, nil
	}
	out := map[string][]engine.Loss{}
	total := 0
	for migration, drops := range rp.Drops() {
		losses, err := s.Inspector.DataLoss(ctx, rp.Run.Target, drops)
		if err != nil {
			return nil, rpcErr(err)
		}
		if len(losses) > 0 {
			out[migration], total = losses, total+len(losses)
		}
	}
	if total == 0 || allow {
		return out, nil
	}
	s.Log.Warn("revert refused by the data-loss gate", "target", rp.Run.Target, "reverts", rp.Run.ID, "objects", total)

	return nil, connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("%w: %s; pass allow_data_loss (--allow-data-loss) to run it anyway", ErrDataLoss, lossDetail(out)))
}

func lossDetail(losses map[string][]engine.Loss) string {
	var out []string
	for _, migration := range slices.Sorted(maps.Keys(losses)) {
		for _, l := range losses[migration] {
			out = append(out, migration+" drops "+l.String())
		}
	}

	return strings.Join(out, ", ")
}

func lossToProto(losses map[string][]engine.Loss) []*godwitv1.DataLoss {
	var out []*godwitv1.DataLoss
	for _, migration := range slices.Sorted(maps.Keys(losses)) {
		for _, l := range losses[migration] {
			out = append(out, &godwitv1.DataLoss{Migration: migration, Kind: l.Kind(), Object: l.Drop.String(), Rows: l.Rows})
		}
	}

	return out
}

func (s *Server) emit(ctx context.Context, run controlplane.Run, typ, detail string) {
	e := controlplane.RunEvent(run, typ, detail)
	e.Actor = Actor(ctx)
	notify.Emit(ctx, s.Notifier, s.Log, e)
}

// queue commits mutate and emits typ for the run it returns; the scheduler cannot claim the run before the event is out.
func (s *Server) queue(ctx context.Context, typ, detail string, mutate func(tx *controlplane.Store) (controlplane.Run, error)) (controlplane.Run, error) {
	var run controlplane.Run
	err := s.store.Transact(ctx, func(tx *controlplane.Store) error {
		r, err := mutate(tx)
		if err != nil {
			return err
		}
		run = r
		s.emit(ctx, r, typ, detail)

		return nil
	})

	return run, err
}

// record audits an operator action on a run and notifies with the run's current state; the audit survives a failed lookup.
func (s *Server) record(ctx context.Context, id, action, typ, detail string) {
	run, err := s.store.Run(ctx, id)
	if err != nil {
		s.Log.Warn("notification skipped", "run", id, "type", typ, "error", err)
	} else {
		s.emit(ctx, run, typ, detail)
	}
	s.audit(ctx, action, id, run.Target, detail)
}

const auditDetailLimit = 500

// audit writes an entry for a mutation that already happened; a store failure is logged, not returned.
func (s *Server) audit(ctx context.Context, action, runID, target, detail string) {
	if r := []rune(detail); len(r) > auditDetailLimit {
		detail = string(r[:auditDetailLimit]) + "…"
	}
	e := controlplane.AuditEntry{Actor: Actor(ctx), Action: action, RunID: runID, Target: target, Detail: detail}
	if err := s.store.Audit(ctx, e); err != nil {
		s.Log.Error("audit write failed", "actor", e.Actor, "action", action, "run", runID, "target", target, "error", err)
	}
}

type admission struct {
	applied    controlplane.AppliedSet
	validated  bool
	validation *controlplane.Validation
	// plans is the admitted set with every directive migration replaced by its expansion.
	plans      []engine.Plan
	expansions map[string]controlplane.Expansion
}

// expanded is what admission decided to run; it falls back to the submitted plans when a validator
// stub reported none.
func (a admission) expanded(spec runSpec) []engine.Plan {
	if a.plans != nil {
		return a.plans
	}

	return spec.plans
}

// admit refuses unacknowledged hazards, out-of-order versions and plans that fail on the scratch database.
func (s *Server) admit(ctx context.Context, target string, plans []engine.Plan, acked []string, skipValidation, allowOutOfOrder bool, searchPath string) (admission, error) {
	if err := s.checkHazards(plans, acked); err != nil {
		s.Log.Warn("run refused by hazard gate", "target", target, "error", err.Error())

		return admission{}, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if _, _, err := s.store.Target(ctx, target); err != nil {
		return admission{}, rpcErr(err)
	}
	applied, err := s.store.Applied(ctx, target)
	if err != nil {
		return admission{}, rpcErr(err)
	}
	if err := s.checkOrder(target, plans, applied.Versions, allowOutOfOrder); err != nil {
		return admission{}, err
	}
	plans, err = engine.ShapeCheckpoint(plans, applied.Newest())
	if err != nil {
		s.Log.Warn("run refused by the checkpoint gate", "target", target, "error", err.Error())

		return admission{}, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	adm := admission{applied: applied, plans: plans}
	if s.validator == nil || skipValidation {
		if id := directiveID(plans, applied); id != "" {
			return admission{}, invalid(id + " carries a godwit directive: directives need validation, so drop --skip-validation")
		}

		return adm, nil
	}
	val, err := s.validator.Validate(ctx, target, plans, searchPath)
	if err != nil {
		if errors.Is(err, controlplane.ErrValidationFailed) || errors.Is(err, controlplane.ErrDirective) {
			s.Metrics.ValidationFailed(target)
			s.Log.Warn("run refused by validation", "target", target, "error", err.Error())

			return admission{}, connect.NewError(connect.CodeInvalidArgument, err)
		}

		return admission{}, rpcErr(err)
	}
	adm.validated, adm.validation = true, &val
	adm.expansions = val.Expansions
	if val.Plans != nil {
		adm.plans = val.Plans
	}

	return adm, nil
}

// directiveID names a directive migration still to apply; one the target holds is never expanded again.
func directiveID(plans []engine.Plan, applied controlplane.AppliedSet) string {
	for _, p := range plans {
		if len(p.Migration.Directives) > 0 && !applied.Has(p.Migration) {
			return p.Migration.ID()
		}
	}

	return ""
}

// checkRollout refuses a directive that splits into two phases under a rollout that runs everything at
// once: the rollout is part of the plan key, so godwit will not silently upgrade it.
func checkRollout(rollout string, plans []engine.Plan) error {
	if rollout != controlplane.RolloutDirect {
		return nil
	}
	for _, p := range plans {
		for _, st := range p.Statements {
			if st.Phase == engine.PhaseContract {
				return connect.NewError(connect.CodeFailedPrecondition,
					fmt.Errorf("%s expands into expand and contract phases; use rollout: expand-contract", p.Migration.ID()))
			}
		}
	}

	return nil
}

// checkIdle refuses to plan or run against a target parked between the phases of an earlier run: its
// schema matches no recorded state, so nothing can be validated against it.
func (s *Server) checkIdle(ctx context.Context, target string) error {
	run, ok, err := s.store.AwaitingContract(ctx, target)
	if err != nil {
		return rpcErr(err)
	}
	if !ok {
		return nil
	}

	return connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("target %s has run %s awaiting contract; confirm or revert it first", target, run.ID))
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
		Kind:      r.Kind,
		CreatedBy: r.Provenance.CreatedBy,
		Source:    r.Provenance.Source,
		PlanId:    r.PlanID,
		Retries:   int32(r.Retries),
		CreatedAt: timestamppb.New(r.CreatedAt),

		LockTimeout: r.Timeouts.Lock, StatementTimeout: r.Timeouts.Statement,
	}
	if r.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*r.FinishedAt)
	}
	if r.NotBefore != nil {
		out.NotBefore = timestamppb.New(*r.NotBefore)
	}
	if p := r.Progress; p != nil {
		out.Progress = &godwitv1.RunProgress{
			Migration: p.Migration, Statement: int32(p.Statement), Phase: p.Phase,
			RowsDone: p.RowsDone, RowsTotal: p.RowsTotal, Batches: int32(p.Batches),
		}
	}

	return out
}

// GetRun returns one run and the ledger of what it applied.
func (s *Server) GetRun(ctx context.Context, req *connect.Request[godwitv1.GetRunRequest]) (*connect.Response[godwitv1.GetRunResponse], error) {
	r, err := s.store.Run(ctx, req.Msg.RunId)
	if err != nil {
		return nil, rpcErr(err)
	}
	applied, err := s.store.AppliedMigrations(ctx, r.ID)
	if err != nil {
		return nil, rpcErr(err)
	}
	out := &godwitv1.GetRunResponse{Run: toProto(r), Applied: appliedToProto(applied)}
	if req.Msg.IncludeFiles {
		files, err := s.store.RunFiles(ctx, r.ID)
		if err != nil {
			return nil, rpcErr(err)
		}
		out.Files = filesToProto(files)
	}

	return connect.NewResponse(out), nil
}

func filesToProto(files map[string]string) []*godwitv1.MigrationFile {
	out := make([]*godwitv1.MigrationFile, 0, len(files))
	for name, body := range files {
		out = append(out, &godwitv1.MigrationFile{Name: name, Body: body})
	}
	slices.SortFunc(out, func(a, b *godwitv1.MigrationFile) int { return cmp.Compare(a.Name, b.Name) })

	return out
}

func appliedToProto(applied []controlplane.RunMigration) []*godwitv1.RunMigration {
	out := make([]*godwitv1.RunMigration, 0, len(applied))
	for _, m := range applied {
		out = append(out, &godwitv1.RunMigration{
			Migration: m.Migration, AppliedAt: timestamppb.New(m.AppliedAt), RevertedBy: m.RevertedBy, Held: m.Held,
		})
	}

	return out
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
	run, err := s.queue(ctx, notify.RunResumed, "", func(tx *controlplane.Store) (controlplane.Run, error) {
		return tx.Resume(ctx, req.Msg.RunId)
	})
	if err != nil {
		return nil, rpcErr(err)
	}
	s.Metrics.RunResumed(run.Target)
	s.Log.Info("run resumed", "run", run.ID, "target", run.Target)
	s.audit(ctx, controlplane.AuditRunResume, run.ID, run.Target, "")

	return connect.NewResponse(&godwitv1.ResumeRunResponse{}), nil
}

// ParkRun moves a run to needs_attention.
func (s *Server) ParkRun(ctx context.Context, req *connect.Request[godwitv1.ParkRunRequest]) (*connect.Response[godwitv1.ParkRunResponse], error) {
	if err := s.store.Finish(ctx, req.Msg.RunId, controlplane.StateNeedsAttention, req.Msg.Reason); err != nil {
		return nil, rpcErr(err)
	}
	s.Log.Info("run parked", "run", req.Msg.RunId, "reason", req.Msg.Reason)
	s.record(ctx, req.Msg.RunId, controlplane.AuditRunPark, notify.RunParked, req.Msg.Reason)

	return connect.NewResponse(&godwitv1.ParkRunResponse{}), nil
}

// ConfirmRollout releases a run's contract phase.
func (s *Server) ConfirmRollout(ctx context.Context, req *connect.Request[godwitv1.ConfirmRolloutRequest]) (*connect.Response[godwitv1.ConfirmRolloutResponse], error) {
	run, err := s.queue(ctx, notify.RunConfirmed, "", func(tx *controlplane.Store) (controlplane.Run, error) {
		return tx.Confirm(ctx, req.Msg.RunId)
	})
	if err != nil {
		return nil, rpcErr(err)
	}
	s.Log.Info("rollout confirmed", "run", run.ID)
	s.audit(ctx, controlplane.AuditRunConfirm, run.ID, run.Target, "")

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

// checkOrder refuses pending versions older than the newest one applied on the target unless allowed, in which case it
// logs them; repeatables carry no version and are never out of order.
func (s *Server) checkOrder(target string, plans []engine.Plan, applied []int64, allow bool) error {
	if len(applied) == 0 {
		return nil
	}
	latest := applied[len(applied)-1]
	var behind []string
	for _, p := range plans {
		v := p.Migration.Version
		if !p.Migration.Repeatable && v < latest && !slices.Contains(applied, v) {
			behind = append(behind, strconv.FormatInt(v, 10))
		}
	}
	if len(behind) == 0 {
		return nil
	}
	if allow {
		s.Log.Warn("out-of-order migrations admitted", "target", target, "versions", behind, "latest_applied", latest)

		return nil
	}
	err := fmt.Errorf("%w %s: newest applied version on %s is %d (pass allow_out_of_order to apply them anyway)",
		errOutOfOrder, strings.Join(behind, ", "), target, latest)
	s.Log.Warn("run refused by order guard", "target", target, "error", err.Error())

	return connect.NewError(connect.CodeFailedPrecondition, err)
}

var (
	errDriftDisabled    = connect.NewError(connect.CodeUnimplemented, errors.New("drift detection is not enabled"))
	errBaselineDisabled = connect.NewError(connect.CodeUnimplemented, errors.New("baselining is not enabled"))
)

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
	s.audit(ctx, controlplane.AuditDriftAccept, "", req.Msg.Target, "")

	return connect.NewResponse(&godwitv1.AcceptBaselineResponse{}), nil
}

// BaselineTarget marks every migration up to a version as applied on a target without running it.
func (s *Server) BaselineTarget(ctx context.Context, req *connect.Request[godwitv1.BaselineTargetRequest]) (*connect.Response[godwitv1.BaselineTargetResponse], error) {
	if s.Baseliner == nil {
		return nil, errBaselineDisabled
	}
	m := req.Msg
	if m.Target == "" {
		return nil, invalid("target is required")
	}
	if m.Version <= 0 {
		return nil, invalid("version must be positive")
	}
	files := map[string]string{}
	for _, f := range m.Files {
		files[f.Name] = f.Body
	}
	plans, err := controlplane.PlansFromFiles(files, engine.DirectionUp)
	if err != nil {
		return nil, invalid(err.Error())
	}
	var migs []engine.Migration
	for _, p := range plans {
		if p.Migration.Version <= m.Version {
			migs = append(migs, p.Migration)
		}
	}
	if len(migs) == 0 {
		return nil, invalid(fmt.Sprintf("no migration at or below version %d", m.Version))
	}

	id := s.newID()
	p := controlplane.Provenance{CreatedBy: Actor(ctx)}
	if err := s.Baseliner.Baseline(ctx, id, m.Target, migs, p); err != nil {
		return nil, rpcErr(err)
	}
	detail := fmt.Sprintf("baseline to version %d: %d migrations marked applied", m.Version, len(migs))
	s.Log.Info("target baselined", "run", id, "target", m.Target, "version", m.Version, "migrations", len(migs))
	s.audit(ctx, controlplane.AuditTargetBaseline, id, m.Target, fmt.Sprintf("version=%d migrations=%d", m.Version, len(migs)))
	s.emit(ctx, controlplane.Run{
		ID: id, Target: m.Target, State: controlplane.StateSucceeded,
		Rollout: controlplane.RolloutDirect, Phase: controlplane.PhaseExpand, Kind: controlplane.KindBaseline, Provenance: p,
	}, notify.RunSucceeded, detail)

	return connect.NewResponse(&godwitv1.BaselineTargetResponse{RunId: id}), nil
}

// ListAudit returns the newest audit entries, optionally filtered by target and run.
func (s *Server) ListAudit(ctx context.Context, req *connect.Request[godwitv1.ListAuditRequest]) (*connect.Response[godwitv1.ListAuditResponse], error) {
	entries, err := s.store.ListAudit(ctx, req.Msg.Target, req.Msg.RunId, int(req.Msg.Limit))
	if err != nil {
		return nil, rpcErr(err)
	}
	resp := &godwitv1.ListAuditResponse{}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &godwitv1.AuditEntry{
			Id:     e.ID,
			At:     timestamppb.New(e.At),
			Actor:  e.Actor,
			Action: e.Action,
			RunId:  e.RunID,
			Target: e.Target,
			Detail: e.Detail,
		})
	}

	return connect.NewResponse(resp), nil
}
