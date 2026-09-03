package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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

// Engine applies plans to a target database, marks migrations applied and inspects its schema and journal.
type Engine interface {
	Apply(ctx context.Context, req ApplyRequest) error
	MarkApplied(ctx context.Context, dsn string, migs []engine.Migration) error
	Snapshot(ctx context.Context, dsn string) (definition, fingerprint string, err error)
	Applied(ctx context.Context, dsn string) ([]engine.Applied, []engine.Repeatable, error)
	Observe(ctx context.Context, dsn string) (Observation, error)
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
		switch {
		case ev.Statement.Batch != nil:
			kind = "batch"
		case ev.Statement.NoTx:
			kind = "no_tx"
		}
		if e.Metrics != nil {
			e.Metrics.Statement(target, kind, ev.Duration, ev.Err)
		}
		attrs := []any{
			"run", runID, "target", target, "migration", ev.Migration, "stmt", ev.Index,
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

// Applied implements Engine.
func (PGEngine) Applied(ctx context.Context, dsn string) ([]engine.Applied, []engine.Repeatable, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("connect target: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	return listApplied(ctx, conn)
}

func listApplied(ctx context.Context, db engine.DB) ([]engine.Applied, []engine.Repeatable, error) {
	applied, err := engine.ListApplied(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	reps, err := engine.ListRepeatables(ctx, db)

	return applied, reps, err
}

// Observe implements Engine: history and schema read over one connection.
func (PGEngine) Observe(ctx context.Context, dsn string) (Observation, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return Observation{}, fmt.Errorf("connect target: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	return observe(ctx, conn)
}

func observe(ctx context.Context, db engine.DB) (Observation, error) {
	obs := Observation{At: time.Now().UTC()}
	var err error
	if obs.Applied, err = engine.ListApplied(ctx, db); err != nil {
		return Observation{}, err
	}
	if obs.Repeatables, err = engine.ListRepeatables(ctx, db); err != nil {
		return Observation{}, err
	}
	if obs.Definition, obs.Fingerprint, err = engine.Snapshot(ctx, db); err != nil {
		return Observation{}, err
	}
	if err := db.QueryRow(ctx, `SELECT array_to_string(current_schemas(false), ',')`).Scan(&obs.SearchPath); err != nil {
		return Observation{}, fmt.Errorf("read search path: %w", err)
	}

	return obs, nil
}
