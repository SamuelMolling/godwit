package controlplane

import "github.com/SamuelMolling/godwit/internal/engine"

// Rollout policy names, chosen per run.
const (
	RolloutDirect         = "direct"
	RolloutExpandContract = "expand-contract"
)

// RolloutPolicy splits a run's plans into the expand phase (applied now)
// and the contract phase (held until ConfirmRollout).
type RolloutPolicy interface {
	Split(plans []engine.Plan) (expand, contract []engine.Plan)
}

// Direct applies everything at once — the default.
type Direct struct{}

// Split implements RolloutPolicy.
func (Direct) Split(plans []engine.Plan) ([]engine.Plan, []engine.Plan) { return plans, nil }

// ExpandContract holds the first destructive migration and everything after
// it, so the previous app version keeps working until the rollout is confirmed.
type ExpandContract struct{}

var contractHazards = map[string]bool{"H002": true, "H003": true}

// Split implements RolloutPolicy.
func (ExpandContract) Split(plans []engine.Plan) ([]engine.Plan, []engine.Plan) {
	for i, p := range plans {
		if destructive(p) {
			return plans[:i], plans[i:]
		}
	}

	return plans, nil
}

func destructive(p engine.Plan) bool {
	for _, st := range p.Statements {
		for _, h := range st.Hazards {
			if contractHazards[h.Code] {
				return true
			}
		}
	}

	return false
}

// Policies is the built-in registry keyed by rollout name.
func Policies() map[string]RolloutPolicy {
	return map[string]RolloutPolicy{
		RolloutDirect:         Direct{},
		RolloutExpandContract: ExpandContract{},
	}
}
