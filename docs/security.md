# Security

What protects the targets, what the service holds, and what leaves it.

## Threat model in one paragraph

The service holds a way to reach every registered target as a role that can run DDL. Whoever can call `CreateRun` with `pipeline` scope can execute arbitrary SQL on a target (a migration is arbitrary SQL, hazards only flag known-dangerous shapes); whoever reads the store with the master key can decrypt every `static` DSN. Treat the `pipeline` token like the target's own credential and the store plus master key like a vault.

## Tokens and scopes

Bearer tokens are static secrets in `GODWIT_TOKENS`, compared in the auth interceptor on every RPC ([spec](configuration.md#token-spec), [per-RPC table](api.md#authentication-and-scopes)). Recommendations:

- one token per caller (`ci`, `argocd-orders`, `oncall`), named, so `cp_runs.created_by` and `cp_audit.actor` mean something;
- `read` for pull request plans and dry runs, `pipeline` for merge pipelines and ArgoCD hooks, `operator` for humans on call, `admin` only for the process that registers targets;
- never run with `GODWIT_TOKENS` unset outside a laptop: everyone becomes `anonymous` with `admin`, and `serve` says so at warn level on every start (`no tokens configured`);
- write the scope out. A spec is `name:scope:secret` or a bare secret, and nothing else: the old two-field `name:secret` form meant *admin*, so `GODWIT_TOKENS=deploy:pipeline` read as an admin token whose secret was the word `pipeline`. It is refused now ([decision 0011](decisions/0011-token-spec-is-three-fields.md));
- rotate by adding the new secret under the same name, rolling callers, removing the old one; the service refuses to start when two entries share a secret, and every change needs a restart (tokens are read at start-up).

Tokens are never logged; the access log carries `actor` (the name) and `scope`.

The identity behind a call fails closed. A context that never passed the auth interceptor carries **no scope**, so every procedure is denied — it used to default to an anonymous `admin`, which meant any future path reaching a handler around the interceptor would have run as admin. The open-service principal (no `GODWIT_TOKENS`) is now built explicitly where the tokens are parsed, not inherited from a missing value.

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

Vault login (`POST auth/<mount>/login` with the pod's service-account JWT) happens on every fetch; the client token is not cached. A missing template field fails with `vault secret has no field for x`, naming the template's own keys and never the rendered string — the substitution is one pass over the template, so a field whose value contains `{{...}}` is not rescanned either. Prefer `vault` with dynamic database credentials: each run then gets a short-lived role.

The DSN, whichever provider produced it, exists only in the replica's memory for the duration of the operation and is never logged or returned by any RPC. Two paths used to break that:

- a Vault template with a missing field returned everything from the first unresolved marker to the end of the *partially rendered* string, so `postgres://{{missing}}:{{password}}@host/db` handed the substituted password back to the caller, into `cp_runs.error` and into Slack;
- a `pgx` parse or dial failure redacts the password and nothing else, so the target's host, port, user and database name — and, for a `kubernetes` target whose `path` names a file that is not a DSN, that file's contents — reached any `read` caller inside an `internal` error.

Both now return `cannot reach the database for this call; the detail is in the server log`, and the access log carries the original under `detail`.

## Database privileges

**Store role**: owner of the store database. Nothing else — `CREATEDB` is only needed when scratch databases stay on the store server, which is the configuration below tells you not to keep.

**Target role** (the one in the DSN): whatever the migrations need, plus `CREATE` on the database for the `godwit` schema on first contact. godwit takes `pg_advisory_lock` (no extra privilege) and reads `information_schema` / `pg_catalog` for status and drift. Give it a dedicated role rather than the application's, so `lock_timeout` and `statement_timeout` are set per run (`SET LOCAL` inside transactions, `SET`/`RESET` around no-tx statements) without touching application sessions, and so `pg_stat_activity` shows who is migrating.

**Scratch role**: `LOGIN CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS`, owner of nothing but the databases it makes for itself.

## The scratch database

`Diff`, `PlanRun` and `CreateRun` all execute submitted SQL — the pasted desired schema, the `R__` bodies, every migration file in the pull request — on a throwaway database, to find out what it produces before anything reaches a target. `Diff` needs only `read` scope, so **the weakest credential in the system runs DDL of the caller's choosing on whatever PostgreSQL that database lives on**. Treat that server as hostile ground.

`--scratch-dsn` (or `GODWIT_SCRATCH_DSN`) says where it lives. Point it at a PostgreSQL that holds nothing:

```sql
-- on the scratch server, as a superuser, once
CREATE ROLE godwit_scratch LOGIN PASSWORD '...'
  CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS;
```

The role needs `CREATEDB` and `CONNECT` on whatever database the DSN names (`postgres` is the usual choice); it makes, owns and drops its own `godwit_diff_<id>` / `godwit_validate_<id>` databases and nothing else. What that buys, all of it verified against PostgreSQL 17 rather than read off the manual:

| Submitted DDL tries | Result |
|---|---|
| `DROP DATABASE <store> WITH (FORCE)` | `must be owner of database` |
| `COPY … FROM PROGRAM 'id'` | `permission denied to COPY to or from an external program` |
| `pg_read_file('/etc/passwd')` | `permission denied for function pg_read_file` |
| `CREATE EXTENSION dblink` / `postgres_fdw` | `permission denied to create extension` (neither is a trusted extension) |
| `ALTER ROLE <store> …`, `CREATE ROLE` | `permission denied to alter role` / `to create role` |

Scratch databases are cloned from **`template0`**, not from the default `template1`, so an extension an operator installed into `template1` — `dblink` above all — is not inherited by a database submitted DDL runs in. `--scratch-template <db>` names a prepared template instead, for migrations that need extensions the scratch server would otherwise lack; whatever is in that template is reachable from every diff, so put only what validation needs in it.

**At start-up godwit inspects the scratch role and refuses to serve** when it is a superuser, owns the store database, is a member of `pg_execute_server_program` / `pg_read_server_files` / `pg_write_server_files`, or holds `CREATEROLE` or `REPLICATION`. There is no flag to override that: the way to run without the check is to leave `--scratch-dsn` unset, which is the fallback below, and it is loud.

**Without `--scratch-dsn` scratch databases stay on the store server under the store's own role**, which is what every version before this one did. The store role needs `CREATEDB` again, and submitted DDL then runs as the owner of the control-plane database: one statement can `DROP DATABASE <store> WITH (FORCE)` and take `cp_runs`, `cp_plans` and `cp_audit` with it, and if that role is a superuser it reaches command execution on the store host. `serve` logs `scratch database is not isolated` for each finding plus a warning naming the fix, on every start. Use it on a laptop; do not run it where anyone but you holds a token.

Whichever way it is configured, a scratch role that can reach the store database over the network is one extension away from reading `cp_targets.config`. `REVOKE CONNECT ON DATABASE <store> FROM PUBLIC` and grant it back only to the store role; `serve` warns when the scratch role still has it.

Migrations that reference roles, tablespaces or extensions that exist only on the target fail validation. Give the scratch server the extensions through `--scratch-template`, or run those with `--skip-validation`. Nothing records that a run skipped validation; if that matters, make the pipeline log it.

**Residual, named rather than hidden.** All scratch databases share one role, so DDL submitted by one caller can drop another caller's in-flight scratch database and fail their validation. Nothing bounds how many are created, how large they grow, or how long a statement in one runs; the scratch server needs its own disk and `max_connections` headroom, and it should not be shared with anything that matters.

## What is logged

Every line in [operations: logging](operations.md#logging). Present: run ids, target names, actor names, scopes, statement index and kind, durations, error messages returned by PostgreSQL. Absent by construction: DSNs, tokens, the master key, Vault tokens, migration SQL text (the planner's statement text stays in the store's `cp_run_files` and in the response of `PlanRun`; the log carries `stmt=<index>` only).

PostgreSQL error messages can quote a fragment of the failing statement (`syntax error at or near "..."`); if migrations embed literals you consider secret, they will appear in `cp_runs.error`, in notifications and in the log.

Notifications carry the same fields as the log plus the error text; a webhook URL or Slack channel is therefore an audience for error messages, not for SQL.

## Audit

`cp_audit` records every admitted mutation with actor, action, run id, target and detail (`run.create` detail is `rollout=<policy> migrations=<n> acked=<codes> source=<source>`; `target.baseline` is `version=<v> migrations=<n>`; `run.park` carries the reason). `ListAudit` needs `read`. Failed writes are logged as `audit write failed` at error level and do not fail the request; alert on that line if the trail matters.

## Web UI

Why the UI has no account model of its own: [decision 0004](decisions/0004-ui-is-a-scoped-client.md).

`serve --ui` mounts the operator UI at `/ui` on the same plaintext listener. It authenticates with HTTP basic auth — browsers send it natively, so there is no login page, no cookie and no session store — and it resolves to a `Principal` exactly like a bearer token does:

| Password | Identity | Scope |
|---|---|---|
| the secret of one of `GODWIT_TOKENS` (the username is ignored) | `ui:<token name>` | that token's scope |
| `--ui-password`, with `--ui-user` as the username | `ui:<user>` | `--ui-scope` (default `operator`) |
| nothing, on a service with neither tokens nor `--ui-user` | `ui:anonymous` | `read`, always |
| anything else | refused with `401` and `WWW-Authenticate: Basic realm="godwit"` | — |

Every secret is compared in constant time as a SHA-256 digest, and the password is never logged. The UI is protected as soon as the service has tokens **or** the `--ui-user` / `--ui-password` pair; with neither it is open to anyone who reaches the port and `serve` logs `ui enabled without basic auth` — treat that as a development setting.

An anonymous visitor on an open UI gets **`read`**, not `--ui-scope`. `--ui-scope` is the scope of the identity that signed in with `--ui-user`; handing it to someone who signed in with nothing was the same fail-open shape as the anonymous-admin default above, and `GODWIT_UI=true` alone was enough to reach it. Every page still renders, and every button that changes something is gone.

The UI calls the service in process, so its actions appear in `cp_audit` under `ui:<name>` rather than under the token that a browser would have used over HTTP. That in-process path does **not** pass the auth interceptor, so the UI runs the same decision itself: every call goes through `api.Authorize(procedure, principal)`, the identical `procedure → scope` table the interceptor uses. A page therefore renders only the actions the scope allows, and a request posted around the page — `POST /ui/runs/<id>/resume` typed by hand — is refused with `403` and the scope message (`ResumeRun requires scope operator; token ui:viewer has scope read`). Scopes reach the UI as: `read` sees every page and can change nothing; `pipeline` adds confirm rollout and revert; `operator` adds resume, park, check drift and accept baseline, which is everything the UI offers; `admin` adds nothing, because the UI calls no admin RPC.

The one button `read` may press is **Generate migration** on `/ui/diff`, and that is deliberate: `Diff` persists nothing — it applies the pasted DDL to a scratch database that is dropped again, and hands back SQL for a human to commit. It does *execute* that DDL, on the scratch server, as the scratch role; what stops it from being a privilege escalation is [the scratch database](#the-scratch-database) being isolated, not the RPC being read-only. The page is gated by the same `.Can` map as every other action, so if `Diff` ever needs a wider scope the button disappears on its own.

Basic auth sends the password on every request, so the TLS termination below is not optional when the UI is on.

### Cross-site requests

Basic credentials are replayed by the browser on a cross-origin form post, and the UI's actions are plain form posts, so without a check a page anywhere on the web could make a signed-in operator accept a drift baseline, revert a run with `force` and `allow-data-loss`, or run `POST /ui/diff` — which executes the pasted DDL in the control-plane cluster. Every request to `/ui` that is not a `GET` or a `HEAD` must therefore prove it came from the UI's own origin, before authentication and before the handler:

| The request carries | Verdict |
|---|---|
| `Sec-Fetch-Site: same-origin` | allowed — the browser sets this header itself and a page cannot forge it |
| any other `Sec-Fetch-Site` (`cross-site`, `same-site`, `none`) | `403 cross-site request refused` |
| no `Sec-Fetch-Site`, and an `Origin` the UI answers on | allowed — the fallback for a browser without fetch metadata |
| no `Sec-Fetch-Site` and an `Origin` from anywhere else | `403` |
| neither header | `403` — no browser posts a form without at least one of them |

That last row means a scripted `curl -X POST http://…/ui/…` is refused; the API, not the UI, is the programmatic surface. A script that genuinely wants a UI route can send `Origin` matching the host it is posting to, which a hostile page cannot do.

**`--ui-origin`.** With no origins configured the `Origin` fallback is compared with the request's own `Host`, which is right for a direct listener and for a proxy that passes `Host` through, and wrong for one that rewrites it. `--ui-origin https://godwit.example.com` (repeatable, or `GODWIT_UI_ORIGIN` comma-separated) names the origins a browser reaches `/ui` at; it is then the allowlist of accepted `Origin` values **and** of hosts the UI answers on at all — a request for any other `Host` is refused whatever its method, which is what stops DNS rebinding from reaching a listener on a private address. A malformed value fails `serve` at start-up. If a proxy in front rewrites `Host`, either configure it to preserve the browser's, or list the origin it rewrites to as well.

**No CSRF token.** A synchroniser token needs somewhere to keep per-session state, and basic auth has no session: keying one to a per-process secret would break the UI behind a load balancer the moment a form rendered by one replica is posted to another, and deriving it from the master key would make the credential-encryption key a web secret. The origin check is complete against the only vector that exists here — there is no cookie to leak, an attacker's page cannot set `Origin` or `Sec-Fetch-Site`, and a request carrying neither is refused rather than admitted.

### Response headers

Every `/ui` response carries `X-Frame-Options: DENY` and `frame-ancestors 'none'` (an origin check does not stop clickjacking: framing the run page and tricking an operator into pressing *Revert* is a same-origin post), `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` so run ids, plan ids and target names do not leave in the `Referer` to the font and script CDNs, and `Cache-Control: no-store` on pages (`/ui/mark.svg` and `/ui/app.js` set their own).

The `Content-Security-Policy` is `default-src 'none'` with `script-src 'self' https://cdnjs.cloudflare.com`, `style-src 'self' 'unsafe-inline' https://fonts.googleapis.com`, `font-src https://fonts.gstatic.com`, `img-src 'self'`, `connect-src 'self'` (htmx's three-second poll), `form-action 'self'` and `base-uri 'none'`. The pages carry no inline script — the copy button moved to `/ui/app.js`, served from the same `embed.FS` as the templates — so `script-src` needs no `'unsafe-inline'`. `style-src` still does: the backfill bar's width is an inline `style` attribute computed per run, which no nonce or hash can cover. htmx and the fonts are still loaded from a CDN without `integrity`; pinning or vendoring them is open.

## GitHub Action

A job running `command: apply`, `confirm`, `revert` or a real `migrate` holds a `pipeline` token, which by the threat model above is the target's own credential. The Action's guards decide who may make that job run, so they authorise rather than advise. Full mechanics in [CI/CD: who may command an apply](ci-cd.md#who-may-command-an-apply).

**`pull_request_target` never reaches an applying command** (exit 2), and is refused outright for a pull request opened from a fork (exit 1). That event runs in the base repository, with its secrets and a write token, on code the fork controls; anything godwit did under it would be done with credentials the fork's author must not have. Use `pull_request`, which withholds secrets from forks, or command the apply from a comment.

**A comment is not a permission.** `author_association` says how someone relates to the owning organisation — `MEMBER` is every member of the org, including those with no access to this repository. The Action treats `allowed-associations` as a narrowing filter and authorises with `GET /repos/{owner}/{repo}/collaborators/{login}/permission`, requiring `admin` or `write`, for the commander and for the approver. `CONTRIBUTOR`, `NONE`, `FIRST_TIME_CONTRIBUTOR` and `MANNEQUIN` are refused as configuration: anyone who opened a pull request carries one. If the permission call fails the command is refused, never allowed.

**The apply is anchored to the commit that was reviewed.** With `require-approval` (default `true`), `apply` and `confirm` need an `APPROVED` review whose `commit_id` is the head being applied, by someone other than the pull request author. A push after the approval moves the head, the approval no longer matches, and the command is refused. The checked-out-commit check alone cannot do this: the checkout and the API head move together with the push. A review that triggers the apply is checked against its own `commit_id` as well.

What this does **not** protect against, stated so nobody reads more into it:

- **Two people with write permission are enough.** One writes the migration, another approves it. That is the intended trust boundary — the same one that lets them merge — not a residual to be closed by the Action.
- **A fork's SQL still runs on the target when a maintainer commands the apply.** That is the point of reviewing a contribution: the guard is that a human with write permission approved *that exact commit*, not that the code is trusted a priori.
- **`require-approval: "false"` removes the anchor.** The comment path then has no sha of its own; `/godwit apply <sha>` in the comment body restores an anchor a commenter chose deliberately, and is refused when the head has moved past it.
- **Anything the checkout runs inherits the job's environment.** `lint` and `diff` with an ORM `schema_source` execute code from the repository (`go run`, `npx prisma`, `python manage.py`) in a step that carries `GODWIT_TOKEN` and `GH_TOKEN`. Keep those steps on `pull_request` with a `read` token, and install dependencies with lifecycle scripts off (`npm ci --ignore-scripts`).

## Admission limits

Request size, file count, page size and concurrency are bounded, and the knobs are in [configuration](configuration.md#admission-limits). What the limits are for, from a security point of view:

- `--max-request-bytes` (32 MiB) and `--max-file-bytes` (4 MiB) bound the memory one call can make the replica allocate; a submitted directory is held about five times over between the proto message, the file map, the loader's in-memory FS, the migration bodies and the split statements.
- `--max-migrations` (2000) and `--max-files` (5000) bound how much work one call can ask for: a migration is a replay step on a scratch database, and the count of them is what a validating call costs. It is counted in migrations because a file count halves in practice — every migration is an up and a down — which is what made the old file cap refuse a directory the byte caps had plenty of room for.
- `--max-concurrent-diffs` (4) bounds how many calls build scratch databases at once — `Diff`, `PlanRun`, `CreateRun`, `RevertRun` and `Checkpoint`, three of which need only `read`. Each `Diff` creates four to five databases and each `Checkpoint` two, so without a cap a `read` token could exhaust the scratch server's `max_connections` or its disk.
- `ListAudit` and `ListPlans` clamp their `limit` to 1000. It used to be clamped only at the bottom, so a `read` caller could pull the whole audit trail into one response.
- `--max-concurrent-runs` (4) and `--run-timeout` (24h) bound what one run can take from the others on the replica: the scheduler now executes each claimed run on its own goroutine instead of inline in its ticker, and cancels one that outlives the timeout.

## Network

- The listener is plaintext h2c/HTTP/1.1. Terminate TLS in front of it (the Helm Ingress needs an h2c- or gRPC-capable class, or a service mesh); the CLI accepts `https://` URLs.
- `/metrics`, `/healthz` and `/readyz` are unauthenticated. Scope them to the cluster network; `/metrics` label values include target names.
- The service dials out to: the store, every target, the scratch server, Vault, the webhook URL, `slack.com`. Egress rules need those and nothing else.
- Replicas do not talk to each other; the store is the only shared state.
- The container runs as non-root with a read-only root filesystem, no capabilities (chart defaults); the CLI in hook Jobs does the same.

## Supply chain

The binary embeds `libpg_query` through cgo; the Dockerfile builds it from source in the repository at a pinned Go version and ships a distroless image. `ghcr.io/samuelmolling/godwit` is built from `main` by `.github/workflows/publish.yml` with the workflow's own `GITHUB_TOKEN` (no long-lived registry credential) and carries `org.opencontainers.image.source` / `revision` labels; images are not signed yet, so pin `sha-<short commit>` or build and sign your own from the same Dockerfile. Release binaries (`v*` tags, GoReleaser) ship with a `checksums.txt`, which is itself unsigned: it detects corruption, not substitution.

Every action used by `.github/workflows/` is pinned to a full commit sha, not a tag: `release.yml` holds `contents: write` and `HOMEBREW_TAP_TOKEN`, a token with write access to a second repository, so a moved `v2` tag on any action in that job would publish a trojaned release and formula for a binary that holds production database credentials. Pin the same way in your own workflows, and pin `SamuelMolling/godwit` to a commit rather than `@main` ([CI/CD](ci-cd.md#github-action)).
