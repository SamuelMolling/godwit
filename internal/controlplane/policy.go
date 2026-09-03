package controlplane

import "github.com/SamuelMolling/godwit/internal/engine"

// Rollout policy names.
const (
	RolloutDirect         = "direct"
	RolloutExpandContract = "expand-contract"
)

// RolloutPolicy splits a run's plans into the expand and contract phases.
type RolloutPolicy interface {
	Split(plans []engine.Plan) (expand, contract []engine.Plan)
}

// Direct applies everything at once.
type Direct struct{}

// Split implements RolloutPolicy.
func (Direct) Split(plans []engine.Plan) ([]engine.Plan, []engine.Plan) { return plans, nil }

// ExpandContract holds the first destructive migration and everything after it.
type ExpandContract struct{}

var contractHazards = map[string]bool{"H002": true, "H003": true, "H008": true}

// Split implements RolloutPolicy.
func (ExpandContract) Split(plans []engine.Plan) ([]engine.Plan, []engine.Plan) {
	for i, p := range plans {
		at := contractFrom(p)
		if at < 0 {
			continue
		}
		if at == 0 {
			return plans[:i], plans[i:]
		}
		partial := p
		partial.HoldFrom = at
		expand := make([]engine.Plan, i, i+1)
		copy(expand, plans[:i])

		return append(expand, partial), plans[i:]
	}

	return plans, nil
}

// contractFrom is the first statement of p belonging to the contract phase, or -1 when p has none.
func contractFrom(p engine.Plan) int {
	for i, st := range p.Statements {
		if st.Phase == engine.PhaseContract {
			return i
		}
	}
	if destructive(p) {
		return 0
	}

	return -1
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

// HeldStatements counts what the contract phase still has to run after a split.
func HeldStatements(expand, contract []engine.Plan) int {
	n := 0
	for _, p := range contract {
		n += len(p.Statements)
	}
	if i := len(expand) - 1; i >= 0 && expand[i].Held() {
		n -= expand[i].HoldFrom
	}

	return n
}

// Policies returns the built-in rollout policies.
func Policies() map[string]RolloutPolicy {
	return map[string]RolloutPolicy{
		RolloutDirect:         Direct{},
		RolloutExpandContract: ExpandContract{},
	}
}
