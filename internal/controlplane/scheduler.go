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
	policy    RolloutPolicy
	cfg       Config
	log       *slog.Logger
}

// NewScheduler wires a Scheduler.
func NewScheduler(store *Store, providers map[string]creds.Provider, eng Engine, policy RolloutPolicy, cfg Config, log *slog.Logger) *Scheduler {
	return &Scheduler{
		store:     store,
		providers: providers,
		engine:    eng,
		policy:    policy,
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

	if err := s.applyRun(ctx, run); err != nil {
		log.Error("run failed", "error", err)
		_ = s.store.Finish(ctx, run.ID, StateFailed, err.Error())

		return
	}
	log.Info("run succeeded")
	_ = s.store.Finish(ctx, run.ID, StateSucceeded, "")
}

func (s *Scheduler) applyRun(ctx context.Context, run Run) error {
	files, err := s.store.RunFiles(ctx, run.ID)
	if err != nil {
		return err
	}
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	migs, err := engine.LoadFS(fsys)
	if err != nil {
		return err
	}
	plans, err := buildPlans(migs)
	if err != nil {
		return err
	}
	for _, p := range plans {
		if err := s.policy.Allow(p); err != nil {
			return fmt.Errorf("rollout policy: %w", err)
		}
	}

	providerName, config, err := s.store.Target(ctx, run.Target)
	if err != nil {
		return err
	}
	provider, ok := s.providers[providerName]
	if !ok {
		return fmt.Errorf("unknown credential provider %q", providerName)
	}
	dsn, err := provider.DSN(ctx, config)
	if err != nil {
		return err
	}

	return s.engine.Apply(ctx, dsn, plans)
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
