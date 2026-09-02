package controlplane

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
)

// Engine applies plans to a target database and inspects its schema.
type Engine interface {
	Apply(ctx context.Context, target, dsn string, plans []engine.Plan) error
	Snapshot(ctx context.Context, dsn string) (definition, fingerprint string, err error)
}

// PGEngine is the PostgreSQL Engine; Metrics and Log are optional.
type PGEngine struct {
	Opts    engine.Options
	Metrics *metrics.Metrics
	Log     *slog.Logger
}

// Apply implements Engine.
func (e PGEngine) Apply(ctx context.Context, target, dsn string, plans []engine.Plan) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect target: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	_, err = applyPlans(ctx, conn, e.Opts, plans, engine.WithObserver(e.observer(target)))

	return err
}

func (e PGEngine) observer(target string) func(engine.StatementEvent) {
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
			"target", target, "version", ev.Version, "stmt", ev.Index,
			"kind", kind, "duration_ms", ev.Duration.Milliseconds(),
		}
		if ev.Err != nil {
			log.Error("statement failed", append(attrs, "error", ev.Err.Error())...)

			return
		}
		log.Info("statement applied", attrs...)
	}
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
