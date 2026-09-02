# godwit

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

The store role needs `CREATEDB` (validation replays history on a scratch database). Full walkthrough, including the local `apply`/`status`/`down` loop without a service and the first CI step: [docs/getting-started.md](docs/getting-started.md).

## What's inside

| Feature | What it does |
|---|---|
| Crash-safe engine | Statement-level journal committed with the DDL; write-ahead intents and verifiers for `CREATE INDEX CONCURRENTLY` and friends; survives `kill -9` at any point. |
| Leased service | Any replica claims a run; lost leases are taken over and resumed from the journal; `--max-attempts` parks a run as `needs_attention`. |
| Hazard gate | `H001`–`H010` from a real PostgreSQL parser (`libpg_query`): non-concurrent indexes, destructive drops, rewrites, unvalidated constraints, renames. Refused unless acknowledged in the run. |
| Validation | Every run replays the target's recorded history plus the new files on a scratch database before it is queued. |
| Rollout policies | `direct`, or `expand-contract`: destructive migrations wait in `awaiting_contract` until `ConfirmRollout`. |
| Revert, baseline, status | `RevertRun` runs the down side through the same gate; `BaselineTarget` adopts an existing database; `GetTargetStatus` answers applied/pending/last run/drift in one call. |
| Drift detection | Fingerprint after every successful run, periodic monitor, events, accept. |
| Out-of-order guard and dry run | Older-than-applied versions are refused unless allowed; `PlanRun` shows the admitted plan without queueing. |
| Plan as contract | `godwit plan --target` stores the admitted plan with an observation of the target; `migrate` binds to it, re-plans when the only changes are explained by other runs, and refuses with the exact diff when the target moved underneath (`require_plan` makes a stored plan mandatory). |
| Credentials | `static` (AES-256-GCM in the store), `kubernetes` (mounted secret), `vault` (KV or dynamic credentials). |
| API and CLI | connect (gRPC + JSON) with scoped bearer tokens (`read`, `pipeline`, `operator`, `admin`); the same binary is the CLI, with `godwit.yaml` for defaults. |
| Audit, metrics, logs | Actor on every mutation, `created_by` and `source` on runs; Prometheus `/metrics`; structured `slog` that never prints a DSN, token or SQL body. |
| Notifications | Webhook JSON and Slack (threaded or edited in place), off the run's critical path. |
| CI/CD and deploy | Composite GitHub Action (`lint`, `plan`, `migrate`, dry run, sticky PR comment), ArgoCD PreSync/PostSync hooks, Helm chart. |

## Documentation

| | |
|---|---|
| [Getting started](docs/getting-started.md) | dev loop, service, first run, CI |
| [Concepts](docs/concepts.md) | journal protocol and crash timeline, run states, leases, hazards, validation, rollouts, revert, drift, baseline |
| [Configuration](docs/configuration.md) | every `godwit.yaml` key, `serve` flag and environment variable, token spec, CLI reference |
| [Operations](docs/operations.md) | HA, store sizing and privileges, backups, retention, upgrades, metrics and alert rules, notifications, logging |
| [Runbook](docs/runbook.md) | per symptom: the SQL to look at and the command to run |
| [CI/CD](docs/ci-cd.md) | Action inputs and outputs, ArgoCD hooks, exit codes, expand → contract |
| [API](docs/api.md) | every RPC with scope, request, response and curl |
| [Security](docs/security.md) | tokens, master key rotation, providers, what is logged, network |
| [Comparison](docs/comparison.md) | versus Flyway, Liquibase and Atlas, including the cut list |

Also: [examples](examples/README.md) (copy-ready pipelines), [deploy/helm/godwit](deploy/helm/godwit/README.md), [deploy/argocd](deploy/argocd/README.md), the two-replica crash [demo](demo/README.md), and [AGENTS.md](AGENTS.md) for contributors.

## Design principles

1. **The journal is the truth.** Progress lives in the target database, committed with the DDL.
2. **Plain SQL.** Any stack can produce it; godwit runs it.
3. **Roll forward.** Down migrations are required, but the production path is expand → contract, and godwit schedules the contract.
4. **Safety is not a paid tier.**

## Testing

`make all` runs lint, proto lint, the unit and in-process integration suites at 100% coverage, and the build. `make e2e` drives the built binary through the CLI against PostgreSQL in Docker, SIGKILLing replicas mid-statement, mid-index-build, under lock timeouts, across expand/contract, reverts and drift; it needs Docker and is not part of CI.

## Status

v1 in progress: PostgreSQL only, API-first. UI in progress on the `feat/ui` branch (PR #16, not merged). Version stays `0.0.1` until v1 has run in production.
