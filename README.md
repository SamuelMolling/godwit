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
| Hazard gate | The planner ([libpg_query](https://github.com/pganalyze/pg_query_go)) tags unsafe DDL (`H001` non-concurrent index, `H002` `DROP TABLE`, `H003` `DROP COLUMN`, `H004` type rewrite, `H005` `NOT NULL` without default). Runs carrying unacknowledged hazards are refused. |
| Pre-apply validation | Every run replays the target's history plus the new files on a scratch database before it is queued. |
| Drift detection | Schema fingerprint after each run; a monitor diffs the live schema, records events, notifies (webhook) and auto-resolves. `AcceptBaseline` blesses manual changes. |
| Rollout policies | `direct` applies everything now. `expand-contract` applies additive migrations at PreSync and holds the first destructive migration (and everything after it) until `ConfirmRollout` — blue/green safe. |
| Credentials | Pluggable providers: `static` (AES-GCM-encrypted in the store) and `kubernetes` (mounted secret). Vault next. |
| API | gRPC and JSON over one connect endpoint, bearer-token auth, `WatchRun` streaming. |

## Rollout policies

With `expand-contract`, put contract statements (drops) in their own migration file. The run stops in `awaiting_contract` once the expand phase is applied; the previous app version keeps working. Your deploy pipeline (or an ArgoCD PostSync hook) calls `ConfirmRollout` after the new version is healthy, and the contract phase runs.

```
CreateRun{rollout: "expand-contract"}  →  running  →  awaiting_contract  →  ConfirmRollout  →  running  →  succeeded
```

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
