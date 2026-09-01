# godwit

> The bar-tailed godwit holds the world record for the longest non-stop migration — ~13,500 km without landing. Your database migrations should be just as reliable.

**godwit** is a pipeline-native database migration service. Platform teams get a golden path for schema changes — from a Backstage form to a GitOps apply — with crash-safe execution, linting, drift detection and rollback, all free of vendor paywalls. Migrations execute under a statement-level journal protocol that survives executor crashes with automatic recovery — no "dirty state", ever.

## Why

The 2025–2026 licensing landscape pushed every incumbent's safety features behind paywalls:

- **Flyway**: undo migrations and dry-runs are Teams/Enterprise-only; Teams is closed to new customers.
- **Liquibase**: automated drift detection, policy checks and targeted rollbacks require a Secure license.
- **Atlas**: `migrate lint`, `migrate down`, drift detection and all integrations moved to the paid Pro tier (v0.38); drift detection additionally requires their cloud registry.

Meanwhile there is **no Backstage plugin for database migrations at all** — every org rebuilds the same glue. godwit is that glue, done once, as open source.

## What's inside (Phase 1 — toolkit)

| Component | What it does | Built on |
|---|---|---|
| Execution engine | Applies plain-SQL migrations under a crash-safe statement-journal protocol | pgx + [libpg_query](https://github.com/pganalyze/pg_query_go) |
| CI action | Lints unsafe DDL on pull requests, validates migration pairs | [squawk](https://squawkhq.com/) |
| Drift check | Scheduled job that diffs live schema vs. desired state, fails on drift | [pg-schema-diff](https://github.com/stripe/pg-schema-diff) |
| ArgoCD PreSync manifest | Kubernetes Job template that migrates before each rollout | ArgoCD hooks |
| Backstage golden path | Scaffolder template + custom actions: form → migration file → PR with reviewers | Backstage Scaffolder |

## Engines

| Engine | Migrations | Lint | Drift | Notes |
|---|---|---|---|---|
| PostgreSQL | ✅ SQL up/down | ✅ squawk | ✅ pg-schema-diff | Full support |
| Cassandra | ✅ CQL up/down | — | — | No locking in driver; serialize Jobs |
| MongoDB | ✅ JSON `runCommand` | — | — | Advisory locking + transactions (replica set) |

## Design principles

1. **Git is the state, GitOps does the apply.** godwit never holds production credentials in a long-lived service; the apply happens in your cluster, next to your app.
2. **Plain SQL, language-agnostic.** Works identically for TypeScript, Go, or any other stack. ORMs (Prisma, Drizzle) can generate the SQL; godwit runs it.
3. **Roll forward, not back.** Down migrations are required and CI-tested, but the production playbook is a corrective forward migration (expand → contract).
4. **Safety is not a paid tier.** Lock timeouts, `CONCURRENTLY` enforcement, hazard linting and drift alerts ship free.

## Roadmap

- **Phase 1 (now)**: toolkit — runner image, CI action, PreSync manifests, Backstage template, drift cron.
- **Phase 2 (if demand proves it)**: control plane — API + dashboard for fleet-wide migration state, audit trail and drift overview, consumed by a Backstage plugin.

## Try it

```bash
cd demo && docker compose up -d --build && ./demo.sh
```

Two replicas, a store and a target database. The script kills the replica executing a slow migration; the other one recovers the lease and finishes the run from the journal. See [demo/README.md](demo/README.md).

## Status

🚧 v1 in progress — **PostgreSQL only, API-first, no UI yet** (Backstage plugin or standalone UI arrive in v1.1 on top of the same API).
