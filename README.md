<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
  <img src="assets/logo-light.svg" alt="godwit" width="100%">
</picture>

> The bar-tailed godwit holds the world record for the longest non-stop migration: ~13,500 km without landing. Your database migrations should be as reliable.

**godwit** is a crash-safe PostgreSQL migration service for pipelines. Plain-SQL migrations run under a statement-level journal in the target database, so a replica killed mid-run is taken over and resumed from the last committed statement; there is no dirty state and no `repair`. Around the executor: a hazard gate for unsafe DDL, scratch-database validation before every run, expand/contract rollouts, reverts, drift detection, scoped tokens, audit, metrics and notifications, all in one binary that is also the CLI.

## Why

Flyway, Liquibase and Atlas moved undo, dry runs, lint and drift detection behind paid tiers, and none of them is a service: each pipeline rebuilds the glue around a CLI. godwit is that glue done once, Apache 2, PostgreSQL only, plain SQL only. The honest side-by-side, including what godwit lacks, is in [docs/comparison.md](docs/comparison.md).

## Quickstart

```bash
go install github.com/SamuelMolling/godwit/cmd/godwit@main      # needs gcc (libpg_query) and Go 1.26
docker pull ghcr.io/samuelmolling/godwit:main                       # or the image: linux/amd64 + arm64, distroless
export GODWIT_MASTER_KEY=$(openssl rand -hex 32) GODWIT_TOKENS='admin:admin:s3cret'
godwit serve --store-dsn postgres://godwit:godwit@localhost/godwit_store &
export GODWIT_SERVER=http://localhost:8474 GODWIT_TOKEN=s3cret
godwit target add app --provider static --dsn postgres://app:app@localhost/app
godwit lint --dir db/migrations                                     # hazards, parse errors, edited history
godwit migrate --target app --dir db/migrations                     # streams the run; exit 0 when applied
godwit target status app --dir db/migrations
```

That single-server form executes submitted SQL on the store server as the store role, which needs `CREATEDB` for it and which `serve` warns about on every start; anywhere a token is shared, add `--scratch-dsn` pointing at a PostgreSQL that holds nothing ([security](docs/security.md#the-scratch-database)). Full walkthrough, including the local `apply`/`status`/`down` loop without a service and the first CI step: [docs/getting-started.md](docs/getting-started.md).

## What's inside

| Feature | What it does |
|---|---|
| Crash-safe engine | Statement-level journal committed with the DDL; write-ahead intents and verifiers for `CREATE INDEX CONCURRENTLY` and friends; survives `kill -9` at any point. |
| Leased service | Any replica claims a run; lost leases are taken over and resumed from the journal; transient failures (lock timeout, deadlock, lost connection) retry with backoff, a pipeline re-run re-attaches to the existing run, and `--max-attempts` parks a run as `needs_attention`. |
| Hazard gate | `H001`–`H010` from a real PostgreSQL parser (`libpg_query`): non-concurrent indexes, destructive drops, rewrites, unvalidated constraints, renames. Refused unless acknowledged in the run. |
| Safe-DDL recipes | Every hazard carries the safe form as ready-to-copy SQL with the real names from the statement (`CREATE INDEX CONCURRENTLY ...`, `CHECK ... NOT VALID` → `VALIDATE` → `SET NOT NULL`, add column → backfill → swap for a type change), in `lint`, `plan` and the API. |
| Directives | A `-- godwit: <op> ...` comment line in a migration declares the intent (`change-type`, `backfill`, `add-not-null`, `add-column`, `add-index`, `drop-index`, `add-fk`, `add-check`, `drop-column`, `assert`) instead of the lock-safe SQL. Parsed offline at load time and checked by `lint` (`E004`); every hazard recipe prints the equivalent directive. |
| Directives, expanded | Every directive is rendered into real statements against the scratch catalog at plan time — the table's own primary key, column type and nullability, not a guess — and frozen into the plan, so the run applies what the pull request showed. `change-type` adds the column, keeps both in sync with a trigger, backfills in resumable batches and swaps them in a held contract phase, leaving `<c>_old` as a lossless rollback. Everything it will not do safely is refused by name — including a column anything in `pg_depend` reads, because a rename moves views, indexes, constraints, triggers, rules, policies, publications and statistics onto `<c>_old` without an error. |
| The simple directives | `add-not-null`, `add-column`, `add-index`, `drop-index`, `add-fk` and `add-check` expand into the same lock-safe forms the hazard recipes print, with the catalog's own generated names: `NOT VALID` then `VALIDATE`, `CREATE INDEX CONCURRENTLY` after clearing the invalid leftover of an interrupted build, a batched fill before a new column is constrained, and a CHECK already saying `IS NOT NULL` reused rather than duplicated. `drop-column` lands in the contract phase, so the run waits for a human before the column goes. |
| Assertions | `-- godwit: assert 'SELECT count(*) FROM orders WHERE total IS NULL' = 0` states a condition about the **data** and makes it part of the plan: parsed offline as a single read-only `SELECT`, rendered in `plan` and in the pull-request comment with its condition, executed inside the run in a read-only transaction and journalled like any other statement. Ahead of the migration's SQL it is a precondition; after a `change-type` or `backfill` it is the last statement of the expand phase, so a bad backfill never becomes the irreversible swap — and `ConfirmRollout` re-checks it before the swap runs. A condition that does not hold fails the run by name, with no retry. |
| Validation | Every run replays the target's recorded history plus the new files on a scratch database before it is queued. That database is a sandbox: `--scratch-dsn` puts it on a PostgreSQL of its own, cloned from `template0`, under a role that owns nothing else, and `serve` refuses to start when that role could reach past it ([decision 0009](docs/decisions/0009-scratch-databases-are-not-the-store.md)). |
| Checkpoints | `godwit checkpoint --name squash` collapses the migrations up to a version into one file marked `-- godwit: checkpoint through=<version>`, carrying the schema they produce and no down side. Generated from a scratch replay of the files, never from a live target, and refused unless applying the generated body alone reproduces the same fingerprint. A database with no history runs it and records what it collapses; every other one records it and runs nothing of it; every scratch replay starts from it. What it collapses can no longer be reverted, and godwit says so instead of running down files against a state the target never passed through ([concepts](docs/concepts.md#checkpoints)). |
| Repeatable migrations | `R__<name>.up.sql` / `.down.sql` have no version: applied after the run's versioned files, in name order, whenever the content differs from what the target recorded in `godwit.repeatables`, and skipped when it does not. Same hazard gate, same journal, same plan contract; `lint`'s edited-after-merge check does not apply to them, and `godwit diff` counts what they declare as part of the desired schema. |
| Batched statements | A backfill is one plan statement with a cursor journalled beside it: each batch commits its rows and the new cursor together, with a configurable size and pause. A kill mid-backfill resumes from the cursor, so no row is redone and none is skipped. |
| Rollout policies | `direct`, or `expand-contract`: destructive statements wait in `awaiting_contract` until `ConfirmRollout`, which resumes the same run from the statement it stopped at. |
| Revert, baseline, status | `RevertRun` undoes **what a run actually applied**, from the per-migration ledger and never from the directory it submitted, newest migration first: the newest un-reverted run by default (an older one takes `--force`), the plan printed before anything runs (`--dry-run` alone), a refusal when it would drop a table or column that still holds rows (`--allow-data-loss`), and the revert recorded as a new run rather than a hole in the history ([concepts](docs/concepts.md#revert), [runbook](docs/runbook.md#reverting-a-run)); `BaselineTarget` adopts an existing database; `GetTargetStatus` answers applied/pending/last run/drift in one call. |
| Target inventory | `ListTargets` (`godwit targets`) summarises every registered target from the control plane alone — settings, applied count, ready plans, runs waiting for a human, open drift, last run — without opening a connection to any of them, so it answers while a target is down. |
| Drift detection | Fingerprint after every successful run, periodic monitor, events, accept. |
| Out-of-order guard and dry run | Older-than-applied versions are refused unless allowed; `PlanRun` shows the admitted plan without queueing. |
| Version targets | `plan --to <version>` and `migrate --to <version>` stop at a chosen migration and leave the rest pending. The whole directory is still submitted and the migrations above the version stay on the plan marked **withheld**, so the pull-request comment cannot be read as the whole set; repeatables are held back with them. A version the directory does not hold, one behind what the target applied (`--to` never reverts), one that selects nothing while work above it is pending, and `--to` on a stored plan are each refused by name ([concepts](docs/concepts.md#version-targets)). |
| Plan as contract | `godwit plan --target` stores the admitted plan with an observation of the target; `migrate` binds to it, re-plans when the only changes are explained by other runs, and refuses with the exact diff when the target moved underneath (`require_plan` makes a stored plan mandatory). |
| Plan inspection and override | `godwit plans` / `godwit plan show <id>` list and show stored plans with their state and the run that applied them; `migrate --plan <id>` binds one explicitly, files optional; `--plan-retention` prunes bound and superseded plans. |
| Already applied by hand | A validated plan spots pending migrations whose effect is already on the target (as a prefix, DDL only) and the run records them with zero statements instead of executing; DML, non-inspectable effects and out-of-prefix changes are refused with the reason. |
| Migrations from a desired schema | `godwit diff --schema schema.sql --name add_status` writes the next `up`/`down` pair from the live target to the DDL you want (pg-schema-diff under the hood), with hazards and recipes on the result and the drift it would absorb; `--prisma prisma/schema.prisma`, `--gorm ./cmd/schema`, `--django manage.py`, `--alembic alembic.ini`, `--rails .`, `--drizzle drizzle.config.ts` and `--exec '<command>'` render the model with the project's own toolchain instead of a dump — Prisma's CLI, `go run` over GORM's dry-run migrator, `showmigrations` + `sqlmigrate`, Alembic's offline `upgrade head --sql`, the `db/structure.sql` Rails already commits (`db/schema.rb` is refused: a Ruby DSL needs ActiveRecord and a database), `drizzle-kit export`, or any command whose stdout is DDL — all client-side and none of them opening a connection, so the service never sees your source tree. A `schema_source` block in `godwit.yaml` declares the source of the directory once, so `godwit diff --name add_status` needs no source flag and a monorepo keeps one source per migration directory. `--dir` is sent along and its `R__` migrations are built on the desired schema, so objects a repeatable declares are never proposed as drops; a diff that cannot see the directory on a target that has run repeatables is refused instead. |
| ORM drift gate | `godwit lint --server <url> --target <t>` replays the committed migrations on a scratch database — the recorded history, then the files — and compares the result with the schema `schema_source` declares. Empty means the committed SQL still expresses the ORM schema; anything left is printed as `E005`, whether someone hand-edited a generated `.sql`, changed the ORM schema without regenerating, or deleted a migration. `schema_source.lint: false` makes it a warning; with no server the check reports `W002` and lint stays offline. |
| Per-target `search_path` | `godwit target add --search-path app,public`: every session godwit opens on the target (run, revert, plan, diff, scratch validation) resolves unqualified names there, while the journal stays schema-qualified in `godwit`; the effective path is part of a plan's observation, so a plan taken under one path will not bind under another. |
| Credentials | `static` (AES-256-GCM in the store), `kubernetes` (mounted secret), `vault` (KV or dynamic credentials). |
| API and CLI | connect (gRPC + JSON) with scoped bearer tokens (`read`, `pipeline`, `operator`, `admin`, written `name:scope:secret`); the same binary is the CLI, with `godwit.yaml` for defaults. |
| Admission limits | Request body, migration file count and size, desired-schema size and list page size are bounded, and the calls that build scratch databases — `Diff`, `PlanRun`, `CreateRun`, `RevertRun`, `Checkpoint` — queue behind `--max-concurrent-diffs`. Each replica runs up to `--max-concurrent-runs` runs at once, each on its own goroutine under `--run-timeout`, so a batched backfill on one target never parks the others ([configuration](docs/configuration.md#admission-limits), [operations](docs/operations.md#admission-limits)). |
| Audit, metrics, logs | Actor on every mutation, `created_by` and `source` on runs; Prometheus `/metrics`; structured `slog` that never prints a DSN, token or SQL body. |
| Notifications | Webhook JSON and Slack (threaded or edited in place), off the run's critical path. |
| UI | `serve --ui` serves an operator UI at `/ui`: needs-you queue, run timeline with the statement it is on, live backfill progress (rows written and batches committed, against the estimate the backfill started from) and resume/park/confirm/revert, every registered target with its settings and one page per target (applied and repeatables with checksum mismatches, what the newest ready plan still has to apply, ready plans, drift with check and accept), stored plans filtered by target and state, one plan in full (statements per phase, every hazard with its recipe, already-applied effects, directives and their expansion, the observation and the drift it was taken against), drift events and accept, and `/ui/diff` — paste the desired schema as DDL and get the up/down migration, its classified statements with hazard recipes and the drift block, with the filenames to save them under (nothing is written to disk; Prisma/GORM/Django/Alembic/Rails/Drizzle schemas stay client-side, on `godwit diff`); on a target that records repeatable migrations the page supplies the `R__` bodies from the newest stored plan, the run that last succeeded, or boxes on the page, and says which snapshot it used and where it disagrees with what the target recorded. Sign in with any bearer token's secret as the basic-auth password (`ui:<token name>`, the token's scope) or with `--ui-user` / `--ui-password` / `--ui-scope`; pages offer only what the scope allows and anything beyond it is a `403`. Actions audited as `ui:<name>`. Every form post must come from the UI's own origin (`Sec-Fetch-Site`, falling back to `Origin`), so a page elsewhere cannot make a signed-in browser act; `--ui-origin` names the origins and hosts it answers on. |
| CI/CD and deploy | Composite GitHub Action, apply before merge: `lint` and `plan` on the pull request (the plan is stored on the service and shown as a sticky comment with the observation and the changes outside migrations), `/godwit apply` on the pull request runs it bound to that plan and sets the `godwit/applied` status the merge requires, `/godwit confirm` runs the contract phase of a two-phase apply (until it does, the status stays `pending`), `verify` on the merge commit proves `main` carries nothing unapplied, `/godwit revert` when the pull request is abandoned; every command authorised by the commander's repository permission and an approving review standing on the exact commit, never by `author_association`, with `pull_request_target` refused; `apply-on-merge` mode for migrate on push; outputs `plan-id`/`plan-key`/`stale`/`phase`/`run-id`/`pending`; ArgoCD PreSync/PostSync hooks, Helm chart. |

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | dev loop, service, first run, CI |
| [Concepts](docs/concepts.md) | journal protocol and crash timeline, run states, leases, hazards, directives, validation, repeatables, version targets, rollouts, revert, drift, baseline, checkpoints, migrations from a schema |
| [Configuration](docs/configuration.md) | every `godwit.yaml` key, `serve` flag and environment variable, token spec, CLI reference |
| [Operations](docs/operations.md) | HA, store sizing and privileges, backups, retention, checkpoints, upgrades, metrics and alert rules, notifications, logging |
| [Runbook](docs/runbook.md) | per symptom: the SQL to look at and the command to run |
| [CI/CD](docs/ci-cd.md) | Action inputs and outputs, ArgoCD hooks, exit codes, expand → contract |
| [API](docs/api.md) | every RPC with scope, request, response and curl |
| [Security](docs/security.md) | tokens, master key rotation, providers, what is logged, network |
| [Comparison](docs/comparison.md) | versus Flyway, Liquibase and Atlas, including the cut list |
| [Decisions](docs/decisions/README.md) | why godwit is shaped this way: the plan contract, directives, ORM sources, the UI, revert, repeatables, assertions, checkpoints |

Also: [examples](examples/README.md) (copy-ready pipelines), [deploy/helm/godwit](deploy/helm/godwit/README.md), [deploy/argocd](deploy/argocd/README.md), the two-replica crash [demo](demo/README.md), and [AGENTS.md](AGENTS.md) for contributors.

## Design principles

1. **The journal is the truth.** Progress lives in the target database, committed with the DDL.
2. **Plain SQL.** Any stack can produce it; godwit runs it.
3. **Roll forward.** Down migrations are required, but the production path is expand → contract, and godwit schedules the contract.
4. **Safety is not a paid tier.**

## Testing

`make all` runs lint, proto lint, the unit and in-process integration suites at 100% coverage, and the build. `make e2e` drives the built binary through the CLI against PostgreSQL in Docker, SIGKILLing replicas mid-statement, mid-index-build, under lock timeouts, across expand/contract, reverts and drift; it needs Docker and is not part of CI.

## Status

v1 in progress: PostgreSQL only, API-first. Version stays `0.0.1` until v1 has run in production.
