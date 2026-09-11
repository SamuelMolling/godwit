package report

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/internal/engine"
)

// PlanFromProto is the plan the service answered a PlanRun with.
func PlanFromProto(m *godwitv1.PlanRunResponse) Plan {
	r := Plan{
		live: true, target: m.Target, rollout: m.Rollout, validated: m.Validated, items: make([]item, 0, len(m.Migrations)),
		planID: m.PlanId, planKey: m.PlanKey, drift: m.Drift,
	}
	r.observed = observationFromProto(m.Observed)
	for _, pm := range m.Migrations {
		p := engine.Plan{
			Migration: engine.Migration{Version: pm.Version, Name: pm.Name, Repeatable: pm.Repeatable, Checksum: pm.Checksum},
			Direction: engine.DirectionUp,
		}
		for _, ps := range pm.Statements {
			st := engine.Statement{SQL: ps.Sql, NoTx: ps.NoTx, Phase: ps.Phase}
			if b := ps.Batch; b != nil {
				st.Batch = &engine.BatchSpec{Key: b.Key, KeyKind: b.Kind, Size: int(b.Size), Pause: parsePause(b.Pause)}
			}
			if a := ps.Assert; a != nil {
				st.Assert = &engine.AssertSpec{Op: a.Op, Kind: a.Kind, Value: a.Value}
			}
			for _, h := range ps.Hazards {
				st.Hazards = append(st.Hazards, engine.Hazard{
					Code: h.Code, Detail: h.Detail, Recipe: h.Recipe, Object: h.Object, Attribute: h.Attribute,
				})
			}
			p.Statements = append(p.Statements, st)
		}
		r.items = append(r.items, item{
			Plan: p, applied: pm.Applied, phase: pm.Phase, alreadyApplied: pm.AlreadyApplied, effect: pm.Effect, note: pm.Note,
			directives: pm.Directives, expanded: pm.Expanded, notes: pm.Notes, withheld: pm.Withheld,
			skipped: pm.Skipped, changes: changesFromProto(pm.Changes),
		})
	}

	return r
}

// PlanFromStored is a plan the service kept, with what became of it since.
func PlanFromStored(p *godwitv1.Plan) Plan {
	r := PlanFromProto(&godwitv1.PlanRunResponse{
		Target: p.Target, Rollout: p.Rollout, Migrations: p.Migrations, Validated: p.Validated,
		PlanId: p.Id, PlanKey: p.Key, Observed: p.Observed, Drift: p.Drift,
	})
	r.stored = &storedPlan{
		State: p.State, RunID: p.RunId, SupersededBy: p.SupersededBy, CreatedBy: p.CreatedBy, CreatedAt: Stamp(p.CreatedAt),
		Source: p.Source, Acked: p.AcknowledgedHazards, AllowOutOfOrder: p.AllowOutOfOrder,
	}

	return r
}

// PlanOffline is both sides of every migration as written, which no database was consulted for; nothing names an empty migration directory.
func PlanOffline(nothing string, plans []engine.Plan) Plan {
	r := Plan{nothing: nothing, items: make([]item, 0, len(plans))}
	for _, p := range plans {
		r.items = append(r.items, item{Plan: p})
	}

	return r
}

// PlanNothing is what a repository that has not written its first migration gets instead of a failure.
func PlanNothing(target, why string) Plan {
	return Plan{live: true, target: target, nothing: why}
}

// RunFrom is what one run did to its target, read against the plan it was bound to; a nil plan is an implicit run, and an empty public links to nothing.
func RunFrom(run *godwitv1.Run, applied []*godwitv1.RunMigration, plan *godwitv1.Plan, command, public string) Run {
	r := Run{
		command: command, run: run, applied: applied, public: public,
		plan: Plan{live: true, target: run.GetTarget(), rollout: run.GetRollout()},
	}
	if plan != nil {
		r.plan = PlanFromStored(plan)
	}

	return r
}

func changesFromProto(in []*godwitv1.SchemaChange) []engine.ObjectChange {
	out := make([]engine.ObjectChange, 0, len(in))
	for _, c := range in {
		oc := engine.ObjectChange{Op: c.Op, Kind: c.Kind, Schema: c.Schema, Name: c.Name, Unchanged: int(c.Unchanged)}
		for _, a := range c.Attributes {
			oc.Attrs = append(oc.Attrs, engine.AttrChange{Op: a.Op, Name: a.Name, Old: a.Old, New: a.New})
		}
		out = append(out, oc)
	}

	return out
}

func parsePause(v string) time.Duration {
	d, _ := time.ParseDuration(v)

	return d
}

func observationFromProto(o *godwitv1.PlanObservation) *planObservation {
	if o == nil {
		return nil
	}

	return &planObservation{
		HistoryHash: o.HistoryHash, SchemaFingerprint: o.SchemaFingerprint,
		AppliedCount: o.AppliedCount, NewestApplied: o.NewestApplied, At: Stamp(o.At), IgnoredTables: o.IgnoredTables,
	}
}

// Stamp is how godwit prints a time it read off the service: RFC3339 in UTC, and empty for no time at all.
func Stamp(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}

	return ts.AsTime().UTC().Format(time.RFC3339)
}
