# Configuration

Every knob, generated from `internal/cli`, `internal/config`, `internal/server` and `internal/creds`. If it is not here, the binary does not read it.

## `godwit.yaml`

Project file for the CLI. Looked up from the working directory upward until a directory containing `.git` (that directory is checked too); `--config <path>` names a file explicitly. Unknown keys are an error. `dir` is resolved relative to the file.

| Key | Type | Default | Env override | Used by |
|---|---|---|---|---|
| `dir` | path | `migrations` | `GODWIT_DIR` | `plan`, `apply`, `status`, `down`, `lint`, `migrate`, `target baseline`, `target status` |
| `target` | string | — | `GODWIT_TARGET` | `migrate`, `run confirm --latest` |
| `rollout` | `direct` \| `expand-contract` | `direct` | `GODWIT_ROLLOUT` | `migrate` |
| `server` | URL | — | `GODWIT_SERVER` | every service command (`target`, `migrate`, `revert`, `run`, `runs`, `drift`, `audit`) |
| `lock_timeout` | Go duration | `5s` | `GODWIT_LOCK_TIMEOUT` | `apply`, `status`, `down` only |
| `statement_timeout` | Go duration | `0` (disabled) | `GODWIT_STATEMENT_TIMEOUT` | `apply`, `status`, `down` only |
| `allow_out_of_order` | bool | `false` | `GODWIT_ALLOW_OUT_OF_ORDER` | `migrate` |

Precedence: explicit flag > `GODWIT_*` env > file > default. The file carries no secrets: the token comes from `GODWIT_TOKEN` or `--token`, DSNs from `--dsn` or a credential provider. `lock_timeout` / `statement_timeout` in the file do not reach `migrate` or `revert`; the service uses the target's registered values unless the run passes `--lock-timeout` / `--statement-timeout` explicitly.

```yaml
dir: db/migrations
target: orders
rollout: expand-contract
allow_out_of_order: false
server: http://godwit.godwit.svc:8474
lock_timeout: 5s
statement_timeout: 0
```

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
| `--max-attempts` | `3` | claims a run may take before it is finished as `needs_attention` |
| `--skip-validation` | `false` | disable the scratch-database validation at admission (also disables `validated` in `PlanRun`) |
| `--require-plan` | `false` | refuse every `CreateRun` that does not bind to a stored plan, on every target (targets registered with `--require-plan` refuse on their own) |
| `--plan-ttl` | `720h` | stored plans older than this are ignored at `CreateRun` (treated as no plan) |
| `--plan-retention` | `2160h` | `bound` and `superseded` plans older than this are deleted on the drift ticker (`ready` plans and plans of unfinished runs are kept); the run keeps its `run.create` audit entry, its `plan_id` becomes empty |
| `--log-format` | `GODWIT_LOG_FORMAT` or `json` | `json` or `text` |
| `--log-level` | `GODWIT_LOG_LEVEL` or `info` | `debug`, `info`, `warn` or `error` |

A bad log format or level fails `serve` before anything else starts.

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
| `godwit version` | | prints `godwit <version> (<commit>)` |
| `godwit plan` | `--dir`, `--format text\|markdown\|json` | plans both sides of every migration offline; with `--target` it becomes a service command (below) |
| `godwit lint` | `--dir`, `--ack H001,...`, `--format text\|markdown\|json`, `--base <git ref>` | with `--base`, only migrations added since the ref are checked and files modified since it are `E003`; blocking findings exit 1 |
| `godwit apply` | `--dsn` (required), `--dir`, `--lock-timeout`, `--statement-timeout` | runs the executor directly against a database |
| `godwit status` | `--dsn` (required), `--dir`, timeouts as above | `applied <ts>`, `pending`, `applied <ts> (checksum drift!)` per migration; bootstraps the `godwit` schema |
| `godwit down` | `--dsn` (required), `--dir`, `--version` (required), `--yes` | applies one migration's down side; refuses without `--yes` |

Lint codes: `E001` directory failed to load, `E002` parse error, `E003` migration modified after merge (needs `--base`), `H001`–`H010` unacknowledged hazards on the up side, `W001` no-op down migration (warning, never blocking). Hazard findings carry a `recipe` (the safe SQL, [concepts: hazards](concepts.md#hazards)): indented under the finding in text, a `<details>` block per finding in markdown, a field in JSON.

### Service

| Command | Flags | Scope |
|---|---|---|
| `godwit target add <name>` | `--provider static\|kubernetes\|vault` (required), `--dsn`, `--secret-path`, `--vault-path`, `--vault-template`, `--lock-timeout`, `--statement-timeout`, `--require-plan` | admin |
| `godwit target baseline <name>` | `--dir`, `--version` (required) | operator |
| `godwit target status <name>` | `--dir` (skipped when the directory does not exist, unless set explicitly) | read |
| `godwit plan --target <name>` | `--dir`, `--rollout`, `--ack`, `--skip-validation`, `--allow-out-of-order`, `--source`, `--format text\|markdown\|json` | read; plans against the live target, stores the plan and prints its id, key, observation and drift |
| `godwit plan show <plan-id>` | `--format text\|markdown\|json` | read; statements, hazards and recipes, observation, drift, state, run id, superseded-by |
| `godwit plans` | `--target`, `--limit` | read; newest first |
| `godwit migrate` | `--target`, `--dir`, `--rollout`, `--ack`, `--skip-validation`, `--allow-out-of-order`, `--source`, `--lock-timeout`, `--statement-timeout`, `--dry-run`, `--format text\|markdown\|json` (dry run only), `--plan <plan-id>` | pipeline (`--dry-run`: read) |
| `godwit revert <run-id>` | `--ack`, `--skip-validation`, `--lock-timeout`, `--statement-timeout` | pipeline |
| `godwit run get <run-id>` | | read |
| `godwit run watch <run-id>` | | read; exits 1 on `failed` / `needs_attention` |
| `godwit run resume <run-id>` | | operator |
| `godwit run confirm [run-id]` | `--latest`, `--target`, `--allow-none` | pipeline (`--latest` also lists runs: read) |
| `godwit runs` | `--target` | read |
| `godwit drift check <target>` | | operator |
| `godwit drift accept <target>` | | operator |
| `godwit audit` | `--target`, `--run`, `--limit` | read |

`migrate --plan <id>` binds that plan explicitly: target, rollout and files come from the plan unless `--target`, `--rollout` or `--dir` are given (then they must agree with it); it cannot be combined with `--dry-run`. `migrate` prints `plan <id>: bound` or `no stored plan for this set: implicit plan` before streaming; a `PlanStale` / `PlanRequired` refusal prints the service's message and exits 3. `migrate` and `revert` stream the run and return when it settles: exit 0 on `succeeded` or `awaiting_contract`, 1 on `failed` or `needs_attention` with `run <id> <state>: <error>` on stderr. Files are sent as `<version>_<name>.up.sql` / `.down.sql` bodies; the directory is loaded and validated locally first.

## GitHub Action inputs

See [CI/CD](ci-cd.md#action-inputs-and-outputs); they map one-to-one onto the CLI flags above.

## Helm values

`deploy/helm/godwit/values.yaml` documents every value inline; the `serve` block exposes `port`, `driftInterval`, `skipValidation`, `logFormat`, `logLevel` and `extraArgs`, and every environment variable above comes from the Secret named by `existingSecret` or from `vault.*`, `notifications.*`, `extraEnv` / `extraEnvFrom`. `--lease-ttl`, `--tick-interval` and `--max-attempts` go through `serve.extraArgs`.
