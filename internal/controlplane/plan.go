package controlplane

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/notify"
)

// Plan states.
const (
	PlanReady      = "ready"
	PlanBound      = "bound"
	PlanSuperseded = "superseded"
)

// Stale reasons carried by PlanStale.
const (
	StaleHistory    = "history"
	StaleSchema     = "schema"
	StaleOrder      = "order"
	StaleValidation = "validation"
	StaleContent    = "content"
)

// ErrAppliedContent marks a file whose version is applied on the target with a different checksum.
var ErrAppliedContent = errors.New("applied with different content")

// Observation is a target's live migration history and schema at one moment.
type Observation struct {
	Applied     []engine.Applied
	Definition  string
	Fingerprint string
	SearchPath  string
	At          time.Time
}

// HistoryHash hashes the observed history, ascending by version.
func (o Observation) HistoryHash() string {
	return HistoryHash(o.Applied)
}

// PlanHazard is one hazard on a planned statement.
type PlanHazard struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Recipe string `json:"recipe,omitempty"`
}

// PlanStatement is one classified statement of a stored plan.
type PlanStatement struct {
	SQL     string       `json:"sql"`
	NoTx    bool         `json:"no_tx,omitempty"`
	Hazards []PlanHazard `json:"hazards,omitempty"`
}

// PlanMigration is one migration of a stored plan with its phase and applied flag.
type PlanMigration struct {
	Version    int64           `json:"version"`
	Name       string          `json:"name"`
	Checksum   string          `json:"checksum"`
	Applied    bool            `json:"applied"`
	Phase      string          `json:"phase"`
	Statements []PlanStatement `json:"statements"`

	AlreadyApplied bool   `json:"already_applied,omitempty"`
	Effect         string `json:"effect,omitempty"`
	Note           string `json:"note,omitempty"`
}

// Plan is a stored admission result plus the observation it was taken against.
type Plan struct {
	ID                string
	Target            string
	Key               string
	Rollout           string
	State             string
	HistoryHash       string
	Applied           []engine.Applied
	SchemaFingerprint string
	SchemaDefinition  string
	Drift             string
	Migrations        []PlanMigration
	Validated         bool
	Acked             []string
	AllowOutOfOrder   bool
	CreatedBy         string
	Source            string
	CreatedAt         time.Time
	RunID             string
	SupersededBy      string
}

// Pending returns the migrations the plan would run.
func (p Plan) Pending() []PlanMigration {
	var out []PlanMigration
	for _, m := range p.Migrations {
		if !m.Applied {
			out = append(out, m)
		}
	}

	return out
}

// HistoryHash hashes an applied set as "version checksum" lines, ascending by version.
func HistoryHash(applied []engine.Applied) string {
	sorted := slices.Clone(applied)
	slices.SortFunc(sorted, func(a, b engine.Applied) int { return cmp.Compare(a.Version, b.Version) })
	var b strings.Builder
	for _, a := range sorted {
		fmt.Fprintf(&b, "%d %s\n", a.Version, a.Checksum)
	}

	return sum(b.String())
}

// Pending returns the migrations whose version the target has not applied, sorted by version.
func Pending(migs []engine.Migration, applied []engine.Applied) ([]engine.Migration, error) {
	live := map[int64]string{}
	for _, a := range applied {
		live[a.Version] = a.Checksum
	}
	var out []engine.Migration
	for _, m := range migs {
		sum, ok := live[m.Version]
		switch {
		case !ok:
			out = append(out, m)
		case sum != m.Checksum:
			return nil, fmt.Errorf("%014d_%s %w", m.Version, m.Name, ErrAppliedContent)
		}
	}
	slices.SortFunc(out, func(a, b engine.Migration) int { return cmp.Compare(a.Version, b.Version) })

	return out, nil
}

// PlanKey identifies a pending set on a target: the files and what the target has applied, nothing else.
func PlanKey(target, rollout string, pending []engine.Migration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "godwit-plan-v1\n%s\n%s\n", target, rollout)
	for _, m := range pending {
		fmt.Fprintf(&b, "%014d_%s %s %s\n", m.Version, m.Name, sum(m.UpSQL), sum(m.DownSQL))
	}

	return sum(b.String())
}

// BuildPlanMigrations records what admission decided for each plan: phase under the rollout and whether it is applied.
func BuildPlanMigrations(rollout string, plans []engine.Plan, applied []int64) []PlanMigration {
	expand, _ := Policies()[rollout].Split(plans)
	out := make([]PlanMigration, 0, len(plans))
	for i, p := range plans {
		phase := PhaseContract
		if i < len(expand) {
			phase = PhaseExpand
		}
		pm := PlanMigration{
			Version: p.Migration.Version, Name: p.Migration.Name, Checksum: p.Migration.Checksum,
			Applied: slices.Contains(applied, p.Migration.Version), Phase: phase,
		}
		for _, st := range p.Statements {
			ps := PlanStatement{SQL: st.SQL, NoTx: st.NoTx}
			for _, h := range st.Hazards {
				ps.Hazards = append(ps.Hazards, PlanHazard{Code: h.Code, Detail: h.Detail, Recipe: h.Recipe})
			}
			pm.Statements = append(pm.Statements, ps)
		}
		out = append(out, pm)
	}

	return out
}

// Detect marks the longest prefix of migs whose effect the target already holds; otherwise returns the drift.
func Detect(migs []PlanMigration, plans []engine.Plan, v Validation, obs Observation) string {
	k := len(v.Fingerprints) - 1
	for k >= 0 && v.Fingerprints[k] != obs.Fingerprint {
		k--
	}
	if k < 0 {
		return detectDrift(migs, v, obs)
	}
	for i := range k {
		if migs[i].Applied {
			continue
		}
		reason := plans[i].Opaque()
		if reason == "" && len(v.Effects[i]) == 0 {
			reason = engine.OpaqueUnknown
		}
		if reason != "" {
			migs[i].Note = reason

			break
		}
		migs[i].AlreadyApplied = true
		migs[i].Effect = strings.Join(v.Effects[i], "\n")
	}

	return ""
}

func detectDrift(migs []PlanMigration, v Validation, obs Observation) string {
	lines := engine.DiffSchemas(v.Base, obs.Definition)
	present := make(map[string]bool, len(lines))
	for _, l := range lines {
		present[l] = true
	}
	for i := range migs {
		if migs[i].Applied || len(v.Effects[i]) == 0 {
			continue
		}
		contained := true
		for _, l := range v.Effects[i] {
			contained = contained && present[l]
		}
		if contained {
			migs[i].Note = "effect is present but not as a prefix"
		}
	}

	return strings.Join(lines, "\n")
}

// SameStatements reports whether two plans would run the same statements with the same hazards and phases.
func SameStatements(a, b []PlanMigration) bool {
	return len(a) == len(b) && shape(a) == shape(b)
}

func shape(ms []PlanMigration) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "%d %s %s %t\n", m.Version, m.Checksum, m.Phase, m.AlreadyApplied)
		for _, st := range m.Statements {
			codes := make([]string, 0, len(st.Hazards))
			for _, h := range st.Hazards {
				codes = append(codes, h.Code)
			}
			fmt.Fprintf(&b, "  %s %t %s\n", sum(st.SQL), st.NoTx, strings.Join(codes, ","))
		}
	}

	return b.String()
}

// HistoryChange is one version that entered or left the target's history since a plan was taken.
type HistoryChange struct {
	Version int64
	Name    string
	At      time.Time
	RunID   string
}

// String renders the change as it appears in a stale report.
func (c HistoryChange) String() string {
	return fmt.Sprintf("%014d_%s", c.Version, c.Name)
}

// PlanDiff is how a target moved away from a plan's observation.
type PlanDiff struct {
	Added   []HistoryChange
	Removed []HistoryChange
	Schema  []string
}

// StaleDiff compares a plan's observation with a fresh one.
func StaleDiff(p Plan, obs Observation) PlanDiff {
	then := map[int64]engine.Applied{}
	for _, a := range p.Applied {
		then[a.Version] = a
	}
	now := map[int64]engine.Applied{}
	for _, a := range obs.Applied {
		now[a.Version] = a
	}
	var d PlanDiff
	for _, a := range obs.Applied {
		if _, ok := then[a.Version]; !ok {
			d.Added = append(d.Added, HistoryChange{Version: a.Version, Name: a.Name, At: a.AppliedAt})
		}
	}
	for _, a := range p.Applied {
		if _, ok := now[a.Version]; !ok {
			d.Removed = append(d.Removed, HistoryChange{Version: a.Version, Name: a.Name, At: a.AppliedAt})
		}
	}
	slices.SortFunc(d.Added, func(a, b HistoryChange) int { return cmp.Compare(a.Version, b.Version) })
	slices.SortFunc(d.Removed, func(a, b HistoryChange) int { return cmp.Compare(a.Version, b.Version) })
	if p.SchemaFingerprint != obs.Fingerprint {
		d.Schema = engine.DiffSchemas(p.SchemaDefinition, obs.Definition)
	}

	return d
}

// Explained reports whether every change was made by godwit itself: versions only added, each by a run,
// and the live schema exactly what the last run left (baseline is the stored snapshot fingerprint).
func (d PlanDiff) Explained(baseline, fingerprint string) bool {
	if len(d.Removed) > 0 || baseline != fingerprint {
		return false
	}
	for _, c := range d.Added {
		if c.RunID == "" {
			return false
		}
	}

	return true
}

// Reason names what refuses the bind: history when a version moved without a run, schema otherwise.
func (d PlanDiff) Reason() string {
	if len(d.Removed) > 0 {
		return StaleHistory
	}
	for _, c := range d.Added {
		if c.RunID == "" {
			return StaleHistory
		}
	}

	return StaleSchema
}

// PlanStale is returned when a stored plan no longer matches the target it was taken against.
type PlanStale struct {
	Plan   Plan
	Reason string
	Diff   PlanDiff
	Hint   string
}

// Error renders the report shown on the pull request and in the CLI.
func (e *PlanStale) Error() string {
	var b strings.Builder
	if e.Plan.ID == "" {
		fmt.Fprintf(&b, "migration set on %s cannot bind\n  reason : %s\nfix: %s", e.Plan.Target, e.Reason, e.Hint)

		return b.String()
	}
	fmt.Fprintf(&b, "plan %s on %s is stale (planned %s by %s", notify.ShortID(e.Plan.ID), e.Plan.Target,
		e.Plan.CreatedAt.UTC().Format(time.RFC3339), e.Plan.CreatedBy)
	if e.Plan.Source != "" {
		fmt.Fprintf(&b, ", %s", e.Plan.Source)
	}
	fmt.Fprintf(&b, ")\n  reason : %s\n", e.Reason)
	if len(e.Diff.Added)+len(e.Diff.Removed) > 0 {
		b.WriteString("  history:")
		for i, c := range e.Diff.Added {
			fmt.Fprintf(&b, "%s+ %s   applied %s by %s\n", indent(i), c, c.At.UTC().Format("15:04Z"), applier(c))
		}
		for i, c := range e.Diff.Removed {
			fmt.Fprintf(&b, "%s- %s   removed from history\n", indent(i+len(e.Diff.Added)), c)
		}
	}
	if len(e.Diff.Schema) > 0 {
		b.WriteString("  schema :")
		for i, l := range e.Diff.Schema {
			fmt.Fprintf(&b, "%s%s\n", indent(i), l)
		}
		fmt.Fprintf(&b, "           (%d changes not made by any run since the plan)\n", len(e.Diff.Schema))
	}
	fmt.Fprintf(&b, "  files  : unchanged (key %s…)\nfix: %s", notify.ShortID(e.Plan.Key), e.Hint)

	return b.String()
}

func indent(i int) string {
	if i == 0 {
		return " "
	}

	return "           "
}

func applier(c HistoryChange) string {
	if c.RunID == "" {
		return "no run (unexplained)"
	}

	return "run " + notify.ShortID(c.RunID) + " (explained)"
}

// PlanRequired is returned when a target demands a stored plan and none matches the migration set.
type PlanRequired struct {
	Target  string
	Key     string
	Pending []engine.Migration
	Nearest []Plan
}

// Error renders the report with the nearest stored plans next to this set.
func (e *PlanRequired) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "target %s requires a stored plan and none matches this migration set (key %s…)\n", e.Target, notify.ShortID(e.Key))
	if len(e.Nearest) == 0 {
		b.WriteString("  nearest: none\n")
	}
	for i, p := range e.Nearest {
		label := "  nearest:"
		if i > 0 {
			label = "          "
		}
		fmt.Fprintf(&b, "%s plan %s (%s) covers %s\n           this set : %s\n", label, notify.ShortID(p.ID),
			p.CreatedAt.UTC().Format("2006-01-02T15:04Z"), names(p.Pending()), e.filesDiff(p))
	}
	fmt.Fprintf(&b, "fix: run `godwit plan --target %s` on the pull request; the Action does it with command: plan", e.Target)

	return b.String()
}

// FilesDiff renders this set against each nearest plan, one line per plan.
func (e *PlanRequired) FilesDiff() string {
	lines := make([]string, 0, len(e.Nearest))
	for _, p := range e.Nearest {
		lines = append(lines, notify.ShortID(p.ID)+": "+e.filesDiff(p))
	}

	return strings.Join(lines, "\n")
}

func (e *PlanRequired) filesDiff(p Plan) string {
	stored := map[int64]string{}
	for _, m := range p.Pending() {
		stored[m.Version] = m.Checksum
	}
	parts := make([]string, 0, len(e.Pending))
	for _, m := range e.Pending {
		part := fmt.Sprintf("%014d_%s", m.Version, m.Name)
		switch sum, ok := stored[m.Version]; {
		case !ok:
			part += " (not in plan)"
		case sum != m.Checksum:
			part += " (up checksum differs)"
		}
		parts = append(parts, part)
	}

	return strings.Join(parts, ", ")
}

func names(ms []PlanMigration) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, fmt.Sprintf("%014d_%s", m.Version, m.Name))
	}

	return strings.Join(parts, ", ")
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))

	return hex.EncodeToString(h[:])
}
