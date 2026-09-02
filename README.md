# godwit

> The bar-tailed godwit holds the world record for the longest non-stop migration — ~13,500 km without landing. Your database migrations should be just as reliable.

**godwit** is a pipeline-native database migration service. Platform teams get a golden path for schema changes — from a Backstage form to a GitOps apply — with crash-safe execution, linting, drift detection and rollback, all free of vendor paywalls. Migrations execute under a statement-level journal protocol that survives executor crashes with automatic recovery — no "dirty state", ever.

## Why

The 2025–2026 licensing landscape pushed every incumbent's safety features behind paywalls:

- **Flyway**: undo migrations and dry-runs are Teams/Enterprise-only; Teams is closed to new customers.
- **Liquibase**: automated drift detection, policy checks and targeted rollbacks require a Secure license.
- **Atlas**: `migrate lint`, `migrate down`, drift detection and all integrations moved to the paid Pro tier (v0.38); drift detection additionally requires their cloud registry.

Meanwhile there is **no Backstage plugin for database migrations at all** — every org rebuilds the same glue. godwit is that glue, done once, as open source.

## What's inside

| Feature | What it does |
|---|---|
| Crash-safe engine | Plain-SQL migrations under a statement-level journal in the target database; DDL and progress commit atomically. Non-transactional statements (`CREATE INDEX CONCURRENTLY`, …) use write-ahead intents and verifiers. Survives `kill -9` at any point. |
| Control plane | Runs are leased with heartbeats; a replica that dies mid-run is taken over by another and resumed from the journal. Failed runs park as `needs_attention` after a resume budget. |
| Hazard gate | The planner ([libpg_query](https://github.com/pganalyze/pg_query_go)) tags unsafe DDL with a code and the safe alternative (see [Hazards](#hazards)). Runs carrying unacknowledged hazards are refused. |
| PR lint | `godwit lint` parses a migration directory offline and fails on unacknowledged hazards, parse errors and migrations modified after merge; a no-op `.down.sql` is a warning. Text, markdown (for `$GITHUB_STEP_SUMMARY`) and JSON output. |
| Pre-apply validation | Every run replays the target's history plus the new files on a scratch database before it is queued. |
| Drift detection | Schema fingerprint after each run; a monitor diffs the live schema, records events, notifies (webhook) and auto-resolves. `AcceptBaseline` blesses manual changes. |
| Rollout policies | `direct` applies everything now. `expand-contract` applies additive migrations at PreSync and holds the first destructive migration (and everything after it) until `ConfirmRollout` — blue/green safe. |
| Revert | `RevertRun` queues the down side of the latest run on a target — same journal, lease and hazard gate as the way up. The original is marked `reverted` and leaves the replayable history. |
| Credentials | Pluggable providers: `static` (AES-GCM-encrypted in the store), `kubernetes` (mounted secret) and `vault` (KV or dynamic database credentials, token or Kubernetes auth). |
| API | gRPC and JSON over one connect endpoint, bearer-token auth, `WatchRun` streaming. |
| CLI | The same binary drives the service: `godwit migrate` streams a run to completion with pipeline exit codes; `target`, `run`, `runs`, `revert` and `drift` cover the rest. |
| Metrics | Prometheus on `/metrics`: runs per state with age, resumes by source, attempts, run and statement latency, lock/statement timeouts, hazards, validation refusals, drift outcomes, API calls. |

## Hazards

| Code | Statement | Why | Safe form |
|---|---|---|---|
| `H001` | `CREATE INDEX` without `CONCURRENTLY` | blocks writes while the index builds | `CREATE INDEX CONCURRENTLY` |
| `H002` | `DROP TABLE` | destructive | contract phase, after every app version stopped using it |
| `H003` | `DROP COLUMN` | destructive | contract phase, after every app version stopped using it |
| `H004` | `ALTER COLUMN ... TYPE` | rewrites the table under an exclusive lock | new column, backfill, swap |
| `H005` | `ADD COLUMN ... NOT NULL` without `DEFAULT` | fails on non-empty tables | add a `DEFAULT`, or add nullable and backfill |
| `H006` | `ADD CONSTRAINT ... FOREIGN KEY` / `CHECK` without `NOT VALID` | scans the whole table under lock | `... NOT VALID`, then `VALIDATE CONSTRAINT` in a separate statement |
| `H007` | `ALTER COLUMN ... SET NOT NULL` | scans the table under an exclusive lock | `ADD CHECK (col IS NOT NULL) NOT VALID`, `VALIDATE CONSTRAINT`, then `SET NOT NULL` (instant on PostgreSQL 12+) |
| `H008` | `RENAME` table or column | breaks application versions still using the old name | add the new one, migrate readers and writers, drop the old one |
| `H009` | `DROP INDEX` without `CONCURRENTLY` | blocks reads and writes on the table | `DROP INDEX CONCURRENTLY` |
| `H010` | `ADD PRIMARY KEY` / `ADD CONSTRAINT ... UNIQUE` without `USING INDEX` | builds the index under an exclusive lock | `CREATE UNIQUE INDEX CONCURRENTLY`, then `ADD CONSTRAINT ... USING INDEX` |

Acknowledge a hazard you have reviewed with `--ack H006,H010` (`acknowledge_hazards` on the API); the gate still counts it in `godwit_hazards_total{acked="true"}`.

## Rollout policies

With `expand-contract`, put contract statements (drops) in their own migration file. The run stops in `awaiting_contract` once the expand phase is applied; the previous app version keeps working. Your deploy pipeline (or an ArgoCD PostSync hook) calls `ConfirmRollout` after the new version is healthy, and the contract phase runs.

```
CreateRun{rollout: "expand-contract"}  →  running  →  awaiting_contract  →  ConfirmRollout  →  running  →  succeeded
```

## Reverting

`RevertRun{run_id}` creates a new run that applies the `.down.sql` side of that run's migrations, newest version first, through the same crash-safe executor. Only the latest run on an idle target can be reverted, so reverts happen in reverse order. Down migrations go through the hazard gate and scratch-database validation like any other plan.

```
RevertRun{run_id: A}  →  new run R (reverts: A)  →  succeeded  ⇒  A becomes reverted
```

## Credentials

Targets never store a plaintext DSN. `RegisterTarget` picks a provider:

- `static` — the DSN is AES-GCM-encrypted with `GODWIT_MASTER_KEY` and stored in the control plane.
- `kubernetes` — `secret_path` points at a mounted secret file on the replica.
- `vault` — `vault_path` is read from Vault at run time (`secret/data/app` for KV v2, `database/creds/app` for dynamic credentials). The DSN is the secret's `dsn` field, or `vault_template` rendered over its fields: `postgres://{{username}}:{{password}}@db/app`. The service authenticates with `VAULT_TOKEN` or, when unset, the Kubernetes auth method (`VAULT_K8S_ROLE`, `VAULT_K8S_MOUNT`, `VAULT_K8S_JWT`); `VAULT_ADDR` is required.

## Configuration

Put a `godwit.yaml` in your repo and the CLI stops needing repeated flags. The file is looked up from the working directory upwards until the repo root (`.git`); `--config path` points at a specific file. `dir` is resolved relative to the file.

```yaml
dir: db/migrations          # plan, run, status, down, lint, migrate
target: orders              # migrate
rollout: expand-contract    # migrate
server: http://godwit:8474  # every service command
lock_timeout: 5s            # run, status, down
statement_timeout: 0        # run, status, down (0 disables)
```

With that file a pipeline is `godwit lint --base origin/main` on the PR and `godwit migrate` on merge — both flagless.

Precedence: explicit flag > `GODWIT_*` env (`GODWIT_DIR`, `GODWIT_TARGET`, `GODWIT_ROLLOUT`, `GODWIT_SERVER`, `GODWIT_LOCK_TIMEOUT`, `GODWIT_STATEMENT_TIMEOUT`) > file > default. Unknown keys are an error. The DSN never lives in the file — pass `--dsn` or use a credential provider.

## CLI

One binary, two modes. Local commands talk to a database directly (dev loop, no service needed); service commands talk to `godwit serve` over its API, so a pipeline gets the journal, hazard gate, validation and rollout policies without curl.

| Local (`--dsn`) | Service (`--server`, `--token`) |
|---|---|
| `plan` — classify statements, show hazards; `lint [--base origin/main] [--format markdown]` — PR gate, exit 1 on unacked hazards or edited migrations | `target add <name> --provider static\|kubernetes\|vault` |
| `apply` — apply pending migrations | `migrate --target <t> [--dir] [--rollout] [--ack H001,H003] [--skip-validation]` |
| `status` — applied state per migration | `revert <run-id>`, `run get\|watch\|resume\|confirm <id>`, `runs [--target]` |
| `down --version <v> --yes` — revert one (dev only) | `drift check\|accept <target>` |

`--server` and `--token` fall back to `GODWIT_SERVER` and `GODWIT_TOKEN`; `--json` prints the raw API response. `migrate` and `revert` stream the run until it settles and exit 0 on `succeeded`/`awaiting_contract`, 1 on `failed`/`needs_attention`.

```bash
export GODWIT_SERVER=https://godwit.internal GODWIT_TOKEN=$CI_TOKEN
RUN_ID=$(godwit migrate --target prod --rollout expand-contract --json | tail -n1 | jq -r .run.id)
kubectl rollout status deploy/app
godwit run confirm "$RUN_ID"
```

## In your PR

```yaml
- uses: actions/checkout@v4
  with: { fetch-depth: 0 }
- run: go install github.com/SamuelMolling/godwit/cmd/godwit@latest
- run: godwit lint --base origin/main --format markdown >> "$GITHUB_STEP_SUMMARY"
```

## Metrics

`godwit serve` exposes Prometheus metrics on `/metrics` (same listener, no auth). What to alert on:

| Metric | Question it answers |
|---|---|
| `godwit_runs{target,state}`, `godwit_run_age_seconds{target,state}` | Anything parked in `needs_attention`? An `awaiting_contract` nobody confirmed? A `queued` run nobody claims? |
| `godwit_run_resumes_total{target,source}` | `reconciler` = a replica died mid-run and another took over; `manual` = an operator hit `ResumeRun`. |
| `godwit_run_attempts`, `godwit_heartbeat_failures_total` | How often runs need more than one attempt; heartbeats failing without the lease expiring means a slow store. |
| `godwit_run_duration_seconds{target,result}`, `godwit_statement_duration_seconds{target,kind}` | How long migrations hold the target, per statement (`tx` / `no_tx`). |
| `godwit_statement_failures_total{target,reason}` | `lock_timeout` and `statement_timeout` are contention on a live database; other SQLSTATEs are bugs in the migration. |
| `godwit_hazards_total{code,acked}`, `godwit_validation_failures_total{target}` | Is the gate doing work, or is everything acknowledged blindly? |
| `godwit_drift_checks_total{target,result}` | `clean`, `drifted` or `accepted` per check. |
| `godwit_api_requests_total{method,code}`, `godwit_api_request_duration_seconds{method}`, `godwit_build_info` | The service itself. |

Per-migration and per-statement series are deliberately absent — that detail lives in the run journal, not in label cardinality.

Probes live on the same listener, also unauthenticated: `GET /healthz` answers 200 while the process is up; `GET /readyz` answers 200 when the store replies to `SELECT 1` within 2s and 503 otherwise.

## Engines

| Engine | Status |
|---|---|
| PostgreSQL | v1 |
| Cassandra, MongoDB | planned — the `Engine` interface is the seam |

## Design principles

1. **The journal is the truth.** Progress lives in the target database, committed with the DDL. No "dirty" flag, no manual repair.
2. **Plain SQL, language-agnostic.** Works identically for TypeScript, Go, or any other stack. ORMs can generate the SQL; godwit runs it.
3. **Roll forward, not back.** Down migrations are required, but the production playbook is expand → contract, and godwit schedules the contract for you.
4. **Safety is not a paid tier.** Lock timeouts, hazard gating, validation, drift alerts and rollout policies ship free.

## Try it

```bash
cd demo && docker compose up -d --build && ./demo.sh
```

Two replicas, a store and a target database. The script kills the replica executing a slow migration; the other one recovers the lease and finishes the run from the journal. See [demo/README.md](demo/README.md).

## Status

🚧 v1 in progress — **PostgreSQL only, API-first, no UI yet** (Backstage plugin or standalone UI arrive in v1.1 on top of the same API).
