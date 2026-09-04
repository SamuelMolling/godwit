package api

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/SamuelMolling/godwit/internal/controlplane"
	"github.com/SamuelMolling/godwit/internal/engine"
)

// stopAt cuts a submitted set at a version target: the whole directory arrives, only the part at or below to runs, and the rest is reported as withheld rather than dropped.
func (s *Server) stopAt(ctx context.Context, target string, spec runSpec, to int64) (runSpec, error) {
	if to <= 0 {
		return spec, nil
	}
	if !slices.ContainsFunc(spec.plans, func(p engine.Plan) bool {
		return !p.Migration.Repeatable && p.Migration.Version == to
	}) {
		return spec, invalid(fmt.Sprintf("no migration in this set has version %d; a version target names one the directory holds: %s",
			to, versionList(spec.plans)))
	}
	applied, err := s.store.Applied(ctx, target)
	if err != nil {
		return spec, rpcErr(err)
	}
	if n := len(applied.Versions); n > 0 && to < applied.Versions[n-1] {
		return spec, precondition(fmt.Sprintf("version target %d is behind version %d, already applied on %s: a target stops a run short, it never reverts (godwit revert undoes a run)",
			to, applied.Versions[n-1], target))
	}
	keep, withheld := controlplane.Truncate(spec.plans, to, applied)
	if next := firstPending(withheld, applied); next != "" && firstPending(keep, applied) == "" {
		return spec, precondition(fmt.Sprintf("version target %d selects nothing to apply on %s: everything at or below it is applied and the pending set starts at %s",
			to, target, next))
	}
	files := make(map[string]string, 2*len(keep))
	for _, p := range keep {
		files[p.Migration.UpFile()] = p.Migration.UpSQL
		files[p.Migration.DownFile()] = p.Migration.DownSQL
	}
	spec.files, spec.plans, spec.withheld = files, keep, withheld

	return spec, nil
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

func firstPending(plans []engine.Plan, applied controlplane.AppliedSet) string {
	for _, p := range plans {
		if !applied.Has(p.Migration) {
			return p.Migration.ID()
		}
	}

	return ""
}

// withWithheld appends the held-back migrations after already-applied detection has walked the kept ones by index.
func withWithheld(migs []controlplane.PlanMigration, spec runSpec, applied controlplane.AppliedSet) []controlplane.PlanMigration {
	return append(migs, controlplane.WithheldMigrations(spec.withheld, applied)...)
}
