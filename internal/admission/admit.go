package admission

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

var errOutOfOrder = errors.New("out-of-order migrations")

// Admitted is what the gates decided about a set.
type Admitted struct {
	Applied    controlplane.AppliedSet
	Validated  bool
	Validation *controlplane.Validation
	Plans      []engine.Plan
	Expansions map[string]controlplane.Expansion
}

// Expanded is what admission decided to run, or the submitted plans when a validator stub reported none.
func (a Admitted) Expanded(set Set) []engine.Plan {
	if a.Plans != nil {
		return a.Plans
	}

	return set.Plans
}

// Migrations is what a plan response lists: what the run applies, then what a version target withheld.
func (a Admitted) Migrations(set Set) []controlplane.PlanMigration {
	migs := controlplane.BuildPlanMigrations(set.Rollout, a.Expanded(set), a.Applied, a.Expansions)
	controlplane.AttachChanges(migs, a.Validation)

	return set.withWithheld(migs, a.Applied)
}

// Admit refuses unacknowledged hazards, out-of-order versions and plans that fail on the scratch database.
func (g Gate) Admit(ctx context.Context, target string, plans []engine.Plan, acked []string, skipValidation, allowOutOfOrder bool, searchPath string) (Admitted, error) {
	if _, _, err := g.Store.Target(ctx, target); err != nil {
		return Admitted{}, err
	}
	applied, err := g.Store.Applied(ctx, target)
	if err != nil {
		return Admitted{}, err
	}
	if err := g.checkOrder(target, plans, applied.Versions, allowOutOfOrder); err != nil {
		return Admitted{}, err
	}
	plans, err = engine.ShapeCheckpoint(plans, applied.Newest())
	if err != nil {
		g.Log.Warn("run refused by the checkpoint gate", "target", target, "error", err.Error())

		return Admitted{}, precondition(err)
	}
	if err := g.checkHazards(plans, applied, acked); err != nil {
		g.Log.Warn("run refused by hazard gate", "target", target, "error", err.Error())

		return Admitted{}, precondition(err)
	}
	adm := Admitted{Applied: applied, Plans: plans}
	if g.Validator == nil || skipValidation {
		if id := directiveID(plans, applied); id != "" {
			return Admitted{}, invalid(errors.New(id + " carries a godwit directive: directives need validation, so drop --skip-validation"))
		}

		return adm, nil
	}
	val, err := g.Validator.Validate(ctx, target, plans, searchPath)
	if err != nil {
		if errors.Is(err, controlplane.ErrValidationFailed) || errors.Is(err, controlplane.ErrDirective) {
			g.Metrics.ValidationFailed(target)
			g.Log.Warn("run refused by validation", "target", target, "error", err.Error())

			return Admitted{}, invalid(err)
		}

		return Admitted{}, err
	}
	adm.Validated, adm.Validation = true, &val
	adm.Expansions = val.Expansions
	if val.Plans != nil {
		adm.Plans = val.Plans
	}

	return adm, nil
}

func directiveID(plans []engine.Plan, applied controlplane.AppliedSet) string {
	for _, p := range plans {
		if len(p.Migration.Directives) > 0 && !applied.Has(p.Migration) {
			return p.Migration.ID()
		}
	}

	return ""
}

// CheckRollout refuses a directive that splits into two phases under a rollout that runs everything at once.
func CheckRollout(rollout string, plans []engine.Plan) error {
	if rollout != controlplane.RolloutDirect {
		return nil
	}
	for _, p := range plans {
		for _, st := range p.Statements {
			if st.Phase == engine.PhaseContract {
				return precondition(fmt.Errorf("%s expands into expand and contract phases; use rollout: expand-contract", p.Migration.ID()))
			}
		}
	}

	return nil
}

// CheckIdle refuses to plan or run against a target parked between the phases of an earlier run.
func (g Gate) CheckIdle(ctx context.Context, target string) error {
	run, ok, err := g.Store.AwaitingContract(ctx, target)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	return precondition(fmt.Errorf("target %s has run %s awaiting contract; confirm or revert it first", target, run.ID))
}

func (g Gate) checkHazards(plans []engine.Plan, applied controlplane.AppliedSet, acked []string) error {
	ackSet := map[string]bool{}
	for _, code := range acked {
		ackSet[code] = true
	}
	var pending []string
	for _, p := range plans {
		if !controlplane.RunsBody(p, applied) {
			continue
		}
		for _, st := range p.Statements {
			for _, h := range st.Hazards {
				g.Metrics.Hazard(h.Code, ackSet[h.Code])
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

func (g Gate) checkOrder(target string, plans []engine.Plan, applied []int64, allow bool) error {
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
		g.Log.Warn("out-of-order migrations admitted", "target", target, "versions", behind, "latest_applied", latest)

		return nil
	}
	err := fmt.Errorf("%w %s: newest applied version on %s is %d (pass allow_out_of_order to apply them anyway)",
		errOutOfOrder, strings.Join(behind, ", "), target, latest)
	g.Log.Warn("run refused by order guard", "target", target, "error", err.Error())

	return precondition(err)
}
