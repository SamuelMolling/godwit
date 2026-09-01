package controlplane

import "github.com/SamuelMolling/godwit/internal/engine"

// RolloutPolicy decides whether a plan may execute now relative to the
// deployment strategy (e.g. blue/green holds destructive contract migrations
// until the old version retires).
type RolloutPolicy interface {
	Allow(p engine.Plan) error
}

// Immediate allows everything at PreSync time — the v1 default.
type Immediate struct{}

// Allow implements RolloutPolicy.
func (Immediate) Allow(engine.Plan) error { return nil }
