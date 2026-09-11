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
	Expander *Expander

	scratch *Scratch
	store   *Store
	newID   func() string
	scope   engine.SnapshotScope
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
	Changes      [][]engine.ObjectChange
	Expansions   map[string]Expansion
	Plans        []engine.Plan
	Replayed     int
	Collapsed    int
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

	session := engine.NewSession(conn)
	st, err := replayRuns(ctx, session, history, searchPath)
	if err != nil {
		return Validation{}, err
	}
	if plans, err = engine.ShapeCheckpoint(plans, st.newest); err != nil {
		return Validation{}, err
	}
	val, err := expander.validateEach(ctx, session, plans, st.seen)
	val.Replayed, val.Collapsed = st.replayed, st.collapsed

	return val, err
}

// Replay rebuilds target's recorded history on conn and applies plans on top; a migration the history already covers keeps its own expansion.
func (v *Validator) Replay(ctx context.Context, conn engine.DB, target, searchPath string, plans []engine.Plan) error {
	history, expander, err := v.historyOf(ctx, target)
	if err != nil {
		return err
	}
	conn = engine.NewSession(conn)
	st, err := replayRuns(ctx, conn, history, searchPath)
	if err != nil {
		return err
	}
	if plans, err = engine.ShapeCheckpoint(plans, st.newest); err != nil {
		return err
	}
	for _, p := range plans {
		if p, err = expander.expandPlan(ctx, conn, p, map[string]Expansion{}, st.seen); err != nil {
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

type replayState struct {
	seen      map[string]bool
	newest    int64
	replayed  int
	collapsed int
}

func (s *replayState) add(m engine.Migration) {
	s.seen[m.ID()] = true
	if !m.Repeatable && m.Version > s.newest {
		s.newest = m.Version
	}
}

type historyStep struct {
	plan engine.Plan
	run  int
}

func replayRuns(ctx context.Context, conn engine.DB, history []HistoryRun, searchPath string) (replayState, error) {
	st := replayState{seen: map[string]bool{}}
	if err := mirrorSearchPath(ctx, conn, searchPath); err != nil {
		return st, err
	}
	steps, err := historySteps(history)
	if err != nil {
		return st, err
	}
	ordered, collapsed := collapseAtCheckpoint(steps)
	migs := make([]engine.Migration, 0, len(collapsed))
	for _, s := range collapsed {
		st.add(s.plan.Migration)
		migs = append(migs, s.plan.Migration)
	}
	st.collapsed = len(migs)
	if err := engine.RecordCollapsed(ctx, conn, migs); err != nil {
		return st, err
	}
	for _, s := range ordered {
		if _, err := applyPlans(ctx, conn, engine.Options{}, recordUnexpanded([]engine.Plan{s.plan}), nil, engine.WithAssertProbe()); err != nil {
			return st, fmt.Errorf("replay history run %d: %w", s.run, err)
		}
		st.add(s.plan.Migration)
		st.replayed++
	}

	return st, nil
}

func historySteps(history []HistoryRun) ([]historyStep, error) {
	var out []historyStep
	for i, run := range history {
		plans, err := historyPlans(run)
		if err != nil {
			return nil, fmt.Errorf("history run %d: %w", i, err)
		}
		for _, p := range plans {
			out = append(out, historyStep{plan: p, run: i})
		}
	}

	return out, nil
}

func collapseAtCheckpoint(steps []historyStep) (ordered, collapsed []historyStep) {
	at := -1
	for i, s := range steps {
		if s.plan.Migration.Checkpoint && (at < 0 || s.plan.Migration.Version >= steps[at].plan.Migration.Version) {
			at = i
		}
	}
	if at < 0 {
		return steps, nil
	}
	cp := steps[at].plan.Migration
	ordered = append(ordered, steps[at])
	for i, s := range steps {
		switch {
		case i == at:
		case s.plan.Migration.Collapses(cp):
			collapsed = append(collapsed, s)
		default:
			ordered = append(ordered, s)
		}
	}

	return ordered, collapsed
}

func historyPlans(run HistoryRun) ([]engine.Plan, error) {
	out := make([]engine.Plan, 0, len(run.Migrations))
	for _, m := range run.Migrations {
		exps := map[string]Expansion{}
		if m.Expansion != nil {
			exps[m.ID] = *m.Expansion
		}
		plans, err := PlansFromFiles(pairOf(m.ID, m.UpSQL, m.DownSQL), engine.DirectionUp)
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

func recordUnexpanded(plans []engine.Plan) []engine.Plan {
	for i, p := range plans {
		if len(p.Statements) == 0 && len(p.Migration.Directives) > 0 {
			plans[i].MarkOnly = true
		}
	}

	return plans
}

func (v *Validator) expander(ctx context.Context, target string) (*Validator, error) {
	_, config, err := v.store.Target(ctx, target)
	if err != nil {
		return nil, err
	}
	next := *v
	next.scope = snapshotScopeOf(config)
	if config[ConfigKeepOld] == "" {
		return &next, nil
	}
	x := *v.Expander
	x.KeepOld = config[ConfigKeepOld] != "false"
	next.Expander = &x

	return &next, nil
}

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

func (v *Validator) validateEach(ctx context.Context, conn engine.DB, plans []engine.Plan, replayed map[string]bool) (Validation, error) {
	base, err := snapshotScratch(ctx, conn, v.scope)
	if err != nil {
		return Validation{}, fmt.Errorf("snapshot scratch database: %w", err)
	}
	def := base.Definition
	val := Validation{Base: def, Fingerprints: []string{base.Fingerprint}, Expansions: map[string]Expansion{}, Plans: slices.Clone(plans)}
	for i, p := range val.Plans {
		if p, err = v.expandPlan(ctx, conn, p, val.Expansions, replayed); err != nil {
			return Validation{}, err
		}
		val.Plans[i] = p
		if _, err := applyPlans(ctx, conn, engine.Options{}, []engine.Plan{p}, nil, engine.WithAssertProbe()); err != nil {
			return Validation{}, fmt.Errorf("%w: %w", ErrValidationFailed, err)
		}
		next, err := snapshotScratch(ctx, conn, v.scope)
		if err != nil {
			return Validation{}, fmt.Errorf("snapshot scratch database: %w", err)
		}
		val.Effects = append(val.Effects, engine.DiffSchemas(def, next.Definition))
		val.Changes = append(val.Changes, engine.SchemaChanges(def, next.Definition))
		val.Fingerprints = append(val.Fingerprints, next.Fingerprint)
		def = next.Definition
	}

	return val, nil
}

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

// ExpandPlan rebuilds a plan from its frozen expansion; the migration keeps the checksum of the file, not of what godwit generated from it.
func ExpandPlan(p engine.Plan, exp Expansion) (engine.Plan, error) {
	m := p.Migration
	m.UpSQL, m.DownSQL = exp.UpSQL, exp.DownSQL
	out, err := engine.BuildPlan(m, p.Direction)
	if err != nil {
		return engine.Plan{}, fmt.Errorf("%w: %s: expansion does not parse: %w", ErrDirective, exp.ID, err)
	}
	if p.Direction == engine.DirectionDown {
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
		st.Hazards = nil
		st.Assert = exp.assertAt(i)
		if b := exp.Batches[i]; b != nil {
			st.Batch, st.Verifier = b, engine.VerifierBatch
		}
	}

	return out, nil
}
