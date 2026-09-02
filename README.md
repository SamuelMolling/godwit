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
| PR lint | `godwit lint` parses a migration directory offline and fails on unacknowledged hazards, parse errors and migrations modified after merge; a no-op `.down.sql` is a warning. Text, markdown (for `$GITHUB_STEP_SUMMARY`) and JSON output. The repository is also a GitHub Action wrapping `lint`, `plan` and `migrate` with a sticky PR comment. |
| Pre-apply validation | Every run replays the target's history plus the new files on a scratch database before it is queued. |
| Drift detection | Schema fingerprint after each run; a monitor diffs the live schema, records events, notifies and auto-resolves. `AcceptBaseline` blesses manual changes. |
| Rollout policies | `direct` applies everything now. `expand-contract` applies additive migrations at PreSync and holds the first destructive migration (and everything after it) until `ConfirmRollout` — blue/green safe. |
| Revert | `RevertRun` queues the down side of the latest run on a target — same journal, lease and hazard gate as the way up. The original is marked `reverted` and leaves the replayable history. |
| Baseline | `BaselineTarget` adopts an existing database: every migration up to a version is marked applied without running it, recorded as a `baseline` run whose files seed the replayable history, and snapshotted for drift — see [Baselining an existing database](#baselining-an-existing-database). |
| Target status | `GetTargetStatus` answers "where is this database?" in one call: applied versions read from the target's `godwit.migrations` (checksum mismatches flagged against the given files), pending versions, the last run, the drift baseline and whether drift is open, the credential provider and the configured timeouts — see [Target status](#target-status). |
| Out-of-order guard | A pending version older than the newest one already applied on the target is refused at admission; `allow_out_of_order` lets it through and is logged — see [Out-of-order migrations](#out-of-order-migrations). |
| Dry run | `PlanRun` / `migrate --dry-run` puts the files through the same admission as a real run (hazard gate, order guard, scratch validation) against a live target and returns what it would do — applied or pending per migration, the expand/contract split, statements with hazards — without queueing anything. The Action posts it as a PR comment — see [Dry run](#dry-run). |
| Credentials | Pluggable providers: `static` (AES-GCM-encrypted in the store), `kubernetes` (mounted secret) and `vault` (KV or dynamic database credentials, token or Kubernetes auth). |
| API | gRPC and JSON over one connect endpoint, bearer-token auth, `WatchRun` streaming. |
| CLI | The same binary drives the service: `godwit migrate` streams a run to completion with pipeline exit codes; `target add`, `target baseline`, `target status`, `run`, `runs`, `revert` and `drift` cover the rest. |
| Metrics | Prometheus on `/metrics`: runs per state with age, resumes by source, attempts, run and statement latency, lock/statement timeouts, hazards, validation refusals, drift outcomes, API calls. |
| Logging | Structured `slog` output (JSON or text, level control) with one key set across the service: every API call, run lifecycle, per-statement timing, drift checks. Never a DSN, token, secret or SQL body. |
| Notifications | Every run transition and drift event goes to Slack (one message per run, threaded or edited in place) and/or a JSON webhook, delivered off the run's critical path — see [Notifications](#notifications). |
| Deploy | Helm chart (two replicas, probes, PDB, ServiceMonitor, Ingress) and ArgoCD PreSync/PostSync hook examples — see [Deploy](#deploy). |

## Hazards

| Code | Statement | Why | Safe form | Phase |
|---|---|---|---|---|
| `H001` | `CREATE INDEX` without `CONCURRENTLY` | blocks writes while the index builds | `CREATE INDEX CONCURRENTLY` | `expand` |
| `H002` | `DROP TABLE` | destructive | contract phase, after every app version stopped using it | `contract` |
| `H003` | `DROP COLUMN` | destructive | contract phase, after every app version stopped using it | `contract` |
| `H004` | `ALTER COLUMN ... TYPE` | rewrites the table under an exclusive lock | new column, backfill, swap | `expand` |
| `H005` | `ADD COLUMN ... NOT NULL` without `DEFAULT` | fails on non-empty tables | add a `DEFAULT`, or add nullable and backfill | `expand` |
| `H006` | `ADD CONSTRAINT ... FOREIGN KEY` / `CHECK` without `NOT VALID` | scans the whole table under lock | `... NOT VALID`, then `VALIDATE CONSTRAINT` in a separate statement | `expand` |
| `H007` | `ALTER COLUMN ... SET NOT NULL` | scans the table under an exclusive lock | `ADD CHECK (col IS NOT NULL) NOT VALID`, `VALIDATE CONSTRAINT`, then `SET NOT NULL` (instant on PostgreSQL 12+) | `expand` |
| `H008` | `RENAME` table or column | breaks application versions still using the old name | add the new one, migrate readers and writers, drop the old one | `contract` |
| `H009` | `DROP INDEX` without `CONCURRENTLY` | blocks reads and writes on the table | `DROP INDEX CONCURRENTLY` | `expand` |
| `H010` | `ADD PRIMARY KEY` / `ADD CONSTRAINT ... UNIQUE` without `USING INDEX` | builds the index under an exclusive lock | `CREATE UNIQUE INDEX CONCURRENTLY`, then `ADD CONSTRAINT ... USING INDEX` | `expand` |

Acknowledge a hazard you have reviewed with `--ack H006,H010` (`acknowledge_hazards` on the API); the gate still counts it in `godwit_hazards_total{acked="true"}`.

The phase column is what `expand-contract` acts on: `contract` codes (`H002`, `H003`, `H008`) remove or rename something the previous app version still addresses by name, so the first migration carrying one — and everything after it — waits in `awaiting_contract`. `expand` codes are lock, rewrite and constraint hazards; the old version keeps working once the statement finishes, so they run in the expand phase.

## Rollout policies

With `expand-contract`, put contract statements (drops and renames) in their own migration file. The run stops in `awaiting_contract` once the expand phase is applied; the previous app version keeps working. Your deploy pipeline (or an ArgoCD PostSync hook) calls `ConfirmRollout` after the new version is healthy, and the contract phase runs.

```
CreateRun{rollout: "expand-contract"}  →  running  →  awaiting_contract  →  ConfirmRollout  →  running  →  succeeded
```

## Reverting

`RevertRun{run_id}` creates a new run that applies the `.down.sql` side of that run's migrations, newest version first, through the same crash-safe executor. Only the latest run on an idle target can be reverted, so reverts happen in reverse order. Down migrations go through the hazard gate and scratch-database validation like any other plan. A revert is all-or-nothing per run: every migration in the run comes down together, and there is no way to revert a single version out of a multi-migration run.

```
RevertRun{run_id: A}  →  new run R (reverts: A)  →  succeeded  ⇒  A becomes reverted
```

## Baselining an existing database

A database that predates godwit already has its schema; running the migrations that built it would fail on the first `CREATE TABLE`. `BaselineTarget{target, files, version}` loads the files, marks every migration with a version at or below `version` as applied in `godwit.migrations` (with its checksum) without executing anything, and records a run of kind `baseline` in state `succeeded` holding those files. From then on the target behaves like one godwit built: later migrations apply normally, scratch-database validation replays the baseline files before the new ones, and a schema snapshot taken right after the call feeds drift detection.

The usual shape is a schema dump as the first migration plus the real migrations after it:

```
migrations/
  00000000000001_baseline.up.sql      # pg_dump --schema-only of the existing database
  00000000000001_baseline.down.sql    # DROP of everything above
  20260901120000_add_email.up.sql
  20260901120000_add_email.down.sql
```

```
godwit target baseline app --dir migrations --version 1
godwit migrate --target app --dir migrations      # applies 20260901120000 only
```

The call is refused with `FailedPrecondition` when the target already has applied versions — a baseline is a one-time adoption, not a way to skip migrations. Baseline runs cannot be reverted; `runs` and `run get` show the kind of every run.

## Out-of-order migrations

Two branches merge in the wrong order and the target already runs `20260901130000` when `20260901120000` shows up. Applying it silently would leave the history in a state that no fresh database can reproduce by replaying versions in order, so `CreateRun` refuses it with `FailedPrecondition`, naming the pending versions that fall behind and the newest applied one:

```
out-of-order migrations 20260901120000: newest applied version on app is 20260901130000 (pass allow_out_of_order to apply them anyway)
```

The newest applied version comes from the target's history in the control plane (the files of every `succeeded` run, baselines included), the same history scratch-database validation replays. Versions the target already has are never out of order, so resending the whole directory stays flagless; only a pending version below the newest applied one trips the guard.

When the older migration is genuinely independent, `CreateRun{allow_out_of_order}` / `migrate --allow-out-of-order` admits it; the service logs `out-of-order migrations admitted` with the versions, and the run is otherwise ordinary. A project that lives with this permanently can set `allow_out_of_order: true` in `godwit.yaml`. This is Flyway's `outOfOrder` (off by default there too) and Atlas's default `--exec-order linear`; Liquibase has no notion of version order.

## Dry run

Offline `plan` says what the files contain; it cannot say what a target would do with them. `PlanRun{target, files, acknowledge_hazards, rollout, allow_out_of_order, skip_validation}` answers that: it runs exactly the admission `CreateRun` runs — hazard gate, out-of-order guard against the target's history, replay on a scratch database — and returns the plan instead of a run id. Nothing is queued and nothing is written to `cp_runs`.

Per migration: version, name, checksum, `applied` (already in the target's history, so the executor would skip it), the `phase` the rollout policy would put it in (`expand` or `contract`), and each statement with its `no_tx` flag and hazards. The response also carries `validated`: `false` when `skip_validation` was passed or the service runs without a scratch database. A run that would be refused is refused here too, with the same codes: `FailedPrecondition` for unacknowledged hazards or out-of-order versions, `InvalidArgument` when the scratch replay fails, `NotFound` for an unknown target.

```
$ godwit migrate --target prod --dry-run --rollout expand-contract --ack H003
dry run on prod (rollout expand-contract, validated on a scratch database)
20260901120000_users (up): 1 statement(s) [expand, applied]
  [0] tx    CREATE TABLE users (id bigint PRIMARY KEY, email text)
20260901120001_name (up): 1 statement(s) [expand, pending]
  [0] tx    ALTER TABLE users ADD COLUMN name text
20260901120002_drop_id (up): 1 statement(s) [contract, pending]
  [0] tx    ALTER TABLE users DROP COLUMN id
        hazard H003: DROP COLUMN is destructive
```

`--format markdown` adds `Phase` and `Status` columns to the `plan` table under a `## godwit dry run` heading; `--format json` wraps the `plan` shape in `{target, rollout, validated, migrations: [{..., applied, phase}]}`; `--json` prints the raw response. A refusal exits 1 with the reason on stderr, as `migrate` would.

## Credentials

Targets never store a plaintext DSN. `RegisterTarget` picks a provider:

- `static` — the DSN is AES-GCM-encrypted with `GODWIT_MASTER_KEY` and stored in the control plane.
- `kubernetes` — `secret_path` points at a mounted secret file on the replica.
- `vault` — `vault_path` is read from Vault at run time (`secret/data/app` for KV v2, `database/creds/app` for dynamic credentials). The DSN is the secret's `dsn` field, or `vault_template` rendered over its fields: `postgres://{{username}}:{{password}}@db/app`. The service authenticates with `VAULT_TOKEN` or, when unset, the Kubernetes auth method (`VAULT_K8S_ROLE`, `VAULT_K8S_MOUNT`, `VAULT_K8S_JWT`); `VAULT_ADDR` is required.

## Timeouts

Every statement runs under PostgreSQL's `lock_timeout` and `statement_timeout` (`SET LOCAL` inside the transaction, `SET`/`RESET` around no-tx statements), so a migration waiting on a lock fails fast instead of queueing the application behind it. Defaults are `lock_timeout: 5s` and `statement_timeout: 0` (disabled). Both are Go durations (`500ms`, `5s`, `2m`); the lock timeout must be at least `1ms`, the statement timeout may be `0`.

| Scope | Where | Wins over |
|---|---|---|
| Target | `RegisterTarget{lock_timeout, statement_timeout}` / `target add --lock-timeout --statement-timeout`, stored in the target config | defaults |
| Run | `CreateRun`/`RevertRun{lock_timeout, statement_timeout}` / `migrate` and `revert --lock-timeout --statement-timeout`, stored on the run and shown by `run get` | target |

Unset fields inherit from the next scope, so `migrate --statement-timeout 10m` keeps the target's lock timeout. A timeout that fires is a `statement failed` log line and a `godwit_statement_failures_total{reason="lock_timeout"|"statement_timeout"}` sample; the run ends `failed` and can be resumed once the contention is gone.

## Configuration

Put a `godwit.yaml` in your repo and the CLI stops needing repeated flags. The file is looked up from the working directory upwards until the repo root (`.git`); `--config path` points at a specific file. `dir` is resolved relative to the file.

```yaml
dir: db/migrations          # plan, run, status, down, lint, migrate
target: orders              # migrate
rollout: expand-contract    # migrate
allow_out_of_order: false   # migrate
server: http://godwit:8474  # every service command
lock_timeout: 5s            # apply, status, down (local commands only; the service uses the target's)
statement_timeout: 0        # apply, status, down (0 disables)
```

With that file a pipeline is `godwit lint --base origin/main` on the PR and `godwit migrate` on merge — both flagless.

Precedence: explicit flag > `GODWIT_*` env (`GODWIT_DIR`, `GODWIT_TARGET`, `GODWIT_ROLLOUT`, `GODWIT_ALLOW_OUT_OF_ORDER`, `GODWIT_SERVER`, `GODWIT_LOCK_TIMEOUT`, `GODWIT_STATEMENT_TIMEOUT`) > file > default. Unknown keys are an error. The DSN never lives in the file — pass `--dsn` or use a credential provider.

## CLI

One binary, two modes. Local commands talk to a database directly (dev loop, no service needed); service commands talk to `godwit serve` over its API, so a pipeline gets the journal, hazard gate, validation and rollout policies without curl.

| Local (`--dsn`) | Service (`--server`, `--token`) |
|---|---|
| `plan [--format markdown]` — classify statements, show hazards; `lint [--base origin/main] [--format markdown]` — PR gate, exit 1 on unacked hazards or edited migrations | `target add <name> --provider static\|kubernetes\|vault [--lock-timeout] [--statement-timeout]`, `target baseline <name> --version <v> [--dir]`, `target status <name> [--dir]` |
| `apply` — apply pending migrations | `migrate --target <t> [--dir] [--rollout] [--ack H001,H003] [--skip-validation] [--allow-out-of-order] [--lock-timeout] [--statement-timeout]`, `migrate --dry-run [--format text\|markdown\|json]` — admission and plan only, no run |
| `status` — applied state per migration | `revert <run-id> [--lock-timeout] [--statement-timeout]`, `run get\|watch\|resume\|confirm <id>`, `run confirm --latest --target <t> [--allow-none]`, `runs [--target]` |
| `down --version <v> --yes` — revert one (dev only) | `drift check\|accept <target>` |

`--server` and `--token` fall back to `GODWIT_SERVER` and `GODWIT_TOKEN`; `--json` prints the raw API response. `migrate` and `revert` stream the run until it settles and exit 0 on `succeeded`/`awaiting_contract`, 1 on `failed`/`needs_attention`. `run confirm --latest` confirms the newest run on the target still in `awaiting_contract` when no run id is at hand (a PostSync hook, a shell); it fails when there is none unless `--allow-none` makes that a no-op.

```bash
export GODWIT_SERVER=https://godwit.internal GODWIT_TOKEN=$CI_TOKEN
RUN_ID=$(godwit migrate --target prod --rollout expand-contract --json | tail -n1 | jq -r .run.id)
kubectl rollout status deploy/app
godwit run confirm "$RUN_ID"
```

## In your PR

The repository doubles as a composite GitHub Action (`action.yml`): it builds `godwit` from the pinned ref and runs one command. `lint` and `plan` write their markdown report to the step summary and keep one sticky comment on the pull request up to date (marker `<!-- godwit:<command> -->`); `migrate` streams the run and exits with its pipeline code. `migrate` with `dry-run: true` asks the service for the [live plan](#dry-run) instead — applied state, phases, hazards, validation — and posts it as its own sticky comment (marker `<!-- godwit:dry-run -->`), failing the step when the run would be refused.

```yaml
# pull request gate
permissions: { contents: read, pull-requests: write }
steps:
  - uses: actions/checkout@v4
  - uses: SamuelMolling/godwit@main
    with: { command: lint, ack: H001 }          # base: origin/main is fetched when the checkout is shallow
  - uses: SamuelMolling/godwit@main
    with: { command: plan }
  - uses: SamuelMolling/godwit@main                   # optional: what prod would do with these files
    with:
      command: migrate
      dry-run: "true"
      server: https://godwit.internal
      token: ${{ secrets.GODWIT_TOKEN }}
      target: prod
      rollout: expand-contract

# on merge
steps:
  - uses: actions/checkout@v4
  - uses: SamuelMolling/godwit@main
    id: migrate
    with:
      command: migrate
      server: https://godwit.internal
      token: ${{ secrets.GODWIT_TOKEN }}
      target: prod
      rollout: expand-contract
  - run: kubectl rollout status deploy/app
  - run: godwit run confirm "${{ steps.migrate.outputs.run-id }}"
```

Inputs: `command` (`lint`|`plan`|`migrate`), `dir`, `base`, `ack`, `server`, `token`, `target`, `rollout`, `dry-run` (`false`), `comment` (`true`), `github-token`, `go-version` (`1.26`). Anything left empty falls back to `godwit.yaml`. Outputs: `run-id`, `blocking`, `summary-path`. The runner needs `gcc` (cgo), `jq` and `gh` — all present on `ubuntu-latest`.

Until the repository goes public only the owner's repositories can `uses:` it; anywhere else, `go install github.com/SamuelMolling/godwit/cmd/godwit@main` and call the same commands (`--format markdown` on `lint`, `plan` and `migrate --dry-run`).

## Deploy

[deploy/helm/godwit](deploy/helm/godwit) runs the service on Kubernetes: two replicas, credentials from an existing Secret, `/readyz` and `/healthz` probes, a PodDisruptionBudget, optional ServiceMonitor and Ingress. Build the image with `docker build -t <registry>/godwit:<tag> .` and push it to your registry; there is no public image yet. `make helm-lint` lints and renders the chart with default and full values.

[deploy/argocd](deploy/argocd) wraps an application's sync with `godwit migrate --rollout expand-contract` as a PreSync hook and `godwit run confirm --latest --allow-none` as PostSync, so the contract phase is released only after the new pods are healthy.

## Notifications

`godwit serve` emits an event for every run transition (`created`, `running`, `succeeded`, `failed`, `needs_attention`, `awaiting_contract`, `confirmed`, `resumed`, `parked`, `reverted`) and every drift change (`detected`, `resolved`, `accepted`). Providers are enabled by environment only:

| Env | Effect |
|---|---|
| `GODWIT_SLACK_TOKEN` | Bot token (`chat:write`); setting it enables Slack. `GODWIT_SLACK_CHANNEL` is then required. |
| `GODWIT_SLACK_MODE` | `thread` (default): one root message per run, each transition a threaded reply, root kept current. `edit`: one message rewritten in place. Drift gets its own message per detection, with the resolution threaded under it. |
| `GODWIT_PUBLIC_URL` | Adds an "Open run" button pointing at `<url>/ui/runs/<id>`. |
| `GODWIT_WEBHOOK_URL` | POSTs every event as JSON (`kind`, `type`, `target`, `run_id`, `state`, `attempt`, `rollout`, `phase`, `detail`, `at`, `text`). |

Both providers can be on at once. Each has its own queue (256 events, one worker, 10 s per delivery, 3 retries with backoff and `Retry-After` on 429); a full queue drops the event with a warning rather than slowing a run. The Slack message timestamp is stored in the control-plane database, so any replica can keep the thread going. `godwit_notifications_total{provider,result}` counts `delivered`, `failed` and `dropped`.

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
| `godwit_notifications_total{provider,result}` | `failed` means Slack or the webhook is refusing events; `dropped` means the queue is full. |
| `godwit_api_requests_total{method,code}`, `godwit_api_request_duration_seconds{method}`, `godwit_build_info` | The service itself. |

Per-migration and per-statement series are deliberately absent — that detail lives in the run journal, not in label cardinality.

Probes live on the same listener, also unauthenticated: `GET /healthz` answers 200 while the process is up; `GET /readyz` answers 200 when the store replies to `SELECT 1` within 2s and 503 otherwise.

## Logging

`godwit serve` logs to stderr through `log/slog`. `--log-format json|text` (`GODWIT_LOG_FORMAT`, default `json`) picks the handler; `--log-level debug|info|warn|error` (`GODWIT_LOG_LEVEL`, default `info`) filters. Every line carries `replica` and `build`; the rest uses one key set so a log query reads the same everywhere:

| Key | Meaning |
|---|---|
| `run`, `target`, `attempt`, `state` | Which run, on which target, which attempt, where it ended (`run claimed`, `run finished`). |
| `version`, `stmt`, `kind`, `duration_ms` | Per-statement lines from the executor (with `run` and `target`): migration version, statement index, `tx` / `no_tx`, wall time. The SQL text is never logged. |
| `method`, `code`, `duration_ms` | Access log for every API call; `ok` at info, any other connect code at warn with the error text. |
| `error` | Error text where something went wrong (the pgx connect error, the SQLSTATE, the refused hazard). |

What never reaches the log: DSNs, tokens, credential files, Vault responses, target config, migration bodies. Clean drift checks log at debug, so `--log-level debug` is the way to watch the monitor tick. `/metrics`, `/healthz` and `/readyz` are plain HTTP routes and stay out of the access log.

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

## Testing

`make test` runs the unit and in-process integration suites (100% coverage enforced by `make cover`). `make e2e` builds the binary and drives it through the CLI against a real PostgreSQL in Docker: replicas run as OS processes and get SIGKILLed mid-statement, mid-index-build, under lock timeouts, across expand/contract, reverts and drift. It needs Docker, takes under a minute and is not part of CI.

## Status

🚧 v1 in progress — **PostgreSQL only, API-first, no UI yet** (Backstage plugin or standalone UI arrive in v1.1 on top of the same API).
