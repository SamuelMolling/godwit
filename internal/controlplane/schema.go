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
	{
		Version:  20260901000009,
		Name:     "plans",
		Checksum: "cp-plans-v1",
		UpSQL: `
CREATE TABLE cp_plans (
	id                 uuid PRIMARY KEY,
	target             text NOT NULL REFERENCES cp_targets (name),
	key                text NOT NULL,
	rollout            text NOT NULL,
	state              text NOT NULL CHECK (state IN ('ready', 'bound', 'superseded')),
	history_hash       text NOT NULL,
	applied            jsonb NOT NULL,
	schema_fingerprint text NOT NULL,
	schema_definition  text NOT NULL,
	drift              text NOT NULL DEFAULT '',
	plan               jsonb NOT NULL,
	validated          boolean NOT NULL,
	acked              text[] NOT NULL DEFAULT '{}',
	allow_out_of_order boolean NOT NULL DEFAULT false,
	created_by         text NOT NULL,
	source             text NOT NULL DEFAULT '',
	created_at         timestamptz NOT NULL DEFAULT now(),
	run_id             uuid REFERENCES cp_runs (id),
	superseded_by      uuid REFERENCES cp_plans (id) ON UPDATE CASCADE
);
CREATE UNIQUE INDEX cp_plans_ready_idx ON cp_plans (target, key) WHERE state = 'ready';
CREATE INDEX cp_plans_target_created_idx ON cp_plans (target, created_at DESC);

CREATE TABLE cp_plan_files (
	plan_id uuid NOT NULL REFERENCES cp_plans (id) ON UPDATE CASCADE,
	name    text NOT NULL,
	body    text NOT NULL,
	PRIMARY KEY (plan_id, name)
);

ALTER TABLE cp_runs ADD COLUMN plan_id uuid REFERENCES cp_plans (id);`,
		DownSQL: `
ALTER TABLE cp_runs DROP COLUMN plan_id;
DROP TABLE cp_plan_files;
DROP TABLE cp_plans;`,
	},
	{
		Version:  20260901000010,
		Name:     "retry",
		Checksum: "cp-retry-v1",
		UpSQL: `
ALTER TABLE cp_runs ADD COLUMN not_before timestamptz, ADD COLUMN retries int NOT NULL DEFAULT 0;
ALTER TABLE cp_plans ADD COLUMN files_hash text NOT NULL DEFAULT '';
UPDATE cp_plans p SET files_hash = h.hash FROM (
	SELECT plan_id, encode(sha256(convert_to(string_agg(name || E'\n' || body || E'\n', '' ORDER BY name COLLATE "C"), 'UTF8')), 'hex') AS hash
	FROM cp_plan_files GROUP BY plan_id) h
WHERE h.plan_id = p.id;
CREATE INDEX cp_plans_files_hash_idx ON cp_plans (target, rollout, files_hash) WHERE state = 'bound';`,
		DownSQL: `
DROP INDEX cp_plans_files_hash_idx;
ALTER TABLE cp_plans DROP COLUMN files_hash;
ALTER TABLE cp_runs DROP COLUMN not_before, DROP COLUMN retries;`,
	},
	{
		Version:  20260901000011,
		Name:     "plan_retention",
		Checksum: "cp-plan-retention-v1",
		UpSQL: `
ALTER TABLE cp_runs DROP CONSTRAINT cp_runs_plan_id_fkey,
	ADD CONSTRAINT cp_runs_plan_id_fkey FOREIGN KEY (plan_id) REFERENCES cp_plans (id) ON DELETE SET NULL;
ALTER TABLE cp_plans DROP CONSTRAINT cp_plans_superseded_by_fkey,
	ADD CONSTRAINT cp_plans_superseded_by_fkey FOREIGN KEY (superseded_by) REFERENCES cp_plans (id) ON UPDATE CASCADE ON DELETE SET NULL;
ALTER TABLE cp_plan_files DROP CONSTRAINT cp_plan_files_plan_id_fkey,
	ADD CONSTRAINT cp_plan_files_plan_id_fkey FOREIGN KEY (plan_id) REFERENCES cp_plans (id) ON UPDATE CASCADE ON DELETE CASCADE;`,
		DownSQL: `
ALTER TABLE cp_plan_files DROP CONSTRAINT cp_plan_files_plan_id_fkey,
	ADD CONSTRAINT cp_plan_files_plan_id_fkey FOREIGN KEY (plan_id) REFERENCES cp_plans (id) ON UPDATE CASCADE;
ALTER TABLE cp_plans DROP CONSTRAINT cp_plans_superseded_by_fkey,
	ADD CONSTRAINT cp_plans_superseded_by_fkey FOREIGN KEY (superseded_by) REFERENCES cp_plans (id) ON UPDATE CASCADE;
ALTER TABLE cp_runs DROP CONSTRAINT cp_runs_plan_id_fkey,
	ADD CONSTRAINT cp_runs_plan_id_fkey FOREIGN KEY (plan_id) REFERENCES cp_plans (id);`,
	},
	{
		Version:  20260901000012,
		Name:     "plan_search_path",
		Checksum: "cp-plan-search-path-v1",
		UpSQL:    `ALTER TABLE cp_plans ADD COLUMN search_path text NOT NULL DEFAULT '';`,
		DownSQL:  `ALTER TABLE cp_plans DROP COLUMN search_path;`,
	},
	{
		Version:  20260902000013,
		Name:     "plan_repeatables",
		Checksum: "cp-plan-repeatables-v1",
		UpSQL:    `ALTER TABLE cp_plans ADD COLUMN repeatables jsonb NOT NULL DEFAULT '[]'::jsonb;`,
		DownSQL:  `ALTER TABLE cp_plans DROP COLUMN repeatables;`,
	},
	{
		Version:  20260902000014,
		Name:     "plan_expansions",
		Checksum: "cp-plan-expansions-v1",
		UpSQL: `
ALTER TABLE cp_plans ADD COLUMN expansions jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE cp_runs ADD COLUMN expansions jsonb NOT NULL DEFAULT '{}'::jsonb, ADD COLUMN progress jsonb;
CREATE TABLE cp_retired_columns (
	target     text NOT NULL REFERENCES cp_targets (name),
	schema     text NOT NULL,
	rel        text NOT NULL,
	col        text NOT NULL,
	retires    text NOT NULL,
	migration  text NOT NULL,
	run_id     uuid REFERENCES cp_runs (id) ON DELETE SET NULL,
	retired_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (target, schema, rel, col)
);`,
		DownSQL: `
DROP TABLE cp_retired_columns;
ALTER TABLE cp_runs DROP COLUMN expansions, DROP COLUMN progress;
ALTER TABLE cp_plans DROP COLUMN expansions;`,
	},
	{
		Version:  20260903000015,
		Name:     "run_ledger",
		Checksum: "cp-run-ledger-v1",
		UpSQL: `
CREATE TABLE cp_run_applied (
	run_id      uuid NOT NULL REFERENCES cp_runs (id),
	migration   text NOT NULL,
	seq         int NOT NULL,
	held        boolean NOT NULL DEFAULT false,
	expansion   jsonb,
	applied_at  timestamptz NOT NULL DEFAULT now(),
	reverted_by uuid REFERENCES cp_runs (id),
	PRIMARY KEY (run_id, migration)
);

INSERT INTO cp_run_applied (run_id, migration, seq, applied_at)
SELECT run_id, migration, row_number() OVER (PARTITION BY run_id ORDER BY migration), applied_at
FROM (
	SELECT DISTINCT ON (f.name, f.body) r.id AS run_id, left(f.name, length(f.name) - 7) AS migration, r.created_at AS applied_at
	FROM cp_run_files f JOIN cp_runs r ON r.id = f.run_id
	WHERE f.name LIKE '%.up.sql' AND r.state IN ('succeeded', 'awaiting_contract')
	ORDER BY f.name, f.body, r.created_at
) first_run;

UPDATE cp_run_applied a SET expansion = r.expansions -> a.migration
FROM cp_runs r WHERE r.id = a.run_id AND r.expansions ? a.migration;`,
		DownSQL: `DROP TABLE cp_run_applied;`,
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

// pairOf names one migration's files; a checkpoint has no inverse, so it contributes no down file.
func pairOf(id, up, down string) map[string]string {
	out := map[string]string{id + ".up.sql": up}
	if down != "" {
		out[id+".down.sql"] = down
	}

	return out
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

// Recorder sees the outcome of every plan as it completes, so the ledger survives a failure mid-run.
type Recorder func(ctx context.Context, res engine.Result) error

func applyPlans(ctx context.Context, db engine.DB, opts engine.Options, plans []engine.Plan, rec Recorder, extra ...engine.Option) (int, error) {
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
		if rec != nil {
			if err := rec(ctx, res); err != nil {
				return applied, err
			}
		}
		if !res.Skipped {
			applied++
		}
	}

	return applied, nil
}
