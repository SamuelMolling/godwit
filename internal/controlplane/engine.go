package controlplane

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
)

// ApplyRequest is one run's worth of plans plus the effective execution options.
type ApplyRequest struct {
	RunID  string
	Target string
	DSN    string
	Plans  []engine.Plan
	Opts   engine.Options
}

// Engine applies plans to a target database, marks migrations applied and inspects its schema.
type Engine interface {
	Apply(ctx context.Context, req ApplyRequest) error
	MarkApplied(ctx context.Context, dsn string, migs []engine.Migration) error
	Snapshot(ctx context.Context, dsn string) (definition, fingerprint string, err error)
}

// PGEngine is the PostgreSQL Engine; Metrics and Log are optional.
type PGEngine struct {
	Metrics *metrics.Metrics
	Log     *slog.Logger
}

// Apply implements Engine.
func (e PGEngine) Apply(ctx context.Context, req ApplyRequest) error {
	conn, err := pgx.Connect(ctx, req.DSN)
	if err != nil {
		return fmt.Errorf("connect target: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	_, err = applyPlans(ctx, conn, req.Opts, req.Plans, engine.WithObserver(e.observer(req.RunID, req.Target)))

	return err
}

func (e PGEngine) observer(runID, target string) func(engine.StatementEvent) {
	log := e.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return func(ev engine.StatementEvent) {
		kind := "tx"
		if ev.Statement.NoTx {
			kind = "no_tx"
		}
		if e.Metrics != nil {
			e.Metrics.Statement(target, kind, ev.Duration, ev.Err)
		}
		attrs := []any{
			"run", runID, "target", target, "version", ev.Version, "stmt", ev.Index,
			"kind", kind, "duration_ms", ev.Duration.Milliseconds(),
		}
		if ev.Err != nil {
			log.Error("statement failed", append(attrs, "error", ev.Err.Error())...)

			return
		}
		log.Info("statement applied", attrs...)
	}
}

// MarkApplied implements Engine.
func (PGEngine) MarkApplied(ctx context.Context, dsn string, migs []engine.Migration) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect target: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	return engine.New(conn, engine.Options{}).MarkApplied(ctx, migs)
}

// Snapshot implements Engine.
func (PGEngine) Snapshot(ctx context.Context, dsn string) (string, string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", "", fmt.Errorf("connect target: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	return engine.Snapshot(ctx, conn)
}
