package admission

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SamuelMolling/godwit/internal/authz"
	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/notify"
)

// Binding names the stored plan a run binds to and what Bind had to do to reach it.
type Binding struct {
	PlanID          string
	Expansions      map[string]controlplane.Expansion
	SearchPath      string
	Acked           []string
	AllowOutOfOrder bool
	// Superseded is the plan a re-plan replaced; its ID is empty when Bind bound one unchanged.
	Superseded controlplane.Plan
	Reattach   *Reattach
	Admitted   *Admitted
}

// Reattach is the run a repeated submission joins instead of queueing a duplicate.
type Reattach struct {
	Run    controlplane.Run
	Plan   controlplane.Plan
	Resume bool
}

// Bind resolves the stored plan a run may bind to, re-planning it when the target moved under it and
// refusing when what moved cannot be explained.
func (g Gate) Bind(ctx context.Context, req Request, set Set) (Binding, error) {
	b := Binding{Acked: req.Acked, AllowOutOfOrder: req.AllowOutOfOrder}
	if g.Observer == nil {
		return b, nil
	}
	obs, err := g.Observer.Observe(ctx, req.Target)
	if err != nil {
		return b, err
	}
	b.SearchPath = obs.SearchPath
	if err := g.CheckIdle(ctx, req.Target); err != nil {
		return b, err
	}
	r, err := g.reattach(ctx, req, set, obs)
	if err != nil {
		return b, err
	}
	if r != nil {
		b.Reattach, b.PlanID = r, r.Run.PlanID

		return b, nil
	}
	pending, err := controlplane.Pending(migrations(set.Plans), obs.Applied, obs.Repeatables)
	if err != nil {
		return b, g.refuse(ctx, req.Target, &controlplane.PlanStale{Plan: controlplane.Plan{Target: req.Target}, Reason: controlplane.StaleContent, Hint: err.Error()})
	}
	plan, err := g.lookup(ctx, req, set, pending)
	if err != nil {
		return b, err
	}
	// A stored plan accounts for the target's history itself: what it cannot explain is refused as stale below.
	if plan.ID == "" {
		return b, g.CheckReconciled(ctx, req.Target, obs)
	}
	b.PlanID, b.Expansions = plan.ID, plan.Expansions
	b.Acked = union(plan.Acked, req.Acked)
	b.AllowOutOfOrder = plan.AllowOutOfOrder || req.AllowOutOfOrder
	if plan.HistoryHash == obs.HistoryHash() && plan.SchemaFingerprint == obs.Fingerprint && !plan.PathMoved(obs) {
		return b, nil
	}

	return g.replan(ctx, req, set, b, plan, obs)
}

func (g Gate) replan(ctx context.Context, req Request, set Set, b Binding, plan controlplane.Plan, obs controlplane.Observation) (Binding, error) {
	d, err := g.attribute(ctx, plan, obs)
	if err != nil {
		return b, err
	}
	baseline, err := g.baselineFingerprint(ctx, req.Target)
	if err != nil {
		return b, err
	}
	if !d.Explained(baseline, obs.Fingerprint) {
		return b, g.refuse(ctx, req.Target, &controlplane.PlanStale{Plan: plan, Reason: d.Reason(), Diff: d, Hint: staleHint(d.Reason(), req.Target)})
	}
	adm, err := g.Admit(ctx, req.Target, set.Plans, b.Acked, req.SkipValidation, b.AllowOutOfOrder, obs.SearchPath)
	if err != nil {
		return b, g.replanFailure(ctx, plan, d, err)
	}
	next := observed(plan, obs)
	next.ID, next.CreatedBy, next.Source, next.Validated = g.NewID(), authz.Actor(ctx), req.Source, adm.Validated
	next.Expansions = adm.Expansions
	migs, drift, detected := planMigrations(set, adm, obs)
	next.Migrations = migs
	if detected {
		next.Drift = drift
	}
	if !controlplane.SameStatements(plan.Pending(), next.Pending()) {
		return b, g.refuse(ctx, req.Target, &controlplane.PlanStale{
			Plan: plan, Reason: controlplane.StaleHistory, Diff: d, Hint: "statements changed after re-plan; push to the pull request (re-plan)",
		})
	}
	if err := g.Store.SupersedePlan(ctx, plan.ID, next, set.Files); err != nil {
		return b, err
	}
	g.Log.Info("plan superseded", "plan", plan.ID, "by", next.ID, "target", req.Target, "history_added", len(d.Added))
	b.PlanID, b.Superseded, b.Admitted, b.Expansions = next.ID, plan, &adm, adm.Expansions

	return b, nil
}

// Save records a plan a later run binds to together with an observation of the target, and reports how
// many migrations it found pending.
func (g Gate) Save(ctx context.Context, req Request, set Set, adm Admitted, obs controlplane.Observation) (controlplane.Plan, int, error) {
	pending, err := controlplane.Pending(migrations(set.Plans), obs.Applied, obs.Repeatables)
	if err != nil {
		return controlplane.Plan{}, 0, invalid(err)
	}
	p := controlplane.Plan{
		ID: g.NewID(), Target: req.Target, Key: controlplane.PlanKey(req.Target, set.Rollout, pending), Rollout: set.Rollout,
		Validated: adm.Validated, Acked: req.Acked, AllowOutOfOrder: req.AllowOutOfOrder,
		CreatedBy: authz.Actor(ctx), Source: req.Source, Expansions: adm.Expansions,
	}
	var detected bool
	if p.Migrations, p.Drift, detected = planMigrations(set, adm, obs); !detected {
		if p.Drift, err = g.driftSince(ctx, req.Target, obs); err != nil {
			return controlplane.Plan{}, 0, err
		}
	}
	p = observed(p, obs)
	if err := g.Store.SavePlan(ctx, p, set.Files); err != nil {
		return controlplane.Plan{}, 0, err
	}
	g.Log.Info("plan stored", "plan", p.ID, "key", p.Key, "target", req.Target, "rollout", set.Rollout, "pending", len(pending),
		"acked", req.Acked, "validated", adm.Validated, "source", req.Source)

	return p, len(pending), nil
}

func observed(p controlplane.Plan, obs controlplane.Observation) controlplane.Plan {
	p.HistoryHash, p.Applied, p.Repeatables = obs.HistoryHash(), obs.Applied, obs.Repeatables
	p.SchemaFingerprint, p.SchemaDefinition = obs.Fingerprint, obs.Definition
	p.SearchPath = obs.SearchPath

	return p
}

func planMigrations(set Set, adm Admitted, obs controlplane.Observation) (migs []controlplane.PlanMigration, drift string, detected bool) {
	plans := adm.Expanded(set)
	migs = controlplane.BuildPlanMigrations(set.Rollout, plans, adm.Applied, adm.Expansions)
	controlplane.AttachChanges(migs, adm.Validation)
	if adm.Validation == nil {
		return set.withWithheld(migs, adm.Applied), "", false
	}
	drift = controlplane.Detect(migs, plans, *adm.Validation, obs)

	return set.withWithheld(migs, adm.Applied), drift, true
}

func (g Gate) driftSince(ctx context.Context, target string, obs controlplane.Observation) (string, error) {
	snap, err := g.Store.SnapshotFor(ctx, target)
	if errors.Is(err, controlplane.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if snap.Fingerprint == obs.Fingerprint || !engine.SameFormat(snap.Definition) {
		return "", nil
	}

	return strings.Join(engine.DiffSchemas(snap.Definition, obs.Definition), "\n"), nil
}

var errUnreconciled = errors.New("target records migrations the ledger does not")

// CheckReconciled refuses to plan against a target whose own journal is ahead of the control plane's
// ledger. The journal is what says a migration is applied; the ledger is the control plane's copy of it,
// and it is what the order guard and the scratch replay read. Planning over a ledger that cannot see an
// out-of-band apply plans against a history the target does not have.
func (g Gate) CheckReconciled(ctx context.Context, target string, obs controlplane.Observation) error {
	applied, err := g.Store.Applied(ctx, target)
	if err != nil {
		return err
	}
	missing := controlplane.Unreconciled(obs, applied)
	if len(missing) == 0 {
		return nil
	}
	refusal := fmt.Errorf("%w: %s records %s; run `godwit target adopt %s --from-journal --dir <migrations>` to adopt what it already has",
		errUnreconciled, target, strings.Join(missing, ", "), target)
	g.Log.Warn("run refused by the reconcile gate", "target", target, "missing", missing)

	return precondition(refusal)
}

// ObservedSearchPath is the target's live search path, empty when no observer is configured.
func (g Gate) ObservedSearchPath(ctx context.Context, target string) (string, error) {
	if g.Observer == nil {
		return "", nil
	}
	obs, err := g.Observer.Observe(ctx, target)
	if err != nil {
		return "", err
	}

	return obs.SearchPath, nil
}

// Detail names the plan a run bound to and what godwit expanded for it.
func (b Binding) Detail() string {
	switch {
	case b.Superseded.ID != "":
		return join(fmt.Sprintf("plan %s superseded by %s", notify.ShortID(b.Superseded.ID), notify.ShortID(b.PlanID)), b.Expanded())
	case b.PlanID != "":
		return join("plan "+notify.ShortID(b.PlanID), b.Expanded())
	default:
		return b.Expanded()
	}
}

func join(a, b string) string {
	if b == "" {
		return a
	}

	return a + ", " + b
}

// Expanded names what godwit generated for this run, so an implicit run without a stored plan still
// leaves the expansion in the audit trail and the notification.
func (b Binding) Expanded() string {
	ids := make([]string, 0, len(b.Expansions))
	for id, e := range b.Expansions {
		ids = append(ids, id+" "+notify.ShortID(e.Hash))
	}
	if len(ids) == 0 {
		return ""
	}
	slices.Sort(ids)

	return "expands " + strings.Join(ids, ", ")
}

// PlanSince is the oldest creation time a stored plan may have and still bind; the zero time keeps
// plans forever.
func (g Gate) PlanSince() time.Time {
	if g.PlanTTL <= 0 {
		return time.Time{}
	}

	return time.Now().Add(-g.PlanTTL)
}

func (g Gate) lookup(ctx context.Context, req Request, set Set, pending []engine.Migration) (controlplane.Plan, error) {
	if req.PlanID == "" {
		key := controlplane.PlanKey(req.Target, set.Rollout, pending)
		plan, err := g.Store.ReadyPlan(ctx, req.Target, key, g.PlanSince())
		if errors.Is(err, controlplane.ErrNotFound) {
			return controlplane.Plan{}, g.noPlan(ctx, req.Target, key, pending)
		}
		if err != nil {
			return controlplane.Plan{}, err
		}

		return plan, nil
	}
	plan, err := g.Store.Plan(ctx, req.PlanID)
	if err != nil {
		return controlplane.Plan{}, err
	}
	switch plan.State {
	case controlplane.PlanBound:
		return controlplane.Plan{}, precondition(fmt.Errorf("plan %s is bound to run %s", plan.ID, plan.RunID))
	case controlplane.PlanSuperseded:
		return controlplane.Plan{}, precondition(fmt.Errorf("plan %s was superseded by %s", plan.ID, plan.SupersededBy))
	}
	if plan.CreatedAt.Before(g.PlanSince()) {
		return controlplane.Plan{}, precondition(fmt.Errorf("plan %s expired: planned %s, ttl %s", plan.ID, plan.CreatedAt.UTC().Format(time.RFC3339), g.PlanTTL))
	}
	planned, err := controlplane.Pending(migrations(set.Plans), plan.Applied, plan.Repeatables)
	if err != nil || controlplane.PlanKey(req.Target, set.Rollout, planned) != plan.Key {
		return controlplane.Plan{}, invalid(fmt.Errorf("files do not match plan %s", plan.ID))
	}

	return plan, nil
}

func (g Gate) noPlan(ctx context.Context, target, key string, pending []engine.Migration) error {
	required, err := g.requiresPlan(ctx, target)
	if err != nil {
		return err
	}
	if !required {
		return nil
	}
	nearest, err := g.Store.ListPlans(ctx, target, 3)
	if err != nil {
		return err
	}

	return g.refuse(ctx, target, &controlplane.PlanRequired{Target: target, Key: key, Pending: pending, Nearest: nearest})
}

func (g Gate) requiresPlan(ctx context.Context, target string) (bool, error) {
	_, config, err := g.Store.Target(ctx, target)
	if err != nil {
		return false, err
	}

	return g.RequirePlan || config[controlplane.ConfigRequirePlan] == "true", nil
}

func (g Gate) attribute(ctx context.Context, plan controlplane.Plan, obs controlplane.Observation) (controlplane.PlanDiff, error) {
	d := controlplane.StaleDiff(plan, obs)
	if len(d.Added) == 0 {
		return d, nil
	}
	runs, err := g.Store.RunsApplying(ctx, plan.Target, plan.CreatedAt)
	if err != nil {
		return d, err
	}
	for i := range d.Added {
		d.Added[i].RunID = runs[d.Added[i].String()]
	}

	return d, nil
}

func (g Gate) baselineFingerprint(ctx context.Context, target string) (string, error) {
	snap, err := g.Store.SnapshotFor(ctx, target)
	if errors.Is(err, controlplane.ErrNotFound) {
		return "", nil
	}

	return snap.Fingerprint, err
}

func (g Gate) replanFailure(ctx context.Context, plan controlplane.Plan, d controlplane.PlanDiff, err error) error {
	stale := &controlplane.PlanStale{Plan: plan, Diff: d}
	switch {
	case errors.Is(err, errOutOfOrder):
		stale.Reason, stale.Hint = controlplane.StaleOrder, "pass allow_out_of_order or renumber the migration above the newest applied version"
	case errors.Is(err, controlplane.ErrValidationFailed):
		stale.Reason, stale.Hint = controlplane.StaleValidation, "the set no longer validates on the target's history: "+err.Error()
	default:
		return err
	}

	return g.refuse(ctx, plan.Target, stale)
}

func staleHint(reason, target string) string {
	if reason == controlplane.StaleSchema {
		return fmt.Sprintf("push to the pull request (re-plan) or `godwit drift accept %s` if the schema changes are intended", target)
	}

	return "push to the pull request (re-plan) after checking who changed godwit.migrations on " + target
}

func (g Gate) refuse(ctx context.Context, target string, reason error) error {
	g.Log.Warn("run refused by plan contract", "target", target, "actor", authz.Actor(ctx), "error", reason.Error())

	return reason
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
