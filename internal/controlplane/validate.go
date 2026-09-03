package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// ErrValidationFailed marks a migration the author must fix.
var ErrValidationFailed = errors.New("migration failed validation")

var (
	connectScratch  = pgx.ConnectConfig
	snapshotScratch = engine.Snapshot
)

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

// Validation is what the scratch database looked like after the history and after each plan in turn.
type Validation struct {
	Base         string
	Effects      [][]string
	Fingerprints []string
}

// Validate replays the history, applies each plan on top and snapshots the schema after every step.
func (v *Validator) Validate(ctx context.Context, target string, plans []engine.Plan, searchPath string) (Validation, error) {
	history, err := v.store.HistoryFiles(ctx, target)
	if err != nil {
		return Validation{}, err
	}

	name := "godwit_validate_" + v.newID()
	if _, err := v.pool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return Validation{}, fmt.Errorf("create scratch database: %w", err)
	}
	defer func() {
		_, _ = v.pool.Exec(context.WithoutCancel(ctx),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}()

	cfg := v.pool.Config().ConnConfig.Copy()
	cfg.Database = name
	conn, err := connectScratch(ctx, cfg)
	if err != nil {
		return Validation{}, fmt.Errorf("connect scratch database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if err := mirrorSearchPath(ctx, conn, searchPath); err != nil {
		return Validation{}, err
	}

	for i, files := range history {
		histPlans, err := PlansFromFiles(files, engine.DirectionUp)
		if err != nil {
			return Validation{}, fmt.Errorf("history run %d: %w", i, err)
		}
		if _, err := applyPlans(ctx, conn, engine.Options{}, histPlans); err != nil {
			return Validation{}, fmt.Errorf("replay history run %d: %w", i, err)
		}
	}

	return validateEach(ctx, conn, plans)
}

// The scratch role is not the target's; without this, unqualified names land in a different schema than on the target.
func mirrorSearchPath(ctx context.Context, conn engine.DB, searchPath string) error {
	if searchPath == "" {
		return nil
	}
	schemas := strings.Split(searchPath, ",")
	stmts := make([]string, 0, len(schemas)+1)
	for i, schema := range schemas {
		schemas[i] = pgx.Identifier{schema}.Sanitize()
		if !strings.HasPrefix(schema, "pg_") {
			stmts = append(stmts, "CREATE SCHEMA IF NOT EXISTS "+schemas[i])
		}
	}
	for _, stmt := range append(stmts, "SET search_path TO "+strings.Join(schemas, ", ")) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("mirror search path: %w", err)
		}
	}

	return nil
}

func validateEach(ctx context.Context, conn engine.DB, plans []engine.Plan) (Validation, error) {
	def, fp, err := snapshotScratch(ctx, conn)
	if err != nil {
		return Validation{}, fmt.Errorf("snapshot scratch database: %w", err)
	}
	val := Validation{Base: def, Fingerprints: []string{fp}}
	for _, p := range plans {
		if _, err := applyPlans(ctx, conn, engine.Options{}, []engine.Plan{p}); err != nil {
			return Validation{}, fmt.Errorf("%w: %w", ErrValidationFailed, err)
		}
		next, nextFP, err := snapshotScratch(ctx, conn)
		if err != nil {
			return Validation{}, fmt.Errorf("snapshot scratch database: %w", err)
		}
		val.Effects = append(val.Effects, engine.DiffSchemas(def, next))
		val.Fingerprints = append(val.Fingerprints, nextFP)
		def = next
	}

	return val, nil
}
