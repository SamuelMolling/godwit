# Security

What protects the targets, what the service holds, and what leaves it.

## Threat model in one paragraph

The service holds a way to reach every registered target as a role that can run DDL. Whoever can call `CreateRun` with `pipeline` scope can execute arbitrary SQL on a target (a migration is arbitrary SQL, hazards only flag known-dangerous shapes); whoever reads the store with the master key can decrypt every `static` DSN. Treat the `pipeline` token like the target's own credential and the store plus master key like a vault.

## Tokens and scopes

Bearer tokens are static secrets in `GODWIT_TOKENS`, compared in the auth interceptor on every RPC ([spec](configuration.md#token-spec), [per-RPC table](api.md#authentication-and-scopes)). Recommendations:

- one token per caller (`ci`, `argocd-orders`, `oncall`), named, so `cp_runs.created_by` and `cp_audit.actor` mean something;
- `read` for pull request plans and dry runs, `pipeline` for merge pipelines and ArgoCD hooks, `operator` for humans on call, `admin` only for the process that registers targets;
- never run with `GODWIT_TOKENS` unset outside a laptop: everyone becomes `anonymous` with `admin`;
- rotate by adding the new secret under the same name, rolling callers, removing the old one; the service refuses to start when two entries share a secret, and every change needs a restart (tokens are read at start-up).

Tokens are never logged; the access log carries `actor` (the name) and `scope`.

## Master key

`GODWIT_MASTER_KEY` is 32 bytes as 64 hex characters. `static` targets store their DSN in `cp_targets.config` as base64(nonce ‖ AES-256-GCM ciphertext) under that key. The key is read at start-up, held in memory, never written. Without it the service cannot connect to any static target, so back it up separately from the store (a store dump plus its key is every target credential).

### Rotation

There is no re-encryption command. The ciphertext does not identify the key, so rotation is re-registration:

1. Start the replicas with the new key (`kubectl create secret ... --dry-run | kubectl apply`, roll).
2. `godwit target add <name> --provider static --dsn <dsn> --lock-timeout ...` for every static target; `RegisterTarget` replaces the row in place, runs and history are untouched.
3. Until step 2 is done for a target, runs on it fail at claim with a `decrypt` error in `cp_runs.error` (`failed`, resumable once re-registered with `godwit run resume`).

Targets using the `kubernetes` or `vault` providers store no secret and need nothing.

## Credential providers

| Provider | Stored in `cp_targets.config` | Read at | Needs |
|---|---|---|---|
| `static` | `dsn` (encrypted) | every run, drift check, status, baseline | master key |
| `kubernetes` | `path` | every use: the file is read and trimmed, so a rotated Secret is picked up without restart | the Secret mounted at that path in the godwit pod |
| `vault` | `path`, `template` | every use: `GET <VAULT_ADDR>/v1/<path>`, KV v2 `data.data` unwrapped, `{{field}}` substituted into `template` | `VAULT_ADDR`, plus `VAULT_TOKEN` or Kubernetes auth (`VAULT_K8S_ROLE`, `VAULT_K8S_MOUNT`, `VAULT_K8S_JWT`) |

Vault login (`POST auth/<mount>/login` with the pod's service-account JWT) happens on every fetch; the client token is not cached. A missing template field fails with `vault secret has no field for {{x}}`. Prefer `vault` with dynamic database credentials: each run then gets a short-lived role.

The DSN, whichever provider produced it, exists only in the replica's memory for the duration of the operation and is never logged or returned by any RPC.

## Database privileges

**Store role**: owner of the store database, `CREATEDB` for validation scratch databases. Nothing else.

**Target role** (the one in the DSN): whatever the migrations need, plus `CREATE` on the database for the `godwit` schema on first contact. godwit takes `pg_advisory_lock` (no extra privilege) and reads `information_schema` / `pg_catalog` for status and drift. Give it a dedicated role rather than the application's, so `lock_timeout` and `statement_timeout` are set per run (`SET LOCAL` inside transactions, `SET`/`RESET` around no-tx statements) without touching application sessions, and so `pg_stat_activity` shows who is migrating.

The validation scratch database runs the migrations as the store role on the store server; migrations that reference roles, tablespaces or extensions that exist only on the target fail validation. Install the same extensions on the store server or run those with `--skip-validation`. Nothing records that a run skipped validation; if that matters, make the pipeline log it.

## What is logged

Every line in [operations: logging](operations.md#logging). Present: run ids, target names, actor names, scopes, statement index and kind, durations, error messages returned by PostgreSQL. Absent by construction: DSNs, tokens, the master key, Vault tokens, migration SQL text (the planner's statement text stays in the store's `cp_run_files` and in the response of `PlanRun`; the log carries `stmt=<index>` only).

PostgreSQL error messages can quote a fragment of the failing statement (`syntax error at or near "..."`); if migrations embed literals you consider secret, they will appear in `cp_runs.error`, in notifications and in the log.

Notifications carry the same fields as the log plus the error text; a webhook URL or Slack channel is therefore an audience for error messages, not for SQL.

## Audit

`cp_audit` records every admitted mutation with actor, action, run id, target and detail (`run.create` detail is `rollout=<policy> migrations=<n> acked=<codes> source=<source>`; `target.baseline` is `version=<v> migrations=<n>`; `run.park` carries the reason). `ListAudit` needs `read`. Failed writes are logged as `audit write failed` at error level and do not fail the request; alert on that line if the trail matters.

## Web UI

`serve --ui` mounts the operator UI at `/ui` on the same plaintext listener. It authenticates with HTTP basic auth — browsers send it natively, so there is no login page, no cookie and no session store — and it resolves to a `Principal` exactly like a bearer token does:

| Password | Identity | Scope |
|---|---|---|
| the secret of one of `GODWIT_TOKENS` (the username is ignored) | `ui:<token name>` | that token's scope |
| `--ui-password`, with `--ui-user` as the username | `ui:<user>` | `--ui-scope` (default `operator`) |
| anything else | refused with `401` and `WWW-Authenticate: Basic realm="godwit"` | — |

Every secret is compared in constant time as a SHA-256 digest, and the password is never logged. The UI is protected as soon as the service has tokens **or** the `--ui-user` / `--ui-password` pair; with neither it is open to anyone who reaches the port, acts as `ui:anonymous` with `--ui-scope`, and `serve` logs `ui enabled without basic auth` — treat that as a development setting.

The UI calls the service in process, so its actions appear in `cp_audit` under `ui:<name>` rather than under the token that a browser would have used over HTTP. That in-process path does **not** pass the auth interceptor, so the UI runs the same decision itself: every call goes through `api.Authorize(procedure, principal)`, the identical `procedure → scope` table the interceptor uses. A page therefore renders only the actions the scope allows, and a request posted around the page — `POST /ui/runs/<id>/resume` typed by hand — is refused with `403` and the scope message (`ResumeRun requires scope operator; token ui:viewer has scope read`). Scopes reach the UI as: `read` sees every page and can press nothing; `pipeline` adds confirm rollout and revert; `operator` adds resume, park, check drift and accept baseline, which is everything the UI offers; `admin` adds nothing, because the UI calls no admin RPC.

Basic auth sends the password on every request, so the TLS termination below is not optional when the UI is on.

## Network

- The listener is plaintext h2c/HTTP/1.1. Terminate TLS in front of it (the Helm Ingress needs an h2c- or gRPC-capable class, or a service mesh); the CLI accepts `https://` URLs.
- `/metrics`, `/healthz` and `/readyz` are unauthenticated. Scope them to the cluster network; `/metrics` label values include target names.
- The service dials out to: the store, every target, the store server for scratch databases, Vault, the webhook URL, `slack.com`. Egress rules need those and nothing else.
- Replicas do not talk to each other; the store is the only shared state.
- The container runs as non-root with a read-only root filesystem, no capabilities (chart defaults); the CLI in hook Jobs does the same.

## Supply chain

The binary embeds `libpg_query` through cgo; the Dockerfile builds it from source in the repository at a pinned Go version and ships a distroless image. `ghcr.io/samuelmolling/godwit` is built from `main` by `.github/workflows/publish.yml` with the workflow's own `GITHUB_TOKEN` (no long-lived registry credential) and carries `org.opencontainers.image.source` / `revision` labels; images are not signed yet, so pin `sha-<short commit>` or build and sign your own from the same Dockerfile. Release binaries (`v*` tags, GoReleaser) ship with a `checksums.txt`.
