// Package admission decides what a submitted migration set becomes on a target and whether the control
// plane lets it run.
package admission

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
)

// Observer reports a target's live history and schema.
type Observer interface {
	Observe(ctx context.Context, target string) (controlplane.Observation, error)
}

// Validator checks migrations before admission.
type Validator interface {
	Validate(ctx context.Context, target string, plans []engine.Plan, searchPath string) (controlplane.Validation, error)
}

// Gate is what every admission decision reads. A nil Observer leaves stored plans unavailable and every
// run implicit; a nil Validator skips the scratch replay.
type Gate struct {
	Store       *controlplane.Store
	Observer    Observer
	Validator   Validator
	Log         *slog.Logger
	Metrics     *metrics.Metrics
	NewID       func() string
	PlanTTL     time.Duration
	RequirePlan bool
}

// Request is what a caller asks of a target, free of the wire types it arrived in.
type Request struct {
	Target          string
	PlanID          string
	Source          string
	Acked           []string
	SkipValidation  bool
	AllowOutOfOrder bool
}

// Set is a submitted migration directory as the control plane reads it.
type Set struct {
	Rollout string
	Files   map[string]string
	Plans   []engine.Plan
	// Withheld is what a version target kept out of Plans and Files: reported, never run.
	Withheld []engine.Plan
}

// NewSet builds the plans files would run under rollout; an empty rollout takes the direct policy.
func NewSet(rollout string, files map[string]string) (Set, error) {
	if rollout == "" {
		rollout = controlplane.RolloutDirect
	}
	if _, ok := controlplane.Policies()[rollout]; !ok {
		return Set{}, invalid(errors.New("unknown rollout policy " + rollout))
	}
	plans, err := controlplane.PlansFromFiles(files, engine.DirectionUp)
	if err != nil {
		return Set{}, invalid(err)
	}

	return Set{Rollout: rollout, Files: files, Plans: plans}, nil
}

// StopAt cuts the set at a version target: the whole directory arrives, only the part at or below to
// runs, and the rest is reported as withheld rather than dropped. A target at or below zero keeps it whole.
func (g Gate) StopAt(ctx context.Context, target string, set Set, to int64) (Set, error) {
	if to <= 0 {
		return set, nil
	}
	keep, withheld, err := g.Store.SelectVersion(ctx, target, set.Plans, to)
	switch {
	case errors.Is(err, controlplane.ErrNoSuchVersion):
		return set, invalid(err)
	case errors.Is(err, controlplane.ErrVersionTarget):
		return set, precondition(err)
	case err != nil:
		return set, err
	}
	files := make(map[string]string, 2*len(keep))
	for _, p := range keep {
		files[p.Migration.UpFile()] = p.Migration.UpSQL
		files[p.Migration.DownFile()] = p.Migration.DownSQL
	}
	set.Files, set.Plans, set.Withheld = files, keep, withheld

	return set, nil
}

func (s Set) withWithheld(migs []controlplane.PlanMigration, applied controlplane.AppliedSet) []controlplane.PlanMigration {
	return append(migs, controlplane.WithheldMigrations(s.Withheld, applied)...)
}

func migrations(plans []engine.Plan) []engine.Migration {
	out := make([]engine.Migration, 0, len(plans))
	for _, p := range plans {
		out = append(out, p.Migration)
	}

	return out
}
