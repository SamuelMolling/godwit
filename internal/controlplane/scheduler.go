package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"testing/fstest"
	"time"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/engine"
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
// Crash recovery is the claim itself: a running run whose lease expired is
// claimed again by any replica and resumed from the target's journal.
type Scheduler struct {
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
	if run.Attempts > s.cfg.MaxAttempts {
		log.Error("resume budget exhausted; parking")
		_ = s.store.Finish(ctx, run.ID, StateNeedsAttention,
			fmt.Sprintf("gave up after %d attempts", run.Attempts-1))

		return
	}

	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go s.heartbeat(hbCtx, run.ID)

	held, err := s.applyRun(ctx, run)
	if err != nil {
		log.Error("run failed", "error", err)
		_ = s.store.Finish(ctx, run.ID, StateFailed, err.Error())

		return
	}
	// Baseline first: when watchers see the final state the snapshot must exist.
	s.baseline(ctx, run, log)
	if held > 0 {
		log.Info("expand phase applied; awaiting rollout confirmation", "held", held)
		_ = s.store.Finish(ctx, run.ID, StateAwaitingContract, "")

		return
	}
	log.Info("run succeeded")
	_ = s.store.Finish(ctx, run.ID, StateSucceeded, "")
}

// baseline records the post-migration schema as the target's expected state.
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

// applyRun applies the run's plans for its current phase and reports how
// many migrations the rollout policy held back for the contract phase.
func (s *Scheduler) applyRun(ctx context.Context, run Run) (int, error) {
	files, err := s.store.RunFiles(ctx, run.ID)
	if err != nil {
		return 0, err
	}
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	migs, err := engine.LoadFS(fsys)
	if err != nil {
		return 0, err
	}
	plans, err := buildPlans(migs)
	if err != nil {
		return 0, err
	}
	held := 0
	if run.Phase != PhaseContract {
		policy, ok := s.policies[run.Rollout]
		if !ok {
			return 0, fmt.Errorf("unknown rollout policy %q", run.Rollout)
		}
		var contract []engine.Plan
		plans, contract = policy.Split(plans)
		held = len(contract)
	}

	dsn, err := s.targetDSN(ctx, run.Target)
	if err != nil {
		return 0, err
	}

	return held, s.engine.Apply(ctx, dsn, plans)
}

func (s *Scheduler) targetDSN(ctx context.Context, target string) (string, error) {
	providerName, config, err := s.store.Target(ctx, target)
	if err != nil {
		return "", err
	}
	provider, ok := s.providers[providerName]
	if !ok {
		return "", fmt.Errorf("unknown credential provider %q", providerName)
	}

	return provider.DSN(ctx, config)
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

				return
			}
		}
	}
}
