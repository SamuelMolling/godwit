// Package controlplane holds the state store, the lease-based scheduler and drift monitoring.
package controlplane

import (
	"context"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// storeMigrations is the control plane's own schema.
var storeMigrations = []engine.Migration{
	{
		Version:  20260901000000,
		Name:     "init",
		Checksum: "cp-init-v1",
		UpSQL: `
CREATE TABLE cp_targets (
	name       text PRIMARY KEY,
	provider   text NOT NULL,
	config     jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE cp_runs (
	id          uuid PRIMARY KEY,
	target      text NOT NULL REFERENCES cp_targets (name),
	state       text NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'needs_attention')),
	error       text,
	attempts    int NOT NULL DEFAULT 0,
	created_at  timestamptz NOT NULL DEFAULT now(),
	updated_at  timestamptz NOT NULL DEFAULT now(),
	finished_at timestamptz
);

CREATE TABLE cp_run_files (
	run_id uuid NOT NULL REFERENCES cp_runs (id),
	name   text NOT NULL,
	body   text NOT NULL,
	PRIMARY KEY (run_id, name)
);

CREATE TABLE cp_leases (
	run_id     uuid PRIMARY KEY REFERENCES cp_runs (id),
	holder     text NOT NULL,
	expires_at timestamptz NOT NULL
);`,
		DownSQL: `
DROP TABLE cp_leases;
DROP TABLE cp_run_files;
DROP TABLE cp_runs;
DROP TABLE cp_targets;`,
	},
	{
		Version:  20260901000001,
		Name:     "drift",
		Checksum: "cp-drift-v1",
		UpSQL: `
CREATE TABLE cp_snapshots (
	target      text PRIMARY KEY REFERENCES cp_targets (name),
	fingerprint text NOT NULL,
	definition  text NOT NULL,
	run_id      uuid,
	taken_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE cp_drift_events (
	id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	target      text NOT NULL REFERENCES cp_targets (name),
	diff        text NOT NULL,
	detected_at timestamptz NOT NULL DEFAULT now(),
	resolved_at timestamptz
);`,
		DownSQL: `
DROP TABLE cp_drift_events;
DROP TABLE cp_snapshots;`,
	},
	{
		Version:  20260901000002,
		Name:     "rollout",
		Checksum: "cp-rollout-v1",
		UpSQL: `
ALTER TABLE cp_runs
	ADD COLUMN rollout text NOT NULL DEFAULT 'direct',
	ADD COLUMN phase   text NOT NULL DEFAULT 'expand' CHECK (phase IN ('expand', 'contract')),
	DROP CONSTRAINT cp_runs_state_check,
	ADD CONSTRAINT cp_runs_state_check CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'needs_attention', 'awaiting_contract'));`,
		DownSQL: `
ALTER TABLE cp_runs
	DROP CONSTRAINT cp_runs_state_check,
	ADD CONSTRAINT cp_runs_state_check CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'needs_attention')),
	DROP COLUMN phase,
	DROP COLUMN rollout;`,
	},
}

func buildPlans(migs []engine.Migration) ([]engine.Plan, error) {
	plans := make([]engine.Plan, 0, len(migs))
	for _, m := range migs {
		p, err := engine.BuildPlan(m, engine.DirectionUp)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}

	return plans, nil
}

func applyPlans(ctx context.Context, db engine.DB, opts engine.Options, plans []engine.Plan) error {
	exec := engine.New(db, opts)
	for _, p := range plans {
		if _, err := exec.Up(ctx, p); err != nil {
			return err
		}
	}

	return nil
}
