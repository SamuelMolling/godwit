package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
	"github.com/SamuelMolling/godwit/internal/notify"
)

// Config tunes a Scheduler.
type Config struct {
	Holder      string
	TTL         time.Duration
	Interval    time.Duration
	MaxAttempts int
	// Jitter returns a value in [0, 1) that spreads retry backoff; nil means random.
	Jitter func() float64
}

func (c Config) withDefaults() Config {
	if c.TTL <= 0 {
		c.TTL = 30 * time.Second
	}
	if c.Interval <= 0 {
		c.Interval = 2 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.Jitter == nil {
		c.Jitter = defaultJitter
	}

	return c
}

// Scheduler claims runnable runs and executes them under a heartbeated lease.
type Scheduler struct {
	// Metrics receives run events; replace it before Run to share a registry.
	Metrics *metrics.Metrics
	// Notifier receives run lifecycle events; replace it before Run.
	Notifier notify.Notifier

	store     *Store
	providers map[string]creds.Provider
	engine    Engine
	policies  map[string]RolloutPolicy
	cfg       Config
	log       *slog.Logger
}

// NewScheduler wires a Scheduler.
func NewScheduler(store *Store, providers map[string]creds.Provider, eng Engine, policies map[string]RolloutPolicy, cfg Config, log *slog.Logger) *Scheduler {
	return &Scheduler{
		Metrics:   metrics.New(),
		Notifier:  notify.None{},
		store:     store,
		providers: providers,
		engine:    eng,
		policies:  policies,
		cfg:       cfg.withDefaults(),
		log:       log,
	}
}

// Run polls for work until ctx is done.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick claims and executes at most one run.
func (s *Scheduler) Tick(ctx context.Context) {
	run, ok, err := s.store.Claim(ctx, s.cfg.Holder, s.cfg.TTL)
	if err != nil {
		s.log.Error("claim failed", "error", err)

		return
	}
	if !ok {
		return
	}
	s.execute(ctx, run)
}

func (s *Scheduler) execute(ctx context.Context, run Run) {
	log := s.log.With("run", run.ID, "target", run.Target, "attempt", run.Attempts)
	log.Info("run claimed", "rollout", run.Rollout, "phase", run.Phase, "reverts", run.Reverts)
	s.Metrics.RunClaimed(run.Target, run.Attempts)
	run.State = StateRunning
	notify.Emit(ctx, s.Notifier, log, RunEvent(run, notify.RunRunning, ""))
	start := time.Now()
	finish := func(state, errText string) {
		_ = s.store.Finish(ctx, run.ID, state, errText)
		d := time.Since(start)
		s.Metrics.RunFinished(run.Target, state, run.Attempts, d)
		run.State = state
		notify.Emit(ctx, s.Notifier, log, RunEvent(run, state, errText))
		if state == StateSucceeded && run.Reverts != "" {
			orig := Run{ID: run.Reverts, Target: run.Target, State: StateReverted}
			notify.Emit(ctx, s.Notifier, log, RunEvent(orig, notify.RunReverted, "reverted by run "+notify.ShortID(run.ID)))
		}
		attrs := []any{"state", state, "duration_ms", d.Milliseconds()}
		if errText != "" {
			log.Error("run finished", append(attrs, "error", errText)...)

			return
		}
		log.Info("run finished", attrs...)
	}
	if run.Attempts > s.cfg.MaxAttempts {
		finish(StateNeedsAttention, fmt.Sprintf("gave up after %d attempts", run.Attempts-1))

		return
	}

	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go s.heartbeat(hbCtx, run.ID)

	held, err := s.applyRun(ctx, run)
	if err != nil {
		s.fail(ctx, run, err, finish)

		return
	}
	// Watchers must find the snapshot once they see the final state.
	s.baseline(ctx, run, log)
	if held.plans == 0 {
		s.retire(ctx, run, log)
	}
	if held.plans > 0 {
		log.Info("expand phase applied; awaiting rollout confirmation", "held", held.plans, "held_statements", held.statements)
		finish(StateAwaitingContract, "")

		return
	}
	finish(StateSucceeded, "")
}

func (s *Scheduler) fail(ctx context.Context, run Run, err error, finish func(state, errText string)) {
	code, transient := classify(err)
	if !transient {
		finish(StateFailed, FailureDetail(err))

		return
	}
	if run.Attempts >= s.cfg.MaxAttempts {
		finish(StateNeedsAttention, fmt.Sprintf("transient: gave up after %d attempts: %v", run.Attempts, err))

		return
	}
	wait := Backoff(s.cfg.Interval, run.Attempts, s.cfg.Jitter)
	detail := retryDetail(err, wait)
	log := s.log.With("run", run.ID, "target", run.Target, "attempt", run.Attempts)
	if err := s.store.Retry(ctx, run.ID, FailureDetail(err), wait); err != nil {
		log.Error("retry not recorded; lease expiry will requeue the run", "error", err)

		return
	}
	s.Metrics.RunRetried(run.Target, code)
	run.State = StateQueued
	notify.Emit(ctx, s.Notifier, log, RunEvent(run, notify.RunRetrying, detail))
	log.Warn("run retrying", "wait", wait, "error", err)
}

// RunEvent builds the notification for a run at its current state.
func RunEvent(r Run, typ, detail string) notify.Event {
	return notify.Event{
		Kind:    notify.KindRun,
		Type:    typ,
		Target:  r.Target,
		RunID:   r.ID,
		State:   r.State,
		Attempt: r.Attempts,
		Rollout: r.Rollout,
		Phase:   r.Phase,
		Actor:   r.Provenance.CreatedBy,
		Detail:  detail,
		At:      time.Now(),
	}
}

func (s *Scheduler) baseline(ctx context.Context, run Run, log *slog.Logger) {
	dsn, err := s.targetDSN(ctx, run.Target)
	if err != nil {
		log.Warn("baseline skipped", "error", err)

		return
	}
	def, fp, err := s.engine.Snapshot(ctx, dsn)
	if err != nil {
		log.Warn("baseline snapshot failed", "error", err)

		return
	}
	if err := s.store.SaveSnapshot(ctx, run.Target, fp, def, run.ID); err != nil {
		log.Warn("baseline save failed", "error", err)
	}
}

// heldWork is what the expand phase left for the contract phase.
type heldWork struct {
	plans      int
	statements int
}

// applyRun applies the current phase and reports what it held back.
func (s *Scheduler) applyRun(ctx context.Context, run Run) (heldWork, error) {
	plans, err := s.plans(ctx, run)
	if err != nil {
		return heldWork{}, err
	}
	var held heldWork
	if run.Reverts == "" && run.Phase != PhaseContract {
		policy, ok := s.policies[run.Rollout]
		if !ok {
			return heldWork{}, fmt.Errorf("unknown rollout policy %q", run.Rollout)
		}
		var contract []engine.Plan
		plans, contract = policy.Split(plans)
		held = heldWork{plans: len(contract), statements: HeldStatements(plans, contract)}
	}

	tg, err := s.target(ctx, run.Target)
	if err != nil {
		return heldWork{}, err
	}
	opts, err := run.Timeouts.Over(tg.timeouts).Options()
	if err != nil {
		return heldWork{}, err
	}
	if run.Reverts == "" && run.PlanID != "" {
		if plans, err = s.markOnly(ctx, run.PlanID, plans, tg.dsn); err != nil {
			return heldWork{}, err
		}
	}

	return held, s.engine.Apply(ctx, ApplyRequest{
		RunID: run.ID, Target: run.Target, DSN: tg.dsn, Plans: plans, Opts: opts,
		Progress: s.progress(ctx, run.ID), Record: s.record(run),
	})
}

// plans is what the run executes: the up side of its own files with the directive expansions frozen
// when it was created, or, for a revert, the down side of what the run it undoes actually applied.
func (s *Scheduler) plans(ctx context.Context, run Run) ([]engine.Plan, error) {
	if run.Reverts != "" {
		rp, err := s.store.PlanRevert(ctx, run.Reverts)
		if err != nil {
			return nil, err
		}

		return rp.Plans, nil
	}
	files, err := s.store.RunFiles(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	plans, err := PlansFromFiles(files, engine.DirectionUp)
	if err != nil {
		return nil, err
	}

	return ExpandUp(plans, run.Expansions)
}

// record keeps the ledger of what the run actually applied, statement by statement, so a later revert
// acts on that and not on the directory the run submitted.
func (s *Scheduler) record(run Run) Recorder {
	return func(ctx context.Context, res engine.Result) error {
		if res.Skipped {
			return nil
		}
		if run.Reverts != "" {
			return s.store.MarkReverted(ctx, run.Reverts, run.ID, res.Migration)
		}
		var exp *Expansion
		if e, ok := run.Expansions[res.Migration]; ok {
			exp = &e
		}

		return s.store.RecordApplied(ctx, run.ID, res.Migration, res.Held, exp)
	}
}

// progressEvery bounds the writes a fast backfill produces; the end of a statement is always recorded.
const progressEvery = time.Second

// progress records the newest statement event under the heartbeat, so a backfill that runs for an hour
// is visible without notifying once per batch.
func (s *Scheduler) progress(ctx context.Context, runID string) func(engine.StatementEvent) {
	var last time.Time

	return func(ev engine.StatementEvent) {
		now := time.Now()
		if ev.Partial && now.Sub(last) < progressEvery {
			return
		}
		last = now
		p := RunProgress{
			Migration: ev.Migration, Statement: ev.Index, Phase: ev.Statement.Phase,
			RowsDone: ev.RowsDone, RowsTotal: ev.RowsTotal, Batches: ev.Batches,
		}
		if err := s.store.SaveProgress(ctx, runID, p); err != nil {
			s.log.Warn("progress not recorded", "run", runID, "error", err)
		}
	}
}

// retire records the columns a completed run left behind as its rollback, and clears the ones a revert
// just renamed back, so a desired-schema diff stops proposing to drop them.
func (s *Scheduler) retire(ctx context.Context, run Run, log *slog.Logger) {
	exps, err := s.expansions(ctx, run)
	if err != nil {
		log.Warn("retired columns skipped", "error", err)

		return
	}
	for migration, cols := range Retired(exps) {
		if err := s.retired(ctx, run, migration, cols); err != nil {
			log.Warn("retired columns not recorded", "migration", migration, "error", err)
		}
	}
	s.unretire(ctx, run, exps, log)
}

// unretire clears the columns a drop-column just removed; a revert has none to clear, since godwit
// generates no inverse that would put a dropped column back.
func (s *Scheduler) unretire(ctx context.Context, run Run, exps map[string]Expansion, log *slog.Logger) {
	cols := Unretired(exps)
	if run.Reverts != "" || len(cols) == 0 {
		return
	}
	if err := s.store.UnretireColumns(ctx, run.Target, cols); err != nil {
		log.Warn("dropped columns still recorded as retired", "error", err)
	}
}

// expansions are the ones this run is accountable for: its own going up, and the ones it just undid
// going down, each read back from the ledger row of the migration it belongs to.
func (s *Scheduler) expansions(ctx context.Context, run Run) (map[string]Expansion, error) {
	if run.Reverts == "" {
		return run.Expansions, nil
	}
	rows, err := s.store.AppliedMigrations(ctx, run.Reverts)
	if err != nil {
		return nil, err
	}
	out := map[string]Expansion{}
	for _, m := range rows {
		if m.RevertedBy == run.ID && m.Expansion != nil {
			out[m.Migration] = *m.Expansion
		}
	}

	return out, nil
}

func (s *Scheduler) retired(ctx context.Context, run Run, migration string, cols []RetiredColumn) error {
	if run.Reverts != "" {
		return s.store.UnretireColumns(ctx, run.Target, cols)
	}

	return s.store.RetireColumns(ctx, run.Target, run.ID, migration, cols)
}

func (s *Scheduler) markOnly(ctx context.Context, planID string, plans []engine.Plan, dsn string) ([]engine.Plan, error) {
	plan, err := s.store.Plan(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("bound plan %s: %w", planID, err)
	}
	marked := map[string]bool{}
	for _, m := range plan.Migrations {
		if m.AlreadyApplied {
			marked[m.ID()] = true
		}
	}
	if len(marked) == 0 {
		return plans, nil
	}
	obs, err := s.engine.Observe(ctx, dsn)
	if err != nil {
		return nil, err
	}
	pending := false
	for i := range plans {
		if !marked[plans[i].Migration.ID()] {
			continue
		}
		plans[i].MarkOnly = true
		pending = pending || !recordedOn(obs, plans[i].Migration)
	}
	if pending && obs.Fingerprint != plan.SchemaFingerprint {
		return nil, fmt.Errorf("target schema changed since plan %s was taken; re-plan before recording %d migration(s) as already applied", planID, len(marked))
	}

	return plans, nil
}

func recordedOn(obs Observation, m engine.Migration) bool {
	if m.Repeatable {
		return slices.ContainsFunc(obs.Repeatables, func(r engine.Repeatable) bool {
			return r.Name == m.Name && r.Checksum == m.Checksum
		})
	}

	return slices.ContainsFunc(obs.Applied, func(a engine.Applied) bool { return a.Version == m.Version })
}

func (s *Scheduler) targetDSN(ctx context.Context, target string) (string, error) {
	tg, err := s.target(ctx, target)

	return tg.dsn, err
}

type resolvedTarget struct {
	dsn        string
	provider   string
	timeouts   Timeouts
	searchPath string
}

func (s *Scheduler) target(ctx context.Context, name string) (resolvedTarget, error) {
	providerName, config, err := s.store.Target(ctx, name)
	if err != nil {
		return resolvedTarget{}, err
	}
	provider, ok := s.providers[providerName]
	if !ok {
		return resolvedTarget{}, fmt.Errorf("unknown credential provider %q", providerName)
	}
	searchPath, err := ParseSearchPath(config[ConfigSearchPath])
	if err != nil {
		return resolvedTarget{}, err
	}
	dsn, err := provider.DSN(ctx, config)
	if err != nil {
		return resolvedTarget{}, err
	}

	return resolvedTarget{
		dsn: dsnWithSearchPath(dsn, searchPath), provider: providerName,
		timeouts: TargetTimeouts(config), searchPath: searchPath,
	}, nil
}

func (s *Scheduler) heartbeat(ctx context.Context, runID string) {
	ticker := time.NewTicker(s.cfg.TTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.store.Heartbeat(ctx, runID, s.cfg.Holder, s.cfg.TTL); err != nil {
				s.log.Warn("heartbeat lost", "run", runID, "error", err)
				s.Metrics.HeartbeatFailed()

				return
			}
		}
	}
}
