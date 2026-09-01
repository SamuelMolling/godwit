package controlplane

import (
	"context"
	"errors"
	"fmt"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// ErrValidationFailed marks a migration the author must fix, as opposed to
// validator infrastructure trouble.
var ErrValidationFailed = errors.New("migration failed validation")

var connectScratch = pgx.ConnectConfig

// Validator proves a migration actually runs before it touches a real target:
// it replays the target's succeeded-run history on a scratch database created
// on the control-plane server, applies the new plans on top, and drops the
// scratch (pg-schema-diff's temp-database validation idea, made incremental).
type Validator struct {
	pool  *pgxpool.Pool
	store *Store
	newID func() string
}

// NewValidator wires a Validator over the control-plane pool.
func NewValidator(pool *pgxpool.Pool, store *Store, newID func() string) *Validator {
	return &Validator{pool: pool, store: store, newID: newID}
}

// Validate rebuilds the target's history on a scratch database and applies
// plans on top, reporting the first failure.
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
		histPlans, err := plansFromFiles(files)
		if err != nil {
			return fmt.Errorf("history run %d: %w", i, err)
		}
		if err := applyPlans(ctx, conn, engine.Options{}, histPlans); err != nil {
			return fmt.Errorf("replay history run %d: %w", i, err)
		}
	}

	if err := applyPlans(ctx, conn, engine.Options{}, plans); err != nil {
		return fmt.Errorf("%w: %w", ErrValidationFailed, err)
	}

	return nil
}

func plansFromFiles(files map[string]string) ([]engine.Plan, error) {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	migs, err := engine.LoadFS(fsys)
	if err != nil {
		return nil, err
	}

	return buildPlans(migs)
}
