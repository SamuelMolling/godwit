package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
	"github.com/SamuelMolling/godwit/internal/notify"
)

// Drift is the outcome of comparing a target's live schema to its baseline.
type Drift struct {
	Target  string
	Drifted bool
	Diff    string
}

// DriftMonitor periodically compares live schemas against their baselines.
type DriftMonitor struct {
	// Metrics receives check outcomes; replace it before Run to share a registry.
	Metrics *metrics.Metrics

	store    *Store
	dsn      func(ctx context.Context, target string) (string, error)
	engine   Engine
	notifier notify.Notifier
	interval time.Duration
	log      *slog.Logger
}

// NewDriftMonitor wires a DriftMonitor; interval <= 0 defaults to 5 minutes.
func NewDriftMonitor(store *Store, sched *Scheduler, eng Engine, notifier notify.Notifier, interval time.Duration, log *slog.Logger) *DriftMonitor {
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	return &DriftMonitor{
		Metrics:  sched.Metrics,
		store:    store,
		dsn:      sched.targetDSN,
		engine:   eng,
		notifier: notifier,
		interval: interval,
		log:      log,
	}
}

// Run checks all targets on every interval until ctx is done.
func (m *DriftMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Tick(ctx)
		}
	}
}

// Tick checks every baselined target once.
func (m *DriftMonitor) Tick(ctx context.Context) {
	targets, err := m.store.SnapshotTargets(ctx)
	if err != nil {
		m.log.Error("drift tick failed", "error", err)

		return
	}
	for _, target := range targets {
		if _, err := m.Check(ctx, target); err != nil {
			m.log.Error("drift check failed", "target", target, "error", err)
		}
	}
}

// Check compares one target now and records the outcome.
func (m *DriftMonitor) Check(ctx context.Context, target string) (Drift, error) {
	expected, err := m.store.SnapshotFor(ctx, target)
	if err != nil {
		return Drift{}, err
	}
	dsn, err := m.dsn(ctx, target)
	if err != nil {
		return Drift{}, err
	}
	liveDef, liveFP, err := m.engine.Snapshot(ctx, dsn)
	if err != nil {
		return Drift{}, err
	}

	if liveFP == expected.Fingerprint {
		resolved, err := m.store.ResolveDrift(ctx, target)
		if err != nil {
			return Drift{}, err
		}
		m.Metrics.DriftChecked(target, metrics.DriftClean)
		m.log.Debug("drift checked", "target", target, "result", metrics.DriftClean)
		if resolved {
			m.log.Info("schema drift resolved", "target", target)
			notify.Emit(ctx, m.notifier, m.log, driftEvent(target, notify.DriftResolved, ""))
		}

		return Drift{Target: target}, nil
	}
	m.Metrics.DriftChecked(target, metrics.DriftDrifted)
	m.log.Info("drift checked", "target", target, "result", metrics.DriftDrifted)

	diff := strings.Join(engine.DiffSchemas(expected.Definition, liveDef), "\n")
	created, err := m.store.RecordDrift(ctx, target, diff)
	if err != nil {
		return Drift{}, err
	}
	if created {
		m.log.Warn("schema drift detected", "target", target)
		notify.Emit(ctx, m.notifier, m.log, driftEvent(target, notify.DriftDetected, diff))
	}

	return Drift{Target: target, Drifted: true, Diff: diff}, nil
}

// AcceptBaseline records the live schema as the expected state and resolves open drift.
func (m *DriftMonitor) AcceptBaseline(ctx context.Context, target string) error {
	dsn, err := m.dsn(ctx, target)
	if err != nil {
		return err
	}
	def, fp, err := m.engine.Snapshot(ctx, dsn)
	if err != nil {
		return fmt.Errorf("snapshot live schema: %w", err)
	}
	m.Metrics.DriftChecked(target, metrics.DriftAccepted)
	m.log.Info("baseline accepted", "target", target)
	if err := m.store.SaveSnapshot(ctx, target, fp, def, ""); err != nil {
		return err
	}
	notify.Emit(ctx, m.notifier, m.log, driftEvent(target, notify.DriftAccepted, ""))

	return nil
}

func driftEvent(target, typ, detail string) notify.Event {
	return notify.Event{Kind: notify.KindDrift, Type: typ, Target: target, Detail: detail, At: time.Now()}
}
