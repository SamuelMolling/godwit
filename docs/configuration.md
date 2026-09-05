# Configuration

Every knob, generated from `internal/cli`, `internal/config`, `internal/server` and `internal/creds`. If it is not here, the binary does not read it.

## `godwit.yaml`

Project file for the CLI. Looked up from the working directory upward until a directory containing `.git` (that directory is checked too); `--config <path>` names a file explicitly. Unknown keys are an error. `dir` is resolved relative to the file.

| Key | Type | Default | Env override | Used by |
|---|---|---|---|---|
| `dir` | path | `migrations` | `GODWIT_DIR` | `plan`, `apply`, `status`, `down`, `lint`, `migrate`, `diff`, `target baseline`, `target status` |
| `target` | string | — | `GODWIT_TARGET` | `plan`, `plans`, `lint`, `diff`, `migrate`, `revert`, `run confirm --latest` |
| `rollout` | `direct` \| `expand-contract` | `direct` | `GODWIT_ROLLOUT` | `plan`, `migrate` |
| `server` | URL | — | `GODWIT_SERVER` | every service command (`target`, `targets`, `plan`, `plans`, `lint`, `diff`, `migrate`, `revert`, `run`, `runs`, `drift`, `audit`) |
| `lock_timeout` | Go duration | `5s` | `GODWIT_LOCK_TIMEOUT` | `apply`, `status`, `down` only |
| `statement_timeout` | Go duration | `0` (disabled) | `GODWIT_STATEMENT_TIMEOUT` | `apply`, `status`, `down` only |
| `allow_out_of_order` | bool | `false` | `GODWIT_ALLOW_OUT_OF_ORDER` | `plan`, `migrate` |
| `schema_source` | block ([below](#schema_source)) | — | `GODWIT_SCHEMA_SOURCE_KIND`, `_PATH`, `_BIN` | `diff`, `lint` |

Precedence: explicit flag > `GODWIT_*` env > file > default. The file carries no secrets: the token comes from `GODWIT_TOKEN` or `--token`, DSNs from `--dsn` or a credential provider. `lock_timeout` / `statement_timeout` in the file do not reach `migrate` or `revert`; the service uses the target's registered values unless the run passes `--lock-timeout` / `--statement-timeout` explicitly.

`target` reaches `plan` as well as `migrate`, and `plan --target` is a service command: once `godwit.yaml` names a target, a bare `godwit plan` no longer parses the directory offline — it plans against the live target and stores a plan on the service. `godwit plan --target ""` forces the offline form back.

```yaml
dir: db/migrations
target: orders
rollout: expand-contract
allow_out_of_order: false
server: http://godwit.godwit.svc:8474
lock_timeout: 5s
statement_timeout: 0
```

### `schema_source`

Says how the desired schema of *this* directory is rendered to DDL, so `godwit diff` needs no source flag. `godwit.yaml` is per directory, so a monorepo puts one next to each migration directory and each keeps its own source; `path` is resolved relative to the file that declares it.

```yaml
dir: db/migrations
target: orders
schema_source:
  kind: prisma                              # file | prisma | gorm | django | alembic | rails | drizzle | command
  path: prisma/schema.prisma
  bin: npx prisma                           # kind-specific, optional
  command: ["go", "run", "./cmd/schema"]    # kind: command only
  lint: true                                # default true
```

| Key | Type | Default | Env override | Meaning |
|---|---|---|---|---|
| `kind` | `file` \| `prisma` \| `gorm` \| `django` \| `alembic` \| `rails` \| `drizzle` \| `command` | — (required) | `GODWIT_SCHEMA_SOURCE_KIND` | which source renders the schema; any other value is a config error naming the set |
| `path` | path | — | `GODWIT_SCHEMA_SOURCE_PATH` | the DDL file, `schema.prisma`, Go package, `manage.py`, `alembic.ini`, Rails application root or `drizzle.config.ts`, resolved relative to `godwit.yaml` |
| `bin` | string | kind's default | `GODWIT_SCHEMA_SOURCE_BIN` | command line running the ORM's CLI; `file` and `rails` run nothing and ignore it |
| `command` | list of strings | — | — | `kind: command` only: the argv whose stdout is the DDL |
| `lint` | bool | `true` | — | whether an out-of-date generated migration is `E005` (error) rather than a warning |

`godwit diff` renders every kind: `file`, `command` and `rails` need no toolchain, `prisma`, `gorm`, `django`, `alembic` and `drizzle` run the project's own ([concepts](concepts.md#generating-migrations-from-a-schema) has the command each one runs and what it refuses). A source flag always wins over the block: `--schema`, `--prisma`, `--exec`, `--gorm`, `--django`, `--alembic`, `--rails` and `--drizzle` select the source and are mutually exclusive, at most one per diff; `--prisma-bin`, `--go-bin`, `--python-bin`, `--alembic-bin` and `--drizzle-bin` given on the command line override `schema_source.bin` (which itself overrides `GODWIT_PRISMA_BIN` / `GODWIT_GO_BIN` / `GODWIT_PYTHON_BIN` / `GODWIT_ALEMBIC_BIN` / `GODWIT_DRIZZLE_BIN`). `kind: django` also reads `DJANGO_SETTINGS_MODULE` from the environment when `manage.py` does not set one, and `--django-database` names the `DATABASES` alias. `kind: rails` takes the application root and reads its `db/structure.sql`; a `db/schema.rb` is refused, since rendering a Ruby DSL needs ActiveRecord and a database.

`godwit lint` reads the same block: with `--server` and `--target` it renders the source and asks the service what the committed migrations still owe it, reporting `E005` when they no longer match ([concepts: schema sources](concepts.md#schema-sources)). Without a server it reports `W002` and does not run the ORM at all; `--no-schema-check` turns the check off even when the block is present.

## Client environment

| Variable | Flag | Meaning |
|---|---|---|
| `GODWIT_SERVER` | `--server` | service base URL. `http://` is dialled as cleartext HTTP/2 (h2c); `https://` is dialled over TLS against the system root store, negotiating HTTP/2 and falling back to HTTP/1.1, which carries every RPC |
| `GODWIT_TOKEN` | `--token` | bearer token sent as `Authorization: Bearer <secret>` |
| `GODWIT_DSN` | `--dsn` | target DSN for the local commands (`plan`, `apply`, `status`, `down`), so the password need not be a process argument |
| `GODWIT_TARGET_DSN` | `target add --dsn` | DSN of a `static` target being registered, for the same reason |

Every service command also accepts `--json` (print the raw protojson response instead of the human line).

## `godwit serve`

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | `:8474` | address for the API, `/metrics`, `/healthz` and `/readyz` |
| `--store-dsn` | `GODWIT_STORE_DSN`, required | control-plane PostgreSQL DSN. Prefer the environment variable: an argument is visible in `docker inspect`, `DescribeTaskDefinition`, `kubectl get pod -o yaml` and `/proc/<pid>/cmdline` |
| `--scratch-dsn` | `GODWIT_SCRATCH_DSN` | PostgreSQL validation and diff create their throwaway databases on. Unset runs them on the store server with the store's own credentials, which is what submitted DDL then executes as; `serve` warns on every start and [security](security.md#the-scratch-database) says why that is bad. Set, the role is inspected at start-up and `serve` refuses to run when it is a superuser, owns the store database, is a member of `pg_execute_server_program` / `pg_read_server_files` / `pg_write_server_files`, or holds `CREATEROLE` or `REPLICATION` |
| `--scratch-template` | `GODWIT_SCRATCH_TEMPLATE` or `template0` | database scratch databases are cloned from. `template0` carries nothing an operator installed into `template1`; name a prepared template to give validation the extensions a migration needs |
| `--drift-interval` | `5m` | how often every snapshotted target is fingerprinted |
| `--holder` | `GODWIT_HOLDER`, or the hostname | name this replica goes by in `cp_leases.holder`, every log line's `replica` and the UI. godwit appends `/<16 hex characters>` drawn once per start, whatever the name: the identity is compared whole wherever two replicas must be kept off one run, so replicas that share a hostname still hold separate leases. There is no way to pin the whole identity, and that is the point |
| `--lease-ttl` | `30s` | how long a claimed run stays leased without a heartbeat; beats go out every quarter of it and a failed beat retries every tenth, and past a fifth of the lease the replica gives the run up |
| `--tick-interval` | `2s` | how often the scheduler looks for runnable runs |
| `--max-attempts` | `5` | attempts a run may take (lost leases and transient failures alike) before it is finished as `needs_attention` |
| `--max-concurrent-runs` | `4` | runs this replica executes at once; a slow run holds one slot, never the ticker |
| `--run-timeout` | `24h` | wall clock one run may take; past it the run is cancelled and finished as `failed` |
| `--shutdown-timeout` | `20s` | budget for the whole shutdown after `SIGINT` or `SIGTERM`: draining the listener, then the runs this replica already claimed. A run that does not finish inside it is cut and left to its lease. Keep it under the platform's kill delay |
| `--store-max-conns` | `20` | size of the pool against the store; wins over `pool_max_conns` in `--store-dsn` |
| `--max-request-bytes` | `33554432` (32 MiB) | largest request body the API decodes; over it the transport refuses before any handler runs |
| `--max-migrations` | `2000` | migrations one `CreateRun`, `PlanRun`, `RevertRun`, `Diff` or `Checkpoint` may carry; the `.up.sql` half is what names one |
| `--max-files` | `5000` | files one such request may carry, migration halves and everything else alike |
| `--max-file-bytes` | `4194304` (4 MiB) | largest single migration body, and the largest desired schema `Diff` accepts |
| `--max-concurrent-diffs` | `4` | `Diff`, `PlanRun`, `CreateRun`, `RevertRun` and `Checkpoint` calls admitted at once; each builds scratch databases on `--scratch-dsn`, or on the store server when it is unset |
| `--skip-validation` | `false` | disable the scratch-database validation at admission (also disables `validated` in `PlanRun`) |
| `--require-plan` | `false` | refuse every `CreateRun` that does not bind to a stored plan, on every target (targets registered with `--require-plan` refuse on their own) |
| `--plan-ttl` | `720h` | stored plans older than this are ignored at `CreateRun` (treated as no plan) |
| `--plan-retention` | `2160h` | `bound` and `superseded` plans older than this are deleted on the drift ticker (`ready` plans and plans of unfinished runs are kept); the run keeps its `run.create` audit entry, its `plan_id` becomes empty |
| `--log-format` | `GODWIT_LOG_FORMAT` or `json` | `json` or `text` |
| `--log-level` | `GODWIT_LOG_LEVEL` or `info` | `debug`, `info`, `warn` or `error` |
| `--ui` | `false` | serve the operator web UI at `/ui` on the same listener (also `GODWIT_UI=true`) |
| `--ui-user` | `GODWIT_UI_USER` | basic auth user for a shared `/ui` identity; needs `--ui-password` |
| `--ui-password` | `GODWIT_UI_PASSWORD` | basic auth password for that shared identity |
| `--ui-scope` | `GODWIT_UI_SCOPE` or `operator` | what the shared `--ui-user` identity may do: `read`, `pipeline`, `operator` or `admin`; an anonymous visitor on an open UI is always `read` |
| `--ui-origin` | `GODWIT_UI_ORIGIN` (comma-separated) | repeatable `scheme://host[:port]` origins a browser reaches `/ui` at, e.g. `https://godwit.example.com`; the allowlist of origins a form post may come from and of hosts the UI answers on. Empty compares the browser's `Origin` with the request's `Host`, which needs the proxy in front to preserve it |

A bad log format or level, an unknown `--ui-scope`, a malformed `--ui-origin`, or a UI user without a password (or the reverse), fails `serve` before anything else starts. A `--scratch-dsn` that does not parse, cannot be reached, or names a role that can act outside its own scratch databases fails it right after the store migration.

### Admission limits

Everything above the `--max-*` line is refused with `invalid_argument` naming the limit, except a body over `--max-request-bytes`, which the connect transport refuses before the request is decoded.

A directory is counted in migrations, not in files, because a migration is two files and the count that matters is the one that costs a replay step. The three defaults stop at the same directory rather than one stopping short of the others: 2000 migrations is 4000 files, inside `--max-files` with room for checkpoints and strays, and 32 MiB at the 8 KiB-a-file shape `--max-request-bytes` was sized for. The load rig's 1000-migration target and its checkpoint fit the defaults with the same margin again.

Raise `--max-file-bytes` for a generated schema dump. Raise `--max-migrations` and `--max-request-bytes` together for a directory past two thousand migrations — the byte cap is the one that bites first on bodies larger than 8 KiB, and `--max-files` only needs to move if the directory holds files that are not migration halves.

`--max-concurrent-diffs` is a queue, not a hard refusal: a call waits 30 seconds for a free slot and is then refused with `resource_exhausted`. Each admitted call creates four to five databases on whatever `--scratch-dsn` points at — the store server itself when it is unset — so this is the number to size that server's `max_connections` and disk against. The pool that creates and drops them is sized from this flag (`max(4, 2 × --max-concurrent-diffs)`) and needs no knob of its own. `Checkpoint` is in the queue too: it needs only `read` and builds two databases per call. The UI calls the service in process and does not pass through the queue.

`--max-concurrent-runs` and `--run-timeout` are the scheduler's side: the replica claims up to `--max-concurrent-runs` runs and executes each on its own goroutine, so a backfill with `batch=1 pause=1h` occupies one slot instead of the whole replica; `--run-timeout` is the wall clock past which such a run is cancelled and recorded as `failed`. Raise it above the longest backfill you expect to run in one go.

`/ui` also accepts the secret of any [bearer token](#token-spec) as the basic-auth password, whatever username is typed; that signs in as `ui:<token name>` with the token's own scope. The UI is protected as soon as tokens or the user/password pair exist; with neither it serves open, logs `ui enabled without basic auth`, audits as `ui:anonymous` and carries scope **`read`** — not `--ui-scope`, which applies only to the identity that signed in. Pages offer only the actions the scope allows and a request beyond it is refused with `403`. Every form post must also come from the UI's own origin, so a `POST /ui/…` from a script — with no `Origin` and no `Sec-Fetch-Site` — is refused with `403 cross-site request refused`; see [security](security.md#cross-site-requests).

### Environment

| Variable | Required | Meaning |
|---|---|---|
| `GODWIT_MASTER_KEY` | for `static` targets | 64 hex characters (32 bytes); AES-256-GCM key sealing the DSNs of `static` targets. Unset, `serve` starts anyway and only `static` targets are unusable ([security](security.md#the-key-and-where-it-comes-from)) |
| `GODWIT_MASTER_KEY_PREVIOUS` | no | comma-separated keys in the same form, accepted for decryption only; how an `env` key rotation rolls without downtime |
| `GODWIT_KEY_PROVIDER` | no | where the key lives: `env` (default), `gcpkms` or `vault-transit`. The KMS providers seal a per-value data key, so the DSN never leaves the process |
| `GODWIT_KMS_KEY` | with a KMS provider | the Cloud KMS `projects/…/cryptoKeys/…` resource name, or the Vault Transit key name |
| `GODWIT_KMS_ENDPOINT` | no | Cloud KMS base URL; defaults to `https://cloudkms.googleapis.com` |
| `GOOGLE_OAUTH_ACCESS_TOKEN` | no | a fixed access token for `gcpkms`; unset, the token comes from the GCE metadata server (Workload Identity) |
| `GCE_METADATA_HOST` | no | where that metadata server is; defaults to `metadata.google.internal` |
| `GODWIT_VAULT_TRANSIT_MOUNT` | no | Transit mount for `vault-transit`; defaults to `transit` |
| `GODWIT_TOKENS` | no | comma-separated bearer token specs ([below](#token-spec)); unset means every caller is `anonymous` with scope `admin`, and `serve` logs `no tokens configured` at warn level |
| `GODWIT_SCRATCH_DSN` | no | default for `--scratch-dsn` |
| `GODWIT_SCRATCH_TEMPLATE` | no | default for `--scratch-template` |
| `GODWIT_WEBHOOK_URL` | no | POST every run and drift event here as JSON |
| `GODWIT_SLACK_TOKEN` | no | Slack bot token; enables the Slack provider |
| `GODWIT_SLACK_CHANNEL` | with the token | channel id or name for the root messages; `serve` refuses to start with a token and no channel |
| `GODWIT_SLACK_MODE` | no | `thread` (default; root message plus threaded replies) or `edit` (one message rewritten) |
| `GODWIT_PUBLIC_URL` | no | base URL for the "Open run" button in Slack messages (`<url>/ui/runs/<id>`) |
| `GODWIT_HOLDER` | no | default for `--holder` |
| `GODWIT_LOG_FORMAT` | no | default for `--log-format` |
| `GODWIT_LOG_LEVEL` | no | default for `--log-level` |
| `GODWIT_UI` | no | `true` enables the web UI like `--ui` |
| `GODWIT_UI_USER` | no | default for `--ui-user` |
| `GODWIT_UI_PASSWORD` | no | default for `--ui-password` |
| `GODWIT_UI_SCOPE` | no | default for `--ui-scope` |
| `GODWIT_UI_ORIGIN` | no | comma-separated default for `--ui-origin` |
| `VAULT_ADDR` | for `vault` targets | Vault base URL; the provider fails with `vault provider not configured: set VAULT_ADDR` otherwise |
| `VAULT_TOKEN` | no | static Vault token; when unset the Kubernetes auth method is used |
| `VAULT_K8S_ROLE` | without `VAULT_TOKEN` | role for `POST auth/<mount>/login` |
| `VAULT_K8S_MOUNT` | no | auth mount, default `kubernetes` |
| `VAULT_K8S_JWT` | no | service-account token file, default `/var/run/secrets/kubernetes.io/serviceaccount/token` |

The replica's lease holder name is the hostname; there is no flag for it.

## Token spec

`GODWIT_TOKENS` is a comma-separated list; each entry is one of:

| Form | Name | Scope |
|---|---|---|
| `name:scope:secret` | `name` | `scope` |
| `secret` | `anonymous` | `admin` |

**A two-field spec is refused.** `name:secret` used to mean an admin token whose secret was the second field, so `GODWIT_TOKENS=deploy:pipeline` parsed as an *admin* token with the secret `pipeline` rather than the pipeline token it reads as. `serve` now refuses it, naming the first field only, and the fix is to write the scope out: `deploy:pipeline:<secret>`. [Decision 0011](decisions/0011-token-spec-is-three-fields.md) has the reasoning.

Rules, enforced at start-up: exactly one or three colon-separated fields; name and secret must be non-empty; scope must be one of `read`, `pipeline`, `operator`, `admin`; the same secret under two names is refused, naming both. A secret may contain `:` in the three-field form (everything after the second colon is the secret); a bare secret may not. Scopes are cumulative:

| Scope | Allows |
|---|---|
| `read` | `GetRun`, `ListRuns`, `WatchRun`, `PlanRun`, `GetPlan`, `ListPlans`, `GetTargetStatus`, `ListDriftEvents`, `ListAudit` |
| `pipeline` | read + `CreateRun`, `RevertRun`, `ConfirmRollout` |
| `operator` | pipeline + `ResumeRun`, `ParkRun`, `CheckDrift`, `AcceptBaseline`, `BaselineTarget` |
| `admin` | operator + `RegisterTarget` |

A procedure missing from the table is denied to everyone. With tokens configured, a missing or unknown bearer is `unauthenticated`; a known one below the required scope is `permission_denied: <Method> requires scope X; token <name> has scope Y`.

## CLI reference

Global: `--config <path>`. Every service command: `--server`, `--token`, `--json`. Exit code is 0 on success and 1 on any error (refusal, failed run, connection error); `lint` exits 1 on blocking findings; `migrate` exits 3 when the service refuses the run because its stored plan is stale or a plan is required.

### Local (no service)

| Command | Flags | Notes |
|---|---|---|
| `godwit version` | | prints `<version> (<commit>)`; `dev (none)` from a plain `go build`, `main (<full commit>)` in the published image |
| `godwit plan` | `--dir`, `--format text\|markdown\|json` | plans both sides of every migration offline; with `--target` it becomes a service command (below) |
| `godwit lint` | `--dir`, `--ack H001,...`, `--format text\|markdown\|json`, `--base <git ref>`, `--server`, `--token`, `--target`, `--no-schema-check` | with `--base`, only migrations added since the ref are checked and versioned files modified since it are `E003` (never an `R__` file); with `--server` and `--target` it also checks the committed migrations against `schema_source` (`E005`, below); blocking findings exit 1 |
| `godwit apply` | `--dsn` (required), `--dir`, `--lock-timeout`, `--statement-timeout` | runs the executor directly against a database; on a database with no history a checkpoint in the directory runs and the versions it collapses are recorded ([concepts](concepts.md#checkpoints)) |
| `godwit status` | `--dsn` (required), `--dir`, timeouts as above | `applied <ts>`, `pending`, `applied <ts> (checksum drift!)` per versioned migration, `unchanged since <ts>` or `pending` per repeatable; bootstraps the `godwit` schema |
| `godwit down` | `--dsn` (required), `--dir`, `--version` (required), `--yes` | applies one versioned migration's down side; refuses without `--yes`; repeatables have no version and are reverted with the run that applied them; a checkpoint has no inverse and is refused by name |

Lint codes: `E001` directory failed to load, `E002` parse error, `E003` migration modified after merge (needs `--base`), `E004` malformed or misplaced `-- godwit:` directive, including an `assert` whose query is not a single read-only `SELECT` of one column ([concepts: directives](concepts.md#directives), [assertions](concepts.md#assertions)), `E005` the migration generated from the declared `schema_source` is out of date ([concepts: schema sources](concepts.md#schema-sources)), `H001`–`H010` unacknowledged hazards on the up side, `W001` no-op down migration and `W002` schema source not checked because no server was given (warnings, never blocking). Hazard findings carry a `recipe` (the safe SQL, [concepts: hazards](concepts.md#hazards)): indented under the finding in text, a `<details>` block per finding in markdown, a field in JSON.

### Service

| Command | Flags | Scope |
|---|---|---|
| `godwit target add <name>` | `--provider static\|kubernetes\|vault` (required), `--dsn`, `--secret-path`, `--vault-path`, `--vault-template`, `--lock-timeout`, `--statement-timeout`, `--require-plan`, `--keep-old`, `--search-path` | admin |
| `godwit target baseline <name>` | `--dir`, `--version` (required) | operator |
| `godwit target reconcile <name>` | `--dir` | operator |
| `godwit target status <name>` | `--dir` (skipped when the directory does not exist, unless set explicitly) | read |
| `godwit targets` | | read; every registered target with its settings, applied count, ready plans, open drift and last run, without connecting to any of them. The applied count is versioned migrations only; `target status` also lists the repeatables, so its `applied (N)` is the larger number |
| `godwit migrations` | `--target` (repeatable), `--from`, `--to`, `--not-everywhere`, `--in`, `--not-in` | read; one row per migration and content with a column per target, so a version standing on two targets under different checksums is two rows and reads `differs`. `--in staging --not-in production` is what is ahead in staging ([concepts](concepts.md#the-fleet-view)) |
| `godwit plan --target <name>` | `--dir`, `--rollout`, `--ack`, `--skip-validation`, `--allow-out-of-order`, `--to <version>`, `--source`, `--format text\|markdown\|json` | read; plans against the live target, stores the plan and prints its id, key, observation and drift |
| `godwit plan show <plan-id>` | `--format text\|markdown\|json` | read; statements, hazards and recipes, observation, drift, state, run id, superseded-by |
| `godwit plans` | `--target`, `--limit` | read; newest first |
| `godwit checkpoint` | `--name <snake_case>` (required), `--dir` (read and written to), `--at <version>`, `--dry-run` | read; collapses the versioned migrations at or below `--at` (the newest by default) into `<timestamp>_<name>.up.sql`, a file carrying the schema they produce and no down side ([concepts](concepts.md#checkpoints)). The service replays them on a scratch database and refuses unless the generated body reproduces the same schema fingerprint; `--dry-run` prints it without writing |
| `godwit diff` | `--target`, one of `--schema <file>`, `--prisma <schema.prisma>`, `--exec '<command line>'`, `--gorm <package>`, `--django <manage.py>`, `--alembic <alembic.ini>`, `--rails <app root>`, `--drizzle <drizzle.config.ts>` (none falls back to [`schema_source`](#schema_source)), `--prisma-bin` (`$GODWIT_PRISMA_BIN`, default `npx prisma`), `--go-bin` (`$GODWIT_GO_BIN`, default `go`), `--python-bin` (`$GODWIT_PYTHON_BIN`, default `python`), `--alembic-bin` (`$GODWIT_ALEMBIC_BIN`, default `alembic`), `--drizzle-bin` (`$GODWIT_DRIZZLE_BIN`, default `npx drizzle-kit`), `--django-database <alias>`, `--name <snake_case>` (required unless `--dry-run`), `--dir` (written to, and its `R__` migrations are sent so the diff leaves what they declare alone), `--dry-run` | read; writes `<timestamp>_<name>.up.sql` / `.down.sql` from the live target to the desired schema: a DDL file, a Prisma schema, any command's stdout, a GORM dry-run package, a Django project, an Alembic history, a Rails `db/structure.sql` or a Drizzle schema; prints the up SQL with hazards and recipes, `no changes` when they match, `--dry-run` prints without writing |
| `godwit migrate` | `--target`, `--dir`, `--rollout`, `--ack`, `--skip-validation`, `--allow-out-of-order`, `--to <version>`, `--source`, `--lock-timeout`, `--statement-timeout`, `--dry-run`, `--format text\|markdown\|json` (dry run only), `--plan <plan-id>` | pipeline (`--dry-run`: read); `--to` stops at a version and reports the migrations above it as withheld ([concepts](concepts.md#version-targets)); it has no `godwit.yaml` key, needs `--target`, and is refused with `--plan` |
| `godwit revert [run-id]` | `--target`, `--dry-run`, `--force`, `--allow-data-loss`, `--ack`, `--skip-validation`, `--lock-timeout`, `--statement-timeout` | pipeline; undoes the migrations that run applied, newest first, never the rest of the directory it submitted. With no run id, the newest un-reverted run on `--target`; an older run needs `--force`; a plan that drops a non-empty table or column needs `--allow-data-loss`. The plan is printed before anything runs, and `--dry-run` prints it and queues nothing ([runbook](runbook.md#reverting-a-run)) |
| `godwit run get <run-id>` | | read |
| `godwit run watch <run-id>` | | read; exits 1 on `failed` / `needs_attention` |
| `godwit run resume <run-id>` | | operator |
| `godwit run confirm [run-id]` | `--latest`, `--target`, `--allow-none`, `--no-wait` | pipeline (`--latest` also lists runs: read); streams the contract phase and exits with it, 1 on `failed` / `needs_attention`. `--no-wait` returns as soon as the phase is queued |
| `godwit runs` | `--target` | read |
| `godwit drift check <target>` | | operator |
| `godwit drift accept <target>` | | operator |
| `godwit audit` | `--target`, `--run`, `--limit` | read |

### Target settings

Registered with the target and stored in `cp_targets.config`; they are not `godwit.yaml` keys.

| Setting | Flag | Type | Default | Meaning |
|---|---|---|---|---|
| `lock_timeout` | `--lock-timeout` | Go duration | `5s` | per-statement `lock_timeout`; a run may override it with `--lock-timeout` |
| `statement_timeout` | `--statement-timeout` | Go duration | `0` (disabled) | per-statement `statement_timeout`; a run may override it with `--statement-timeout` |
| `require_plan` | `--require-plan` | bool | `false` | refuse runs whose migration set has no stored plan |
| `keep_old` | `--keep-old` | bool | `true` | `-- godwit: change-type` on this target keeps the pre-swap column as the rollback; a directive's own `keep-old=` still wins |
| `search_path` | `--search-path` | comma-separated schema names | — | `search_path` for every session godwit opens on the target ([concepts](concepts.md#search_path)); unquoted identifiers only, `$user` and `godwit` refused, no per-run override |

A setting the target does not carry is printed as `none` by `godwit target status` and `godwit targets`; it means "nothing registered", not "no limit". An unregistered `lock_timeout` still runs under the executor's own 5s default, and an unregistered `statement_timeout` is genuinely disabled.

`godwit target status <name>` prints the provider and three of them — `lock_timeout`, `statement_timeout` and `search_path`. `require_plan` and `keep_old` are in `godwit targets` and in `ListTargets`.

`migrate --plan <id>` binds that plan explicitly: target, rollout and files come from the plan unless `--target`, `--rollout` or `--dir` are given (then they must agree with it); it cannot be combined with `--dry-run`. `migrate` prints `plan <id>: bound`, `no stored plan for this set: implicit plan` or `re-attached to run <id>` (a re-run of a job whose files already bound a plan follows that run instead of queueing another) before streaming; a run waiting out a transient failure shows `(retry in Ns)` on its line; a `PlanStale` / `PlanRequired` refusal prints the service's message and exits 3. `revert` prints the plan it is about to run — the down statements per migration and anything the plan would destroy — before it streams; `--dry-run` prints that and stops. `migrate` and `revert` stream the run and return when it settles: exit 0 on `succeeded` or `awaiting_contract`, 1 on `failed` or `needs_attention` with `run <id> <state>: <error>` on stderr. Files are sent as `<version>_<name>.up.sql` / `.down.sql` or `R__<name>.up.sql` / `.down.sql` bodies; the directory is loaded and validated locally first.

## GitHub Action inputs

See [CI/CD](ci-cd.md#action-inputs-and-outputs); they map one-to-one onto the CLI flags above.

## Helm values

`deploy/helm/godwit/values.yaml` documents every value inline; the `serve` block exposes `port`, `driftInterval`, `skipValidation`, `scratch.enabled`, `scratch.template`, `logFormat`, `logLevel`, `ui.enabled`, `ui.basicAuth`, `ui.scope` and `extraArgs`, and every environment variable above comes from the Secret named by `existingSecret` or from `vault.*`, `notifications.*`, `extraEnv` / `extraEnvFrom`. `serve.scratch.enabled` wires `GODWIT_SCRATCH_DSN` from `existingSecret.keys.scratchDSN`; off, the chart ships the unisolated fallback and the pods warn about it. `existingSecret.keys.masterKey` is **empty by default**, so no `GODWIT_MASTER_KEY` reaches the pod until a deployment with `static` targets names the Secret key holding it; `serve.keyProvider` swaps that for `gcpkms` or `vault-transit`. `--lease-ttl`, `--tick-interval`, `--max-attempts`, `--require-plan`, `--plan-ttl` and `--plan-retention` have no value of their own and go through `serve.extraArgs`. Standing the chart up on ArgoCD, and where the credentials in it come from, is [deployment](deployment.md).
