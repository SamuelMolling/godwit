package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// ErrValidationFailed marks a migration the author must fix.
var ErrValidationFailed = errors.New("migration failed validation")

var (
	connectScratch  = pgx.ConnectConfig
	snapshotScratch = engine.Snapshot
)

// Validator replays a target's history on a scratch database and applies new plans on top.
type Validator struct {
	// Expander renders `-- godwit:` directives against the scratch catalog before each plan is applied.
	Expander *Expander

	scratch *Scratch
	store   *Store
	newID   func() string
}

// NewValidator wires a Validator over the scratch connection.
func NewValidator(scratch *Scratch, store *Store, newID func() string) *Validator {
	return &Validator{Expander: NewExpander(), scratch: scratch, store: store, newID: newID}
}

// Validation is what the scratch database looked like after the history and after each plan in turn.
type Validation struct {
	Base         string
	Effects      [][]string
	Fingerprints []string
	// Expansions is the SQL godwit generated per migration id, empty when no plan carried a directive.
	Expansions map[string]Expansion
	// Plans is the validated set: every directive migration replaced by its expansion.
	Plans []engine.Plan
}

// Validate replays the history, applies each plan on top and snapshots the schema after every step.
func (v *Validator) Validate(ctx context.Context, target string, plans []engine.Plan, searchPath string) (Validation, error) {
	history, expander, err := v.historyOf(ctx, target)
	if err != nil {
		return Validation{}, err
	}

	name := "godwit_validate_" + v.newID()
	if err := v.scratch.create(ctx, name); err != nil {
		return Validation{}, err
	}
	defer func() { v.scratch.drop(ctx, name) }()

	conn, err := connectScratch(ctx, v.scratch.connConfig(name, ""))
	if err != nil {
		return Validation{}, fmt.Errorf("connect scratch database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	replayed, err := replayRuns(ctx, conn, history, searchPath)
	if err != nil {
		return Validation{}, err
	}

	return expander.validateEach(ctx, conn, plans, replayed)
}

// Replay rebuilds target's recorded history on conn and applies plans on top, so conn ends up holding the
// schema the committed files claim to produce; a migration the history already covers keeps its own expansion.
func (v *Validator) Replay(ctx context.Context, conn engine.DB, target, searchPath string, plans []engine.Plan) error {
	history, expander, err := v.historyOf(ctx, target)
	if err != nil {
		return err
	}
	replayed, err := replayRuns(ctx, conn, history, searchPath)
	if err != nil {
		return err
	}
	for _, p := range plans {
		if p, err = expander.expandPlan(ctx, conn, p, map[string]Expansion{}, replayed); err != nil {
			return err
		}
		if _, err := applyPlans(ctx, conn, engine.Options{}, []engine.Plan{p}, nil, engine.WithAssertProbe()); err != nil {
			return fmt.Errorf("%w: %w", ErrValidationFailed, err)
		}
	}

	return nil
}

func (v *Validator) historyOf(ctx context.Context, target string) ([]HistoryRun, *Validator, error) {
	history, err := v.store.History(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	expander, err := v.expander(ctx, target)
	if err != nil {
		return nil, nil, err
	}

	return history, expander, nil
}

func replayRuns(ctx context.Context, conn engine.DB, history []HistoryRun, searchPath string) (map[string]bool, error) {
	if err := mirrorSearchPath(ctx, conn, searchPath); err != nil {
		return nil, err
	}
	replayed := map[string]bool{}
	for i, run := range history {
		histPlans, err := historyPlans(run)
		if err != nil {
			return nil, fmt.Errorf("history run %d: %w", i, err)
		}
		if _, err := applyPlans(ctx, conn, engine.Options{}, recordUnexpanded(histPlans), nil, engine.WithAssertProbe()); err != nil {
			return nil, fmt.Errorf("replay history run %d: %w", i, err)
		}
		for _, p := range histPlans {
			replayed[p.Migration.ID()] = true
		}
	}

	return replayed, nil
}

// historyPlans rebuilds one run's up plans from its ledger: the migrations it applied, in the order it
// applied them, each carrying the expansion frozen on its own row.
func historyPlans(run HistoryRun) ([]engine.Plan, error) {
	out := make([]engine.Plan, 0, len(run.Migrations))
	for _, m := range run.Migrations {
		exps := map[string]Expansion{}
		if m.Expansion != nil {
			exps[m.ID] = *m.Expansion
		}
		plans, err := PlansFromFiles(
			map[string]string{m.ID + ".up.sql": m.UpSQL, m.ID + ".down.sql": m.DownSQL}, engine.DirectionUp)
		if err != nil {
			return nil, err
		}
		if plans, err = ExpandUp(plans, exps); err != nil {
			return nil, err
		}
		out = append(out, plans...)
	}

	return out, nil
}

// recordUnexpanded marks a history plan no run ever expanded — a baseline records without running — so the replay records it too.
func recordUnexpanded(plans []engine.Plan) []engine.Plan {
	for i, p := range plans {
		if len(p.Statements) == 0 && len(p.Migration.Directives) > 0 {
			plans[i].MarkOnly = true
		}
	}

	return plans
}

// expander applies the target's keep_old default over the service's; a directive can still override it.
func (v *Validator) expander(ctx context.Context, target string) (*Validator, error) {
	_, config, err := v.store.Target(ctx, target)
	if err != nil {
		return nil, err
	}
	if config[ConfigKeepOld] == "" {
		return v, nil
	}
	next := *v
	x := *v.Expander
	x.KeepOld = config[ConfigKeepOld] != "false"
	next.Expander = &x

	return &next, nil
}

// The scratch role is not the target's; without this, unqualified names land in a different schema than on the target.
func mirrorSearchPath(ctx context.Context, conn engine.DB, searchPath string) error {
	if searchPath == "" {
		return nil
	}
	schemas := strings.Split(searchPath, ",")
	stmts := make([]string, 0, len(schemas)+1)
	for i, schema := range schemas {
		schemas[i] = pgx.Identifier{schema}.Sanitize()
		if !strings.HasPrefix(schema, "pg_") {
			stmts = append(stmts, "CREATE SCHEMA IF NOT EXISTS "+schemas[i])
		}
	}
	for _, stmt := range append(stmts, "SET search_path TO "+strings.Join(schemas, ", ")) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("mirror search path: %w", err)
		}
	}

	return nil
}

// validateEach applies the plans in order, expanding each directive migration against the catalog the
// ones before it left behind, and snapshots the schema after every step.
func (v *Validator) validateEach(ctx context.Context, conn engine.DB, plans []engine.Plan, replayed map[string]bool) (Validation, error) {
	def, fp, err := snapshotScratch(ctx, conn)
	if err != nil {
		return Validation{}, fmt.Errorf("snapshot scratch database: %w", err)
	}
	val := Validation{Base: def, Fingerprints: []string{fp}, Expansions: map[string]Expansion{}, Plans: slices.Clone(plans)}
	for i, p := range val.Plans {
		if p, err = v.expandPlan(ctx, conn, p, val.Expansions, replayed); err != nil {
			return Validation{}, err
		}
		val.Plans[i] = p
		if _, err := applyPlans(ctx, conn, engine.Options{}, []engine.Plan{p}, nil, engine.WithAssertProbe()); err != nil {
			return Validation{}, fmt.Errorf("%w: %w", ErrValidationFailed, err)
		}
		next, nextFP, err := snapshotScratch(ctx, conn)
		if err != nil {
			return Validation{}, fmt.Errorf("snapshot scratch database: %w", err)
		}
		val.Effects = append(val.Effects, engine.DiffSchemas(def, next))
		val.Fingerprints = append(val.Fingerprints, nextFP)
		def = next
	}

	return val, nil
}

// expandPlan renders an up plan's directives; a down plan already carries the inverse its run froze, and a
// migration the history replayed keeps the expansion its own run froze.
func (v *Validator) expandPlan(ctx context.Context, conn engine.DB, p engine.Plan, into map[string]Expansion, replayed map[string]bool) (engine.Plan, error) {
	if len(p.Migration.Directives) == 0 || p.Direction != engine.DirectionUp || replayed[p.Migration.ID()] {
		return p, nil
	}
	exp, err := v.Expander.Expand(ctx, conn, p.Migration)
	if err != nil {
		return engine.Plan{}, err
	}
	into[exp.ID] = exp

	return ExpandPlan(p, exp)
}

// ExpandPlan rebuilds a plan from its frozen expansion; the migration keeps the checksum of the file,
// so the target records what the pull request carried and not what godwit generated from it.
func ExpandPlan(p engine.Plan, exp Expansion) (engine.Plan, error) {
	m := p.Migration
	m.UpSQL, m.DownSQL = exp.UpSQL, exp.DownSQL
	out, err := engine.BuildPlan(m, p.Direction)
	if err != nil {
		return engine.Plan{}, fmt.Errorf("%w: %s: expansion does not parse: %w", ErrDirective, exp.ID, err)
	}
	if p.Direction == engine.DirectionDown {
		// The whole body is generated, and godwit only generates an inverse it considers lossless, so
		// neither the hazard gate nor the data-loss gate has anything of the author's to speak about.
		for i := range out.Statements {
			out.Statements[i].Hazards, out.Statements[i].Drops = nil, nil
		}

		return out, nil
	}
	if len(out.Statements) != len(exp.Phase) {
		return engine.Plan{}, fmt.Errorf("%w: %s: expansion has %d statements, recorded %d",
			ErrDirective, exp.ID, len(out.Statements), len(exp.Phase))
	}
	for i := range out.Statements {
		st := &out.Statements[i]
		st.Phase = exp.Phase[i]
		if st.Phase == "" {
			continue
		}
		// godwit generated these; the hazard gate speaks about what the author wrote.
		st.Hazards = nil
		st.Assert = exp.assertAt(i)
		if b := exp.Batches[i]; b != nil {
			st.Batch, st.Verifier = b, engine.VerifierBatch
		}
	}

	return out, nil
}
