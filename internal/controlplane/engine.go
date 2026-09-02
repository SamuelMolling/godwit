package controlplane

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
	"github.com/SamuelMolling/godwit/internal/metrics"
)

// Engine applies plans to a target database and inspects its schema.
type Engine interface {
	Apply(ctx context.Context, target, dsn string, plans []engine.Plan) error
	Snapshot(ctx context.Context, dsn string) (definition, fingerprint string, err error)
}

// PGEngine is the PostgreSQL Engine; Metrics is optional.
type PGEngine struct {
	Opts    engine.Options
	Metrics *metrics.Metrics
}

// Apply implements Engine.
func (e PGEngine) Apply(ctx context.Context, target, dsn string, plans []engine.Plan) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect target: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	return applyPlans(ctx, conn, e.Opts, plans, engine.WithObserver(e.observer(target)))
}

func (e PGEngine) observer(target string) func(engine.StatementEvent) {
	if e.Metrics == nil {
		return func(engine.StatementEvent) {}
	}

	return func(ev engine.StatementEvent) {
		kind := "tx"
		if ev.Statement.NoTx {
			kind = "no_tx"
		}
		e.Metrics.Statement(target, kind, ev.Duration, ev.Err)
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
