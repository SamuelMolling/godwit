package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/SamuelMolling/godwit/internal/engine"
)

var (
	// ErrNoSuchVersion marks a version target that names no migration in the submitted set.
	ErrNoSuchVersion = errors.New("no migration in this set has version")
	// ErrVersionTarget marks a version target the target's own history will not honour.
	ErrVersionTarget = errors.New("version target")
)

// SelectVersion splits a submitted set at a version target: what is at or below to is kept, the rest withheld.
func (s *Store) SelectVersion(ctx context.Context, target string, plans []engine.Plan, to int64) (keep, withheld []engine.Plan, err error) {
	if !slices.ContainsFunc(plans, func(p engine.Plan) bool {
		return !p.Migration.Repeatable && p.Migration.Version == to
	}) {
		return nil, nil, fmt.Errorf("%w %d; a version target names one the directory holds: %s",
			ErrNoSuchVersion, to, versionList(plans))
	}
	applied, err := s.Applied(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	if n := len(applied.Versions); n > 0 && to < applied.Versions[n-1] {
		return nil, nil, fmt.Errorf("%w %d is behind version %d, already applied on %s: a target stops a run short, it never reverts (godwit revert undoes a run)",
			ErrVersionTarget, to, applied.Versions[n-1], target)
	}
	keep, withheld = Truncate(plans, to, applied)
	if next := firstPending(withheld, applied); next != "" && firstPending(keep, applied) == "" {
		return nil, nil, fmt.Errorf("%w %d selects nothing to apply on %s: everything at or below it is applied and the pending set starts at %s",
			ErrVersionTarget, to, target, next)
	}

	return keep, withheld, nil
}

func versionList(plans []engine.Plan) string {
	out := make([]string, 0, len(plans))
	for _, p := range plans {
		if !p.Migration.Repeatable {
			out = append(out, strconv.FormatInt(p.Migration.Version, 10))
		}
	}
	if len(out) == 0 {
		return "none, the set is repeatable migrations only"
	}

	return strings.Join(out, ", ")
}

func firstPending(plans []engine.Plan, applied AppliedSet) string {
	for _, p := range plans {
		if !applied.Has(p.Migration) {
			return p.Migration.ID()
		}
	}

	return ""
}
