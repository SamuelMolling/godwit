package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// ErrValidationFailed marks a migration the author must fix.
var ErrValidationFailed = errors.New("migration failed validation")

var connectScratch = pgx.ConnectConfig

// Validator replays a target's history on a scratch database and applies new plans on top.
type Validator struct {
	pool  *pgxpool.Pool
	store *Store
	newID func() string
}

// NewValidator wires a Validator over the control-plane pool.
func NewValidator(pool *pgxpool.Pool, store *Store, newID func() string) *Validator {
	return &Validator{pool: pool, store: store, newID: newID}
}

// Validate reports the first plan that fails on a scratch copy of the target.
func (v *Validator) Validate(ctx context.Context, target string, plans []engine.Plan) error {
	history, err := v.store.HistoryFiles(ctx, target)
	if err != nil {
		return err
	}

	name := "godwit_validate_" + v.newID()
	if _, err := v.pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("create scratch database: %w", err)
	}
	defer func() {
		_, _ = v.pool.Exec(context.WithoutCancel(ctx),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}()

	cfg := v.pool.Config().ConnConfig.Copy()
	cfg.Database = name
	conn, err := connectScratch(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect scratch database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	for i, files := range history {
		histPlans, err := PlansFromFiles(files, engine.DirectionUp)
		if err != nil {
			return fmt.Errorf("history run %d: %w", i, err)
		}
		if _, err := applyPlans(ctx, conn, engine.Options{}, histPlans); err != nil {
			return fmt.Errorf("replay history run %d: %w", i, err)
		}
	}

	if _, err := applyPlans(ctx, conn, engine.Options{}, plans); err != nil {
		return fmt.Errorf("%w: %w", ErrValidationFailed, err)
	}

	return nil
}
