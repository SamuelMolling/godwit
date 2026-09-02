package controlplane

import (
	"context"
	"fmt"
	"log/slog"
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
}

func (c Config) withDefaults() Config {
	if c.TTL <= 0 {
		c.TTL = 30 * time.Second
	}
	if c.Interval <= 0 {
		c.Interval = 2 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
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
		finish(StateFailed, err.Error())

		return
	}
	// Watchers must find the snapshot once they see the final state.
	s.baseline(ctx, run, log)
	if held > 0 {
		log.Info("expand phase applied; awaiting rollout confirmation", "held", held)
		finish(StateAwaitingContract, "")

		return
	}
	finish(StateSucceeded, "")
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

// applyRun applies the current phase and reports how many plans were held back.
func (s *Scheduler) applyRun(ctx context.Context, run Run) (int, error) {
	filesID, dir := run.ID, engine.DirectionUp
	if run.Reverts != "" {
		filesID, dir = run.Reverts, engine.DirectionDown
	}
	files, err := s.store.RunFiles(ctx, filesID)
	if err != nil {
		return 0, err
	}
	plans, err := PlansFromFiles(files, dir)
	if err != nil {
		return 0, err
	}
	held := 0
	if run.Reverts == "" && run.Phase != PhaseContract {
		policy, ok := s.policies[run.Rollout]
		if !ok {
			return 0, fmt.Errorf("unknown rollout policy %q", run.Rollout)
		}
		var contract []engine.Plan
		plans, contract = policy.Split(plans)
		held = len(contract)
	}

	dsn, defaults, err := s.target(ctx, run.Target)
	if err != nil {
		return 0, err
	}
	opts, err := run.Timeouts.Over(defaults).Options()
	if err != nil {
		return 0, err
	}

	return held, s.engine.Apply(ctx, ApplyRequest{RunID: run.ID, Target: run.Target, DSN: dsn, Plans: plans, Opts: opts})
}

func (s *Scheduler) targetDSN(ctx context.Context, target string) (string, error) {
	dsn, _, err := s.target(ctx, target)

	return dsn, err
}

func (s *Scheduler) target(ctx context.Context, name string) (string, Timeouts, error) {
	providerName, config, err := s.store.Target(ctx, name)
	if err != nil {
		return "", Timeouts{}, err
	}
	provider, ok := s.providers[providerName]
	if !ok {
		return "", Timeouts{}, fmt.Errorf("unknown credential provider %q", providerName)
	}
	dsn, err := provider.DSN(ctx, config)

	return dsn, TargetTimeouts(config), err
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
