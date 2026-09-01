// Package controlplane implements the godwit service brain: state store,
// per-target scheduler with leases, and crash recovery by lease expiry.
package controlplane

import (
	"context"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// storeMigrations is the control plane's own schema, applied by godwit's engine.
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
}

// buildPlans compiles migrations into up plans.
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

// applyPlans runs plans in order over one session.
func applyPlans(ctx context.Context, db engine.DB, opts engine.Options, plans []engine.Plan) error {
	exec := engine.New(db, opts)
	for _, p := range plans {
		if _, err := exec.Up(ctx, p); err != nil {
			return err
		}
	}

	return nil
}
