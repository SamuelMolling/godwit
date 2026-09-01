package controlplane

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// Engine applies plans to a target database and inspects its schema.
type Engine interface {
	Apply(ctx context.Context, dsn string, plans []engine.Plan) error
	Snapshot(ctx context.Context, dsn string) (definition, fingerprint string, err error)
}

// PGEngine is the PostgreSQL Engine.
type PGEngine struct {
	Opts engine.Options
}

// Apply implements Engine.
func (e PGEngine) Apply(ctx context.Context, dsn string, plans []engine.Plan) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect target: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	return applyPlans(ctx, conn, e.Opts, plans)
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
