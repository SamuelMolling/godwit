<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
  <img src="assets/logo-light.svg" alt="godwit" width="100%">
</picture>

> The bar-tailed godwit holds the world record for the longest non-stop migration: ~13,500 km without landing. Your database migrations should be as reliable.

**godwit** is a crash-safe PostgreSQL migration service for pipelines. Migrations are plain SQL files; they run under a statement-level journal in the target database, so a replica killed mid-run is taken over by another and resumed from the last committed statement. There is no dirty state and no `repair`. Apache 2, PostgreSQL only, one binary that is both the service and the CLI.

Flyway, Liquibase and Atlas moved undo, dry runs, lint and drift detection behind paid tiers, and none of them is a service: every pipeline rebuilds the same glue around a CLI. godwit is that glue done once. The honest side-by-side, including what godwit lacks, is in [docs/comparison.md](docs/comparison.md).

## Three things that are actually different

**The journal is in the target database, committed with the DDL.** A `tx` statement and its `done` row commit together; a `CREATE INDEX CONCURRENTLY` gets a write-ahead intent and a verifier that inspects `pg_index` after a crash; a backfill commits its rows and its cursor in the same transaction. So `kill -9` at any point leaves a state the next attempt can read and continue from, and nothing has to be repaired by hand. The point-by-point [crash timeline](docs/concepts.md#crash-timeline) is in the concepts page.

**A directive is a change godwit executes, not a recipe it prints.** A `-- godwit:` comment line says what the migration wants; godwit renders the lock-safe statements against the target's own catalog at plan time — the real primary key, column type and nullability — and freezes them onto the plan, so the run applies what the pull request showed:

```sql
-- 20260901121000_customer_id_text.up.sql
-- godwit: change-type orders.customer_id text using='customer_id::text'
-- godwit: assert 'SELECT count(*) FROM orders WHERE customer_id IS NULL' = 0
```

```
$ godwit plan --target app --dir db/migrations --rollout expand-contract
20260901121000_customer_id_text (up): 13 statement(s) [expand, pending]   directive, expand 7 / contract 6
  -- godwit: change-type orders.customer_id text using='customer_id::text'
  -- godwit: assert 'SELECT count(*) FROM orders WHERE customer_id IS NULL' = 0
  -- godwit expanded: change-type orders.customer_id text
  [0] tx    ALTER TABLE public.orders ADD COLUMN customer_id_new text   [expand]
  [1] tx    CREATE FUNCTION public.orders_customer_id_sync() RETURNS trigger LANGUAGE plpgsql AS …   [expand]
  [2] tx    CREATE TRIGGER orders_customer_id_sync BEFORE INSERT OR UPDATE ON public.orders …   [expand]
  [3] batch WITH b AS (SELECT id AS godwit_key FROM public.orders WHERE id > $1::bigint AND …)   [expand]
        batch over id (int), 5000 rows per transaction
  -- godwit expanded: assert 'SELECT count(*) FROM orders WHERE customer_id IS NULL' = 0
  [6] assert SELECT count(*) FROM orders WHERE customer_id IS NULL   [expand]
        the result must be = 0
  [9] tx    ALTER TABLE public.orders RENAME COLUMN customer_id TO customer_id_old   [contract]
  [10] tx    ALTER TABLE public.orders RENAME COLUMN customer_id_new TO customer_id   [contract]
  note: leaves public.orders.customer_id_old for rollback; drop it with `-- godwit: drop-column public.orders.customer_id_old`
```

(An excerpt: the run has thirteen statements.) The trigger keeps both columns in sync while the batches walk the table, the batches resume from their journalled cursor after a crash, the assertion is the last statement of the expand phase so a bad backfill never becomes the irreversible swap, and the rename waits in `awaiting_contract` until a human confirms it. Ten operations exist; everything godwit will not do safely is refused by name. [Concepts: directives](docs/concepts.md#directives).

**The plan is a contract, and it applies before the merge.** `godwit plan` stores the admitted plan with an observation of the live target; `migrate` binds to that plan and refuses with the exact diff when the target moved underneath. On a pull request the GitHub Action turns that into: lint and plan as a sticky comment, `/godwit apply` bound to the reviewed plan, `/godwit confirm` for the contract phase, and a `godwit/applied` commit status that stays `pending` until the whole migration is on the database. By the time the branch lands, `main` describes a schema the target already has. [Concepts: plans](docs/concepts.md#plans), [CI/CD](docs/ci-cd.md).

## Quickstart

```bash
go install github.com/SamuelMolling/godwit/cmd/godwit@main   # needs gcc (libpg_query) and Go 1.26
docker pull ghcr.io/samuelmolling/godwit:main               # or the image: linux/amd64 + arm64, distroless

# the service's own store, and a database to migrate
psql -U postgres \
  -c "CREATE ROLE godwit LOGIN PASSWORD 'godwit' CREATEDB" \
  -c "CREATE DATABASE godwit_store OWNER godwit" \
  -c "CREATE ROLE app LOGIN PASSWORD 'app'" \
  -c "CREATE DATABASE app OWNER app"

export GODWIT_MASTER_KEY=$(openssl rand -hex 32) GODWIT_TOKENS='admin:admin:s3cret'
godwit serve --store-dsn postgres://godwit:godwit@localhost/godwit_store &

export GODWIT_SERVER=http://localhost:8474 GODWIT_TOKEN=s3cret
godwit target add app --provider static --dsn postgres://app:app@localhost/app
godwit lint --dir db/migrations                     # hazards, parse errors, malformed directives
godwit plan --target app --dir db/migrations        # what would run, against the live target
godwit migrate --target app --dir db/migrations     # streams the run; exit 0 when applied
godwit target status app --dir db/migrations
```

That single-server form executes submitted SQL on the store server as the store role, which is why it needs `CREATEDB` and why `serve` warns about it on every start. Anywhere a token is shared, add `--scratch-dsn` pointing at a PostgreSQL that holds nothing ([security](docs/security.md#the-scratch-database)). The full walkthrough — the local `apply`/`status`/`down` loop with no service at all, writing a migration from an ORM schema, and the first CI step — is [docs/getting-started.md](docs/getting-started.md).

## What's inside

| | |
|---|---|
| Crash-safe engine | statement-level journal, write-ahead intents and verifiers for non-transactional statements, batched backfills resumed from their cursor — [concepts](docs/concepts.md#the-journal-protocol) |
| Leased service | any replica claims a run, a lost lease is taken over, transient failures retry with backoff, a re-run of the same pipeline job re-attaches instead of queueing a second — [concepts](docs/concepts.md#leases) |
| Hazard gate | `H001`–`H010` from a real PostgreSQL parser, each carrying the safe form as ready-to-copy SQL; refused unless acknowledged in the run — [concepts](docs/concepts.md#hazards) |
| Directives | ten `-- godwit:` operations godwit expands against the target's catalog and freezes onto the plan; `backfill` holds a sync trigger for the length of its batches — [concepts](docs/concepts.md#directives) |
| Assertions | `-- godwit: assert '<select>' = 0` makes a condition about the data a statement of the plan, journalled and re-checked at confirm time — [concepts](docs/concepts.md#assertions) |
| Admission | the hazard gate, an out-of-order guard, and a replay of the target's recorded history plus the new files on a throwaway database, before anything is queued — [concepts](docs/concepts.md#admission) |
| Plan as contract | the admitted plan is stored with an observation of the target; `migrate` binds to it, re-plans what other runs explain, refuses the rest, and records a migration already applied by hand instead of executing it — [concepts](docs/concepts.md#plans) |
| Apply before merge | composite GitHub Action: lint and plan on the pull request, `/godwit apply`, `/godwit confirm`, `/godwit revert`, `verify` on the merge commit; ArgoCD hooks and a Helm chart — [CI/CD](docs/ci-cd.md) |
| Expand → contract | the rollout is split by statement: the run parks in `awaiting_contract` and `ConfirmRollout` resumes the same run where it stopped — [concepts](docs/concepts.md#rollout-policies) |
| Revert | scoped to what the run actually applied, never to the directory it submitted; a plan that would destroy rows is refused, not warned about — [concepts](docs/concepts.md#revert) |
| Version targets | `--to <version>` stops a run short; the migrations above it stay on the plan marked **withheld**, so the report cannot be read as the whole set — [concepts](docs/concepts.md#version-targets) |
| Repeatables and checkpoints | `R__` files re-applied whenever their content changes; `godwit checkpoint` collapses old history into one file the replay runs instead — [concepts](docs/concepts.md#repeatable-migrations), [checkpoints](docs/concepts.md#checkpoints) |
| Drift and baseline | a schema fingerprint after every successful run, a periodic monitor, events and accept; `BaselineTarget` adopts a database godwit did not build — [concepts](docs/concepts.md#drift) |
| Migrations from a schema | `godwit diff` writes the next up/down pair from a DDL file or from Prisma, GORM, Django, Alembic, Rails, Drizzle or any command, all rendered client-side — [concepts](docs/concepts.md#generating-migrations-from-a-schema) |
| ORM drift gate | `godwit lint --server <url> --target <t>` fails (`E005`) when the committed SQL no longer expresses the ORM schema — [concepts](docs/concepts.md#keeping-the-generated-sql-and-the-orm-schema-together) |
| API, CLI and UI | connect (gRPC + JSON) with scoped bearer tokens (`read`, `pipeline`, `operator`, `admin`); the same binary is the CLI; `serve --ui` adds an operator UI at `/ui` — [API](docs/api.md), [configuration](docs/configuration.md), [operations](docs/operations.md#web-ui) |
| Credentials | `static` (AES-256-GCM in the store), `kubernetes` (mounted secret), `vault` (KV or dynamic) — [security](docs/security.md#credential-providers) |
| Operations | per-target timeouts and `search_path`, admission limits, Prometheus `/metrics`, audit on every mutation, webhook and Slack notifications — [operations](docs/operations.md), [configuration](docs/configuration.md) |

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | dev loop, service, first run, CI |
| [Concepts](docs/concepts.md) | the journal protocol, run states, leases, hazards, directives, validation, rollouts, revert, drift, checkpoints, plans |
| [Configuration](docs/configuration.md) | every `godwit.yaml` key, `serve` flag, environment variable, the token spec and the CLI reference |
| [Deployment](docs/deployment.md) | registering a target, the three credential providers, Vault end to end, Helm and ArgoCD, a staging checklist |
| [Operations](docs/operations.md) | HA, the store, backups, retention, upgrades, metrics and alert rules, notifications, logging, the UI |
| [Runbook](docs/runbook.md) | per symptom: the SQL to look at and the command to run |
| [CI/CD](docs/ci-cd.md) | Action inputs and outputs, who may command an apply, ArgoCD hooks, exit codes |
| [API](docs/api.md) | every RPC with its scope, request, response and curl |
| [Security](docs/security.md) | tokens, key providers and rotation, credential providers, the scratch database, what is logged |
| [Comparison](docs/comparison.md) | versus Flyway, Liquibase and Atlas, including the cut list |
| [Decisions](docs/decisions/README.md) | why godwit is shaped this way, and what was refused |

Also: [examples](examples/README.md) (copy-ready pipelines), [deploy/helm/godwit](deploy/helm/godwit/README.md), [deploy/argocd](deploy/argocd/README.md), the two-replica crash [demo](demo/README.md), and [AGENTS.md](AGENTS.md) for contributors.

## Design principles

1. **The journal is the truth.** Progress lives in the target database, committed with the DDL.
2. **Plain SQL.** Any stack can produce it; godwit runs it.
3. **Roll forward.** Down migrations are required, but the production path is expand → contract, and godwit schedules the contract.
4. **Safety is not a paid tier.**

## Status

v1 in progress: PostgreSQL only, API-first. Version stays `0.0.1` until v1 has run in production — nothing here has.

`make all` (lint, proto lint, the suites at 100% statement coverage, build) is the gate on every commit. Outside it, `make e2e` drives the built binary against PostgreSQL in Docker while SIGKILLing replicas mid-statement, `make load` measures a ten-million-row backfill and a thousand-migration target, and `make chaos` kills godwit in the gaps the crash rig cannot reach. Their numbers and knobs are in [docs/testing.md](docs/testing.md).
