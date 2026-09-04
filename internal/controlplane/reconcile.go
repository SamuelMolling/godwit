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

// ErrDiverged marks a target whose own journal and the control plane's ledger cannot be reconciled
// without a decision only a person can take.
var ErrDiverged = errors.New("target and ledger disagree")

// Divergence is how a target's journal and the control plane's ledger differ. Adopt is repairable;
// everything else is a fact about the world that a reconcile must not paper over.
type Divergence struct {
	// Adopt names what the target records and the ledger does not; reconciling writes these.
	Adopt []string
	// Conflicting names what both record, under different content.
	Conflicting []string
	// Unknown names what the target records and the given directory does not carry.
	Unknown []string
	// Withdrawn names what the ledger stands on and the target does not record.
	Withdrawn []string
}

// Unreconciled names the migrations a target's journal records that no standing ledger row accounts
// for. An observation with no history at all reports none: nothing was looked at.
func Unreconciled(obs Observation, applied AppliedSet) []string {
	var out []string
	for _, a := range obs.Applied {
		if !slices.Contains(applied.Versions, a.Version) {
			out = append(out, engine.MigrationID(a.Version, a.Name, false))
		}
	}
	for _, r := range obs.Repeatables {
		if applied.Repeatables[r.Name] != r.Checksum {
			out = append(out, engine.RepeatablePrefix+r.Name)
		}
	}
	slices.Sort(out)

	return out
}

// Diverged compares a target's journal with the ledger, reading the content of each migration from migs.
func Diverged(obs Observation, applied AppliedSet, migs []engine.Migration) Divergence {
	byID := make(map[string]engine.Migration, len(migs))
	for _, m := range migs {
		byID[m.ID()] = m
	}
	var d Divergence
	journal := journalOf(obs)
	for id, sum := range journal {
		m, ok := byID[id]
		switch {
		case !ok:
			d.Unknown = append(d.Unknown, id)
		case m.Checksum != sum:
			d.Conflicting = append(d.Conflicting, id)
		case !applied.Has(m):
			d.Adopt = append(d.Adopt, id)
		}
	}
	for _, v := range applied.Versions {
		if !slices.ContainsFunc(obs.Applied, func(a engine.Applied) bool { return a.Version == v }) {
			d.Withdrawn = append(d.Withdrawn, strconv.FormatInt(v, 10))
		}
	}
	for name, sum := range applied.Repeatables {
		if journal[engine.RepeatablePrefix+name] != sum {
			d.Withdrawn = append(d.Withdrawn, engine.RepeatablePrefix+name)
		}
	}
	for _, list := range [][]string{d.Adopt, d.Conflicting, d.Unknown, d.Withdrawn} {
		slices.Sort(list)
	}

	return d
}

// journalOf keys a target's recorded history by migration id.
func journalOf(obs Observation) map[string]string {
	out := make(map[string]string, len(obs.Applied)+len(obs.Repeatables))
	for _, a := range obs.Applied {
		out[engine.MigrationID(a.Version, a.Name, false)] = a.Checksum
	}
	for _, r := range obs.Repeatables {
		out[engine.RepeatablePrefix+r.Name] = r.Checksum
	}

	return out
}

// Err reports what a reconcile cannot decide on its own, or nil.
func (d Divergence) Err(target string) error {
	var parts []string
	for _, c := range []struct {
		label string
		ids   []string
	}{
		{"recorded under different content than the directory carries", d.Conflicting},
		{"recorded on the target and absent from the directory", d.Unknown},
		{"standing in the ledger and absent from the target", d.Withdrawn},
	} {
		if len(c.ids) > 0 {
			parts = append(parts, fmt.Sprintf("%s: %s", c.label, strings.Join(c.ids, ", ")))
		}
	}
	if len(parts) == 0 {
		return nil
	}

	return fmt.Errorf("%w on %s: %s", ErrDiverged, target, strings.Join(parts, "; "))
}

// Reconciler repairs the control plane's ledger from a target's own journal. It never writes to the target.
type Reconciler struct {
	sched *Scheduler
}

// NewReconciler shares the scheduler's store, credential providers and engine.
func NewReconciler(sched *Scheduler) *Reconciler {
	return &Reconciler{sched: sched}
}

// Reconcile adopts into run runID every migration target's journal records that the ledger does not,
// taking each body from migs, and reports what it found.
func (r *Reconciler) Reconcile(ctx context.Context, runID, target string, migs []engine.Migration, p Provenance) (Divergence, error) {
	dsn, err := r.sched.targetDSN(ctx, target)
	if err != nil {
		return Divergence{}, err
	}
	obs, err := r.sched.engine.Observe(ctx, dsn)
	if err != nil {
		return Divergence{}, err
	}
	applied, err := r.sched.store.Applied(ctx, target)
	if err != nil {
		return Divergence{}, err
	}
	d := Diverged(obs, applied, migs)
	if err := d.Err(target); err != nil {
		return d, err
	}
	if len(d.Adopt) == 0 {
		return d, nil
	}
	adopt := make([]engine.Migration, 0, len(d.Adopt))
	for _, m := range migs {
		if slices.Contains(d.Adopt, m.ID()) {
			adopt = append(adopt, m)
		}
	}
	slices.SortFunc(adopt, engine.CompareMigrations)
	if err := r.sched.store.CreateAdoption(ctx, runID, target, KindReconcile, adopt, p); err != nil {
		return d, err
	}
	r.sched.baseline(ctx, Run{ID: runID, Target: target}, r.sched.log.With("run", runID, "target", target))

	return d, nil
}
