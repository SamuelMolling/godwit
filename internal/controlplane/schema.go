// Package controlplane holds the state store, the lease-based scheduler and drift monitoring.
package controlplane

import (
	"context"
	"slices"
	"testing/fstest"

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
	{
		Version:  20260901000003,
		Name:     "revert",
		Checksum: "cp-revert-v1",
		UpSQL: `
ALTER TABLE cp_runs
	ADD COLUMN reverts uuid REFERENCES cp_runs (id),
	DROP CONSTRAINT cp_runs_state_check,
	ADD CONSTRAINT cp_runs_state_check CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'needs_attention', 'awaiting_contract', 'reverted'));`,
		DownSQL: `
ALTER TABLE cp_runs
	DROP CONSTRAINT cp_runs_state_check,
	ADD CONSTRAINT cp_runs_state_check CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'needs_attention', 'awaiting_contract')),
	DROP COLUMN reverts;`,
	},
	{
		Version:  20260901000004,
		Name:     "notifications",
		Checksum: "cp-notifications-v1",
		UpSQL: `
CREATE TABLE cp_notifications (
	kind    text NOT NULL,
	key     text NOT NULL,
	channel text NOT NULL,
	ts      text NOT NULL,
	PRIMARY KEY (kind, key)
);`,
		DownSQL: `DROP TABLE cp_notifications;`,
	},
	{
		Version:  20260901000005,
		Name:     "timeouts",
		Checksum: "cp-timeouts-v1",
		UpSQL:    `ALTER TABLE cp_runs ADD COLUMN lock_timeout text, ADD COLUMN statement_timeout text;`,
		DownSQL:  `ALTER TABLE cp_runs DROP COLUMN lock_timeout, DROP COLUMN statement_timeout;`,
	},
	{
		Version:  20260901000006,
		Name:     "run_kind",
		Checksum: "cp-run-kind-v1",
		UpSQL:    `ALTER TABLE cp_runs ADD COLUMN kind text NOT NULL DEFAULT 'migrate' CHECK (kind IN ('migrate', 'baseline'));`,
		DownSQL:  `ALTER TABLE cp_runs DROP COLUMN kind;`,
	},
	{
		Version:  20260901000007,
		Name:     "audit",
		Checksum: "cp-audit-v1",
		UpSQL: `
ALTER TABLE cp_runs
	ADD COLUMN created_by text NOT NULL DEFAULT 'anonymous',
	ADD COLUMN source     text NOT NULL DEFAULT '';

CREATE TABLE cp_audit (
	id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	at      timestamptz NOT NULL DEFAULT now(),
	actor   text NOT NULL,
	action  text NOT NULL,
	run_id  uuid,
	target  text NOT NULL,
	detail  text NOT NULL DEFAULT ''
);

CREATE INDEX cp_audit_target_at_idx ON cp_audit (target, at DESC);
CREATE INDEX cp_audit_run_id_idx ON cp_audit (run_id) WHERE run_id IS NOT NULL;`,
		DownSQL: `
DROP TABLE cp_audit;
ALTER TABLE cp_runs DROP COLUMN source, DROP COLUMN created_by;`,
	},
	{
		Version:  20260901000008,
		Name:     "drift_event_dedup",
		Checksum: "cp-drift-event-dedup-v1",
		UpSQL: `
UPDATE cp_drift_events e SET resolved_at = now()
WHERE e.resolved_at IS NULL AND EXISTS (
	SELECT 1 FROM cp_drift_events o
	WHERE o.target = e.target AND o.diff = e.diff AND o.resolved_at IS NULL AND o.id < e.id);

CREATE UNIQUE INDEX cp_drift_events_open_idx ON cp_drift_events (target, md5(diff)) WHERE resolved_at IS NULL;`,
		DownSQL: `DROP INDEX cp_drift_events_open_idx;`,
	},
}

// PlansFromFiles loads migration files and plans one direction; down plans come newest first.
func PlansFromFiles(files map[string]string, dir engine.Direction) ([]engine.Plan, error) {
	migs, err := MigrationsFromFiles(files)
	if err != nil {
		return nil, err
	}

	return buildPlans(migs, dir)
}

// MigrationsFromFiles loads migrations from in-memory files named like on disk.
func MigrationsFromFiles(files map[string]string) ([]engine.Migration, error) {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}

	return engine.LoadFS(fsys)
}

func buildPlans(migs []engine.Migration, dir engine.Direction) ([]engine.Plan, error) {
	plans := make([]engine.Plan, 0, len(migs))
	for _, m := range migs {
		p, err := engine.BuildPlan(m, dir)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	if dir == engine.DirectionDown {
		slices.Reverse(plans)
	}

	return plans, nil
}

func applyPlans(ctx context.Context, db engine.DB, opts engine.Options, plans []engine.Plan, extra ...engine.Option) (int, error) {
	exec := engine.New(db, opts, extra...)
	applied := 0
	for _, p := range plans {
		run := exec.Up
		if p.Direction == engine.DirectionDown {
			run = exec.Down
		}
		res, err := run(ctx, p)
		if err != nil {
			return applied, err
		}
		if !res.Skipped {
			applied++
		}
	}

	return applied, nil
}
