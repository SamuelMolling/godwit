package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/notify"
)

var errPlanDisabled = connect.NewError(connect.CodeUnimplemented, errors.New("stored plans are not enabled"))

// PlanRun runs CreateRun's admission on the files and returns what the run would do without queueing it;
// with persist the plan is stored together with an observation of the target so a later CreateRun binds to it.
func (s *Server) PlanRun(ctx context.Context, req *connect.Request[godwitv1.PlanRunRequest]) (*connect.Response[godwitv1.PlanRunResponse], error) {
	m := req.Msg
	spec, err := upSpec(m.Target, m.Rollout, m.Files)
	if err != nil {
		return nil, err
	}
	if m.Persist && s.Inspector == nil {
		return nil, errPlanDisabled
	}
	var obs controlplane.Observation
	if m.Persist {
		if obs, err = s.Inspector.Observe(ctx, m.Target); err != nil {
			return nil, rpcErr(err)
		}
	}
	adm, err := s.admit(ctx, m.Target, spec.plans, m.AcknowledgeHazards, m.SkipValidation, m.AllowOutOfOrder, obs.SearchPath)
	if err != nil {
		return nil, err
	}
	migs := controlplane.BuildPlanMigrations(spec.rollout, spec.plans, adm.applied)
	out := &godwitv1.PlanRunResponse{Target: m.Target, Rollout: spec.rollout, Validated: adm.validated, Migrations: migrationsToProto(migs)}
	if !m.Persist {
		s.Log.Info("run planned", "target", m.Target, "rollout", spec.rollout, "files", len(spec.files),
			"acked", m.AcknowledgeHazards, "validated", adm.validated, "allow_out_of_order", m.AllowOutOfOrder)

		return connect.NewResponse(out), nil
	}
	pending, err := controlplane.Pending(migrations(spec.plans), obs.Applied, obs.Repeatables)
	if err != nil {
		return nil, invalid(err.Error())
	}
	p := controlplane.Plan{
		ID: s.newID(), Target: m.Target, Key: controlplane.PlanKey(m.Target, spec.rollout, pending), Rollout: spec.rollout,
		Validated: adm.validated, Acked: m.AcknowledgeHazards, AllowOutOfOrder: m.AllowOutOfOrder,
		CreatedBy: Actor(ctx), Source: m.Source,
	}
	var detected bool
	if p.Migrations, p.Drift, detected = planMigrations(spec, adm, obs); !detected {
		if p.Drift, err = s.driftSince(ctx, m.Target, obs); err != nil {
			return nil, rpcErr(err)
		}
	}
	p = observed(p, obs)
	out.Migrations = migrationsToProto(p.Migrations)
	if err := s.store.SavePlan(ctx, p, spec.files); err != nil {
		return nil, rpcErr(err)
	}
	s.Log.Info("plan stored", "plan", p.ID, "key", p.Key, "target", m.Target, "rollout", spec.rollout, "pending", len(pending),
		"acked", m.AcknowledgeHazards, "validated", adm.validated, "source", m.Source)
	s.audit(ctx, controlplane.AuditPlanCreate, "", m.Target,
		fmt.Sprintf("plan=%s key=%s rollout=%s pending=%d acked=%s source=%s", p.ID, p.Key, spec.rollout, len(pending),
			strings.Join(m.AcknowledgeHazards, ","), m.Source))
	out.PlanId, out.PlanKey, out.Drift, out.Observed = p.ID, p.Key, p.Drift, observationToProto(obs)

	return connect.NewResponse(out), nil
}

func observed(p controlplane.Plan, obs controlplane.Observation) controlplane.Plan {
	p.HistoryHash, p.Applied, p.Repeatables = obs.HistoryHash(), obs.Applied, obs.Repeatables
	p.SchemaFingerprint, p.SchemaDefinition = obs.Fingerprint, obs.Definition
	p.SearchPath = obs.SearchPath

	return p
}

func planMigrations(spec runSpec, adm admission, obs controlplane.Observation) (migs []controlplane.PlanMigration, drift string, detected bool) {
	migs = controlplane.BuildPlanMigrations(spec.rollout, spec.plans, adm.applied)
	if adm.validation == nil {
		return migs, "", false
	}

	return migs, controlplane.Detect(migs, spec.plans, *adm.validation, obs), true
}

func (s *Server) driftSince(ctx context.Context, target string, obs controlplane.Observation) (string, error) {
	snap, err := s.store.SnapshotFor(ctx, target)
	if errors.Is(err, controlplane.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if snap.Fingerprint == obs.Fingerprint {
		return "", nil
	}

	return strings.Join(engine.DiffSchemas(snap.Definition, obs.Definition), "\n"), nil
}

func migrations(plans []engine.Plan) []engine.Migration {
	out := make([]engine.Migration, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Migration)
	}

	return out
}

func migrationsToProto(migs []controlplane.PlanMigration) []*godwitv1.PlannedMigration {
	out := make([]*godwitv1.PlannedMigration, 0, len(migs))
	for _, m := range migs {
		pm := &godwitv1.PlannedMigration{
			Version: m.Version, Name: m.Name, Repeatable: m.Repeatable, Checksum: m.Checksum, Applied: m.Applied,
			Phase: m.Phase, AlreadyApplied: m.AlreadyApplied, Effect: m.Effect, Note: m.Note,
		}
		pm.Statements = statementsToProto(m.Statements)
		out = append(out, pm)
	}

	return out
}

func statementsToProto(sts []controlplane.PlanStatement) []*godwitv1.PlannedStatement {
	out := make([]*godwitv1.PlannedStatement, 0, len(sts))
	for _, st := range sts {
		ps := &godwitv1.PlannedStatement{Sql: st.SQL, NoTx: st.NoTx}
		for _, h := range st.Hazards {
			ps.Hazards = append(ps.Hazards, &godwitv1.PlannedHazard{Code: h.Code, Detail: h.Detail, Recipe: h.Recipe})
		}
		out = append(out, ps)
	}

	return out
}

func observationToProto(obs controlplane.Observation) *godwitv1.PlanObservation {
	out := &godwitv1.PlanObservation{
		HistoryHash: obs.HistoryHash(), SchemaFingerprint: obs.Fingerprint, AppliedCount: int32(len(obs.Applied)),
		At: timestamppb.New(obs.At), SearchPath: obs.SearchPath,
	}
	for _, a := range obs.Applied {
		out.NewestApplied = max(out.NewestApplied, a.Version)
	}

	return out
}

type binding struct {
	planID          string
	searchPath      string
	acked           []string
	allowOutOfOrder bool
	superseded      string
	reattached      string
	adm             *admission
}

func (s *Server) bind(ctx context.Context, m *godwitv1.CreateRunRequest, spec runSpec) (binding, error) {
	b := binding{acked: m.AcknowledgeHazards, allowOutOfOrder: m.AllowOutOfOrder}
	if s.Inspector == nil {
		return b, nil
	}
	obs, err := s.Inspector.Observe(ctx, m.Target)
	if err != nil {
		return b, rpcErr(err)
	}
	b.searchPath = obs.SearchPath
	if run, ok, err := s.reattach(ctx, m, spec, obs); err != nil || ok {
		b.reattached, b.planID = run.ID, run.PlanID

		return b, err
	}
	pending, err := controlplane.Pending(migrations(spec.plans), obs.Applied, obs.Repeatables)
	if err != nil {
		return b, s.refuse(ctx, m.Target, &controlplane.PlanStale{Plan: controlplane.Plan{Target: m.Target}, Reason: controlplane.StaleContent, Hint: err.Error()})
	}
	plan, err := s.lookup(ctx, m, spec, pending)
	if err != nil || plan.ID == "" {
		return b, err
	}
	b.planID = plan.ID
	b.acked = union(plan.Acked, m.AcknowledgeHazards)
	b.allowOutOfOrder = plan.AllowOutOfOrder || m.AllowOutOfOrder
	if plan.HistoryHash == obs.HistoryHash() && plan.SchemaFingerprint == obs.Fingerprint && !plan.PathMoved(obs) {
		return b, nil
	}
	d, err := s.attribute(ctx, plan, obs)
	if err != nil {
		return b, rpcErr(err)
	}
	baseline, err := s.baselineFingerprint(ctx, m.Target)
	if err != nil {
		return b, rpcErr(err)
	}
	if !d.Explained(baseline, obs.Fingerprint) {
		return b, s.refuse(ctx, m.Target, &controlplane.PlanStale{Plan: plan, Reason: d.Reason(), Diff: d, Hint: staleHint(d.Reason(), m.Target)})
	}
	adm, err := s.admit(ctx, m.Target, spec.plans, b.acked, m.SkipValidation, b.allowOutOfOrder, obs.SearchPath)
	if err != nil {
		return b, s.replanFailure(ctx, plan, d, err)
	}
	next := observed(plan, obs)
	next.ID, next.CreatedBy, next.Source, next.Validated = s.newID(), Actor(ctx), m.Source, adm.validated
	migs, drift, detected := planMigrations(spec, adm, obs)
	next.Migrations = migs
	if detected {
		next.Drift = drift
	}
	if !controlplane.SameStatements(plan.Pending(), next.Pending()) {
		return b, s.refuse(ctx, m.Target, &controlplane.PlanStale{
			Plan: plan, Reason: controlplane.StaleHistory, Diff: d, Hint: "statements changed after re-plan; push to the pull request (re-plan)",
		})
	}
	if err := s.store.SupersedePlan(ctx, plan.ID, next, spec.files); err != nil {
		return b, rpcErr(err)
	}
	s.Log.Info("plan superseded", "plan", plan.ID, "by", next.ID, "target", m.Target, "history_added", len(d.Added))
	s.audit(ctx, controlplane.AuditPlanSupersede, "", m.Target, fmt.Sprintf("plan=%s by=%s key=%s", plan.ID, next.ID, plan.Key))
	b.planID, b.superseded, b.adm = next.ID, plan.ID, &adm

	return b, nil
}

func (s *Server) observedSearchPath(ctx context.Context, target string) (string, error) {
	if s.Inspector == nil {
		return "", nil
	}
	obs, err := s.Inspector.Observe(ctx, target)
	if err != nil {
		return "", rpcErr(err)
	}

	return obs.SearchPath, nil
}

func (b binding) detail() string {
	switch {
	case b.superseded != "":
		return fmt.Sprintf("plan %s superseded by %s", notify.ShortID(b.superseded), notify.ShortID(b.planID))
	case b.planID != "":
		return "plan " + notify.ShortID(b.planID)
	default:
		return ""
	}
}

func (s *Server) planSince() time.Time {
	if s.PlanTTL <= 0 {
		return time.Time{}
	}

	return time.Now().Add(-s.PlanTTL)
}

func (s *Server) noPlan(ctx context.Context, target, key string, pending []engine.Migration) error {
	required, err := s.requiresPlan(ctx, target)
	if err != nil {
		return rpcErr(err)
	}
	if !required {
		return nil
	}
	nearest, err := s.store.ListPlans(ctx, target, 3)
	if err != nil {
		return rpcErr(err)
	}

	return s.refuse(ctx, target, &controlplane.PlanRequired{Target: target, Key: key, Pending: pending, Nearest: nearest})
}

func (s *Server) requiresPlan(ctx context.Context, target string) (bool, error) {
	_, config, err := s.store.Target(ctx, target)
	if err != nil {
		return false, err
	}

	return s.RequirePlan || config[controlplane.ConfigRequirePlan] == "true", nil
}

func (s *Server) attribute(ctx context.Context, plan controlplane.Plan, obs controlplane.Observation) (controlplane.PlanDiff, error) {
	d := controlplane.StaleDiff(plan, obs)
	if len(d.Added) == 0 {
		return d, nil
	}
	runs, err := s.store.RunsApplying(ctx, plan.Target, plan.CreatedAt)
	if err != nil {
		return d, err
	}
	for i := range d.Added {
		d.Added[i].RunID = runs[d.Added[i].String()]
	}

	return d, nil
}

func (s *Server) baselineFingerprint(ctx context.Context, target string) (string, error) {
	snap, err := s.store.SnapshotFor(ctx, target)
	if errors.Is(err, controlplane.ErrNotFound) {
		return "", nil
	}

	return snap.Fingerprint, err
}

func (s *Server) replanFailure(ctx context.Context, plan controlplane.Plan, d controlplane.PlanDiff, err error) error {
	stale := &controlplane.PlanStale{Plan: plan, Diff: d}
	switch {
	case errors.Is(err, errOutOfOrder):
		stale.Reason, stale.Hint = controlplane.StaleOrder, "pass allow_out_of_order or renumber the migration above the newest applied version"
	case errors.Is(err, controlplane.ErrValidationFailed):
		stale.Reason, stale.Hint = controlplane.StaleValidation, "the set no longer validates on the target's history: "+errMessage(err)
	default:
		return err
	}

	return s.refuse(ctx, plan.Target, stale)
}

func errMessage(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Message()
	}

	return err.Error()
}

func staleHint(reason, target string) string {
	if reason == controlplane.StaleSchema {
		return fmt.Sprintf("push to the pull request (re-plan) or `godwit drift accept %s` if the schema changes are intended", target)
	}

	return "push to the pull request (re-plan) after checking who changed godwit.migrations on " + target
}

func (s *Server) refuse(ctx context.Context, target string, reason error) error {
	s.Log.Warn("run refused by plan contract", "target", target, "actor", Actor(ctx), "error", reason.Error())
	cerr := connect.NewError(connect.CodeFailedPrecondition, reason)
	if detail, err := connect.NewErrorDetail(planDetail(reason)); err == nil {
		cerr.AddDetail(detail)
	}

	return cerr
}

func planDetail(reason error) proto.Message {
	var stale *controlplane.PlanStale
	if errors.As(reason, &stale) {
		out := &godwitv1.PlanStale{PlanId: stale.Plan.ID, Reason: stale.Reason, SchemaDiff: strings.Join(stale.Diff.Schema, "\n"), Hint: stale.Hint}
		for _, c := range stale.Diff.Added {
			out.HistoryAdded = append(out.HistoryAdded, c.String())
		}
		for _, c := range stale.Diff.Removed {
			out.HistoryRemoved = append(out.HistoryRemoved, c.String())
		}

		return out
	}
	var required *controlplane.PlanRequired
	_ = errors.As(reason, &required)
	out := &godwitv1.PlanRequired{Target: required.Target, Key: required.Key, FilesDiff: required.FilesDiff()}
	for _, p := range required.Nearest {
		out.NearestPlanIds = append(out.NearestPlanIds, p.ID)
	}

	return out
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}

	return out
}
