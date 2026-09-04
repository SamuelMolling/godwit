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
| `GODWIT_SERVER` | `--server` | service base URL (`http://` or `https://`); the client speaks HTTP/2 over cleartext (h2c) or TLS |
| `GODWIT_TOKEN` | `--token` | bearer token sent as `Authorization: Bearer <secret>` |

Every service command also accepts `--json` (print the raw protojson response instead of the human line).

## `godwit serve`

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | `:8474` | address for the API, `/metrics`, `/healthz` and `/readyz` |
| `--store-dsn` | required | control-plane PostgreSQL DSN |
| `--drift-interval` | `5m` | how often every snapshotted target is fingerprinted |
| `--lease-ttl` | `30s` | how long a claimed run stays leased without a heartbeat; heartbeats run every third of it |
| `--tick-interval` | `2s` | how often the scheduler looks for runnable runs |
| `--max-attempts` | `5` | attempts a run may take (lost leases and transient failures alike) before it is finished as `needs_attention` |
| `--skip-validation` | `false` | disable the scratch-database validation at admission (also disables `validated` in `PlanRun`) |
| `--require-plan` | `false` | refuse every `CreateRun` that does not bind to a stored plan, on every target (targets registered with `--require-plan` refuse on their own) |
| `--plan-ttl` | `720h` | stored plans older than this are ignored at `CreateRun` (treated as no plan) |
| `--plan-retention` | `2160h` | `bound` and `superseded` plans older than this are deleted on the drift ticker (`ready` plans and plans of unfinished runs are kept); the run keeps its `run.create` audit entry, its `plan_id` becomes empty |
| `--log-format` | `GODWIT_LOG_FORMAT` or `json` | `json` or `text` |
| `--log-level` | `GODWIT_LOG_LEVEL` or `info` | `debug`, `info`, `warn` or `error` |
| `--ui` | `false` | serve the operator web UI at `/ui` on the same listener (also `GODWIT_UI=true`) |
| `--ui-user` | `GODWIT_UI_USER` | basic auth user for a shared `/ui` identity; needs `--ui-password` |
| `--ui-password` | `GODWIT_UI_PASSWORD` | basic auth password for that shared identity |
| `--ui-scope` | `GODWIT_UI_SCOPE` or `operator` | what the shared identity (and the anonymous one, when the UI is open) may do: `read`, `pipeline`, `operator` or `admin` |

A bad log format or level, an unknown `--ui-scope`, or a UI user without a password (or the reverse), fails `serve` before anything else starts.

`/ui` also accepts the secret of any [bearer token](#token-spec) as the basic-auth password, whatever username is typed; that signs in as `ui:<token name>` with the token's own scope. The UI is protected as soon as tokens or the user/password pair exist; with neither it serves open, logs `ui enabled without basic auth` and audits as `ui:anonymous`. Pages offer only the actions the scope allows and a request beyond it is refused with `403`; see [security](security.md#web-ui).

### Environment

| Variable | Required | Meaning |
|---|---|---|
| `GODWIT_MASTER_KEY` | yes | 64 hex characters (32 bytes); AES-256-GCM key for DSNs of `static` targets |
| `GODWIT_TOKENS` | no | comma-separated bearer token specs ([below](#token-spec)); unset means every caller is `anonymous` with scope `admin` |
| `GODWIT_WEBHOOK_URL` | no | POST every run and drift event here as JSON |
| `GODWIT_SLACK_TOKEN` | no | Slack bot token; enables the Slack provider |
| `GODWIT_SLACK_CHANNEL` | with the token | channel id or name for the root messages; `serve` refuses to start with a token and no channel |
| `GODWIT_SLACK_MODE` | no | `thread` (default; root message plus threaded replies) or `edit` (one message rewritten) |
| `GODWIT_PUBLIC_URL` | no | base URL for the "Open run" button in Slack messages (`<url>/ui/runs/<id>`) |
| `GODWIT_LOG_FORMAT` | no | default for `--log-format` |
| `GODWIT_LOG_LEVEL` | no | default for `--log-level` |
| `GODWIT_UI` | no | `true` enables the web UI like `--ui` |
| `GODWIT_UI_USER` | no | default for `--ui-user` |
| `GODWIT_UI_PASSWORD` | no | default for `--ui-password` |
| `GODWIT_UI_SCOPE` | no | default for `--ui-scope` |
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
| `name:secret` | `name` | `admin` |
| `secret` | `anonymous` | `admin` |

Rules, enforced at start-up: name and secret must be non-empty; scope must be one of `read`, `pipeline`, `operator`, `admin` (a two-field spec whose secret contains `:` is parsed as three fields and fails on the unknown scope); the same secret under two names is refused, naming both. Scopes are cumulative:

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
| `godwit apply` | `--dsn` (required), `--dir`, `--lock-timeout`, `--statement-timeout` | runs the executor directly against a database |
| `godwit status` | `--dsn` (required), `--dir`, timeouts as above | `applied <ts>`, `pending`, `applied <ts> (checksum drift!)` per versioned migration, `unchanged since <ts>` or `pending` per repeatable; bootstraps the `godwit` schema |
| `godwit down` | `--dsn` (required), `--dir`, `--version` (required), `--yes` | applies one versioned migration's down side; refuses without `--yes`; repeatables have no version and are reverted with the run that applied them |

Lint codes: `E001` directory failed to load, `E002` parse error, `E003` migration modified after merge (needs `--base`), `E004` malformed or misplaced `-- godwit:` directive ([concepts: directives](concepts.md#directives)), `E005` the migration generated from the declared `schema_source` is out of date ([concepts: schema sources](concepts.md#schema-sources)), `H001`–`H010` unacknowledged hazards on the up side, `W001` no-op down migration and `W002` schema source not checked because no server was given (warnings, never blocking). Hazard findings carry a `recipe` (the safe SQL, [concepts: hazards](concepts.md#hazards)): indented under the finding in text, a `<details>` block per finding in markdown, a field in JSON.

### Service

| Command | Flags | Scope |
|---|---|---|
| `godwit target add <name>` | `--provider static\|kubernetes\|vault` (required), `--dsn`, `--secret-path`, `--vault-path`, `--vault-template`, `--lock-timeout`, `--statement-timeout`, `--require-plan`, `--keep-old`, `--search-path` | admin |
| `godwit target baseline <name>` | `--dir`, `--version` (required) | operator |
| `godwit target status <name>` | `--dir` (skipped when the directory does not exist, unless set explicitly) | read |
| `godwit targets` | | read; every registered target with its settings, applied count, ready plans, open drift and last run, without connecting to any of them. The applied count is versioned migrations only; `target status` also lists the repeatables, so its `applied (N)` is the larger number |
| `godwit plan --target <name>` | `--dir`, `--rollout`, `--ack`, `--skip-validation`, `--allow-out-of-order`, `--to <version>`, `--source`, `--format text\|markdown\|json` | read; plans against the live target, stores the plan and prints its id, key, observation and drift |
| `godwit plan show <plan-id>` | `--format text\|markdown\|json` | read; statements, hazards and recipes, observation, drift, state, run id, superseded-by |
| `godwit plans` | `--target`, `--limit` | read; newest first |
| `godwit diff` | `--target`, one of `--schema <file>`, `--prisma <schema.prisma>`, `--exec '<command line>'`, `--gorm <package>`, `--django <manage.py>`, `--alembic <alembic.ini>`, `--rails <app root>`, `--drizzle <drizzle.config.ts>` (none falls back to [`schema_source`](#schema_source)), `--prisma-bin` (`$GODWIT_PRISMA_BIN`, default `npx prisma`), `--go-bin` (`$GODWIT_GO_BIN`, default `go`), `--python-bin` (`$GODWIT_PYTHON_BIN`, default `python`), `--alembic-bin` (`$GODWIT_ALEMBIC_BIN`, default `alembic`), `--drizzle-bin` (`$GODWIT_DRIZZLE_BIN`, default `npx drizzle-kit`), `--django-database <alias>`, `--name <snake_case>` (required unless `--dry-run`), `--dir` (written to, and its `R__` migrations are sent so the diff leaves what they declare alone), `--dry-run` | read; writes `<timestamp>_<name>.up.sql` / `.down.sql` from the live target to the desired schema: a DDL file, a Prisma schema, any command's stdout, a GORM dry-run package, a Django project, an Alembic history, a Rails `db/structure.sql` or a Drizzle schema; prints the up SQL with hazards and recipes, `no changes` when they match, `--dry-run` prints without writing |
| `godwit migrate` | `--target`, `--dir`, `--rollout`, `--ack`, `--skip-validation`, `--allow-out-of-order`, `--to <version>`, `--source`, `--lock-timeout`, `--statement-timeout`, `--dry-run`, `--format text\|markdown\|json` (dry run only), `--plan <plan-id>` | pipeline (`--dry-run`: read); `--to` stops at a version and reports the migrations above it as withheld ([concepts](concepts.md#version-targets)); it has no `godwit.yaml` key, needs `--target`, and is refused with `--plan` |
| `godwit revert [run-id]` | `--target`, `--dry-run`, `--force`, `--allow-data-loss`, `--ack`, `--skip-validation`, `--lock-timeout`, `--statement-timeout` | pipeline; undoes the migrations that run applied, newest first, never the rest of the directory it submitted. With no run id, the newest un-reverted run on `--target`; an older run needs `--force`; a plan that drops a non-empty table or column needs `--allow-data-loss`. The plan is printed before anything runs, and `--dry-run` prints it and queues nothing ([runbook](runbook.md#reverting-a-run)) |
| `godwit run get <run-id>` | | read |
| `godwit run watch <run-id>` | | read; exits 1 on `failed` / `needs_attention` |
| `godwit run resume <run-id>` | | operator |
| `godwit run confirm [run-id]` | `--latest`, `--target`, `--allow-none` | pipeline (`--latest` also lists runs: read) |
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

`deploy/helm/godwit/values.yaml` documents every value inline; the `serve` block exposes `port`, `driftInterval`, `skipValidation`, `logFormat`, `logLevel`, `ui.enabled`, `ui.basicAuth`, `ui.scope` and `extraArgs`, and every environment variable above comes from the Secret named by `existingSecret` or from `vault.*`, `notifications.*`, `extraEnv` / `extraEnvFrom`. `--lease-ttl`, `--tick-interval` and `--max-attempts` go through `serve.extraArgs`.
