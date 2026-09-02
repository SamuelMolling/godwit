# API

One [connect](https://connectrpc.com) service, `godwit.v1.GodwitService`, defined in `api/proto/godwit/v1/godwit.proto`. Every RPC is reachable over gRPC, gRPC-Web and connect's JSON protocol on the same port as `/metrics`, `/healthz` and `/readyz` (default `:8474`). The CLI is a connect client; nothing below is CLI-only.

## Calling it with curl

```
POST /godwit.v1.GodwitService/<Method>
Content-Type: application/json
Authorization: Bearer <secret>
```

```bash
export GODWIT=http://localhost:8474
export TOKEN=s3cret-ci
call() { body=${2:-'{}'}; curl -s -X POST "$GODWIT/godwit.v1.GodwitService/$1" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "$body"; }

call ListRuns '{"target":"app"}'
```

Conventions:

- JSON field names are protojson camelCase: `runId`, `acknowledgeHazards`, `allowOutOfOrder`, `lockTimeout`, `createdAt`.
- Enums are strings: `"state": "RUN_STATE_SUCCEEDED"`. Fields at their zero value are omitted from responses (an empty `error`, a `0` attempts, an unset `finishedAt`).
- Timestamps are RFC 3339 strings. Durations (`lockTimeout`, `statementTimeout`) are Go duration strings (`"5s"`, `"2m"`), empty for "target default".
- Errors: HTTP status per connect (400/401/403/404/412/500/501) with body `{"code":"permission_denied","message":"CreateRun requires scope pipeline; token viewer has scope read"}`.
- `WatchRun` is server-streaming; plain curl receives connect's enveloped stream, so use `godwit run watch` or a connect client for it.

The server speaks HTTP/2 cleartext (h2c) and HTTP/1.1; curl over `http://` works without flags.

## Authentication and scopes

`Authorization: Bearer <secret>` where the secret matches an entry of `GODWIT_TOKENS` ([token spec](configuration.md#token-spec)). Without any configured token every caller is `anonymous` with scope `admin`. The token's name is the run's `createdBy` and the audit `actor`.

| Scope | RPCs |
|---|---|
| `read` | `GetRun`, `ListRuns`, `WatchRun`, `PlanRun`, `GetTargetStatus`, `ListDriftEvents`, `ListAudit` |
| `pipeline` | + `CreateRun`, `RevertRun`, `ConfirmRollout` |
| `operator` | + `ResumeRun`, `ParkRun`, `CheckDrift`, `AcceptBaseline`, `BaselineTarget` |
| `admin` | + `RegisterTarget` |

Missing or unknown token: `unauthenticated: invalid or missing bearer token`. Insufficient scope: `permission_denied: <Method> requires scope <x>; token <name> has scope <y>`. Both are counted in `godwit_api_requests_total{code=...}` and logged as `api call` at warn.

## Error codes

| connect code | HTTP | When |
|---|---|---|
| `invalid_argument` | 400 | missing `name`/`target`, no files, unknown provider or rollout, bad duration, unparsable migration files, `version must be positive`, `no migration at or below version N`, `migration failed validation: ...` |
| `unauthenticated` | 401 | bad bearer |
| `permission_denied` | 403 | scope too low |
| `not_found` | 404 | unknown target or run id |
| `failed_precondition` | 412 | `unacknowledged hazards ...`, `out-of-order migrations ...`, `run is not failed or parked`, `run is not awaiting contract`, `run is not the latest on its target or the target is busy`, `baseline runs cannot be reverted`, `target already has applied migrations`, `plan <id> on <target> is stale ...` (detail `godwit.v1.PlanStale`), `target <t> requires a stored plan ...` (detail `godwit.v1.PlanRequired`) |
| `unimplemented` | 501 | `drift detection is not enabled`, `baselining is not enabled`, `target status is not enabled`, `stored plans are not enabled` (server wired without those components; not the case for `godwit serve`) |
| `internal` | 500 | store errors, credential provider errors, `replay history run N: ...` from validation |

## RPCs

Request and response fields are listed as JSON. Fields not mentioned do not exist.

### RegisterTarget — admin

Creates or replaces a target. `provider` is `static` (`dsn` encrypted with the master key), `kubernetes` (`secretPath`: a mounted file containing the DSN) or `vault` (`vaultPath` under `/v1/`, optional `vaultTemplate`, default `{{dsn}}`). `lockTimeout` / `statementTimeout` become the target defaults. `requirePlan` refuses every `CreateRun` on the target that does not bind to a stored plan.

```bash
call RegisterTarget '{"name":"app","provider":"static","dsn":"postgres://app:app@db/app","lockTimeout":"5s","requirePlan":true}'
# {}
call RegisterTarget '{"name":"app","provider":"vault","vaultPath":"secret/data/app/db","vaultTemplate":"postgres://{{user}}:{{password}}@db/app"}'
```

### CreateRun — pipeline

Admits and queues a run. `files` are `{name, body}` pairs named `<version>_<name>.up.sql` / `.down.sql`; both sides of every version are required. Admission, in order: target exists → hazard gate (`acknowledgeHazards`) → out-of-order guard (`allowOutOfOrder`) → scratch validation (`skipValidation`). `rollout` is `direct` (default) or `expand-contract`. `source` is free text stored on the run.

Before admission the set is matched against the plans stored by `PlanRun{persist}` (see [Plans](concepts.md#plans)): a fresh plan is bound (`planId` in the response and on the run), an explained one is re-planned and superseded, a stale one is refused with `failed_precondition` and a `godwit.v1.PlanStale` detail (`planId`, `reason`, `historyAdded`, `historyRemoved`, `schemaDiff`, `hint`); the message carries the same information as text. No matching plan means an implicit plan and an empty `planId`, unless the target or the service requires plans: then `failed_precondition` with a `godwit.v1.PlanRequired` detail (`target`, `key`, `nearestPlanIds`, `filesDiff`).

```bash
call CreateRun '{
  "target":"app",
  "files":[
    {"name":"20260901120000_create_orders.up.sql","body":"CREATE TABLE orders (id bigserial PRIMARY KEY);"},
    {"name":"20260901120000_create_orders.down.sql","body":"DROP TABLE orders;"}
  ],
  "rollout":"expand-contract",
  "source":"github.com/acme/app@1f2e3d4"
}'
# {"runId":"0d3c6c6e-3f9b-4b8a-9c8e-1d1f0c1b2a3c","planId":"7f3a2c1e-..."}
```

Refusals:

```json
{"code":"failed_precondition","message":"unacknowledged hazards (pass acknowledge_hazards to accept):\nH001: CREATE INDEX without CONCURRENTLY blocks writes on orders"}
```

```json
{"code":"failed_precondition",
 "message":"plan 7f3a2c1e on app is stale (planned 2026-09-01T12:00:03Z by ci, github.com/acme/app@9c1e2f)\n  reason : schema\n  schema : + column public.users.age bigint null=NO default=<none>\n           (1 changes not made by any run since the plan)\n  files  : unchanged (key 3e0f1a2b)\nfix: push to the pull request (re-plan) or `godwit drift accept app` if the schema changes are intended",
 "details":[{"type":"godwit.v1.PlanStale","value":"..."}]}
```

### PlanRun — read

Same admission as `CreateRun`, no run. Returns every migration with `applied`, `phase` (`expand` / `contract`, given `rollout`) and its statements with `noTx` and hazards (`code`, `detail`, `recipe`: the safe form as SQL with the statement's real names); `validated` is true when the scratch replay ran. With `persist`, the plan is stored for a later `CreateRun` to bind to, and the response adds `planId`, `planKey`, `observed` (`historyHash`, `schemaFingerprint`, `appliedCount`, `newestApplied`, `at`) and `drift` (schema changes on the target that no run made, as `+`/`-` lines; empty without a baseline). `source` is free text stored with the plan. An applied migration whose body no longer matches its recorded checksum is `invalid_argument: <version>_<name> applied with different content`.

```bash
call PlanRun '{"target":"app","files":[...],"rollout":"expand-contract","persist":true,"source":"github.com/acme/app@9c1e2f"}'
```

```json
{"target":"app","rollout":"expand-contract","validated":true,
 "migrations":[{"version":"20260901120000","name":"create_orders","checksum":"9f...","phase":"expand",
   "statements":[{"sql":"CREATE TABLE orders (id bigserial PRIMARY KEY)"}]}],
 "planId":"7f3a2c1e-...","planKey":"3e0f1a2b...",
 "observed":{"historyHash":"c4...","schemaFingerprint":"a1...","appliedCount":3,"newestApplied":"20260901110000","at":"2026-09-01T12:00:03Z"}}
```

### GetRun — read

```bash
call GetRun '{"runId":"0d3c6c6e-3f9b-4b8a-9c8e-1d1f0c1b2a3c"}'
```

```json
{"run":{"id":"0d3c6c6e-...","target":"app","state":"RUN_STATE_SUCCEEDED","attempts":1,
 "createdAt":"2026-09-02T03:14:15.000Z","finishedAt":"2026-09-02T03:14:18.000Z",
 "rollout":"direct","phase":"expand","kind":"migrate","createdBy":"ci","source":"github.com/acme/app@1f2e3d4"}}
```

`Run` fields: `id`, `target`, `state`, `error`, `attempts`, `createdAt`, `finishedAt`, `rollout`, `phase`, `reverts` (id of the run this one undoes), `lockTimeout`, `statementTimeout`, `kind` (`migrate` / `baseline`), `createdBy`, `source`, `planId` (the stored plan the run bound to; empty for an implicit plan).

### ListRuns — read

`{"target":"app"}` filters; `{}` lists every run. Newest first, capped at 100.

### WatchRun — read

Streams a `Run` snapshot immediately and then every 500ms until the state settles (`succeeded`, `failed`, `needs_attention`, `awaiting_contract`, `reverted`).

### ResumeRun — operator

`{"runId":"..."}`. Requeues a `failed` or `needs_attention` run with `attempts` reset to 0; the next attempt resumes from the journal. Anything else: `run is not failed or parked`.

### ParkRun — operator

`{"runId":"...","reason":"waiting for DBA"}`. Sets `needs_attention` with `reason` as the error, drops the lease, audits `run.park`. It does not interrupt an attempt already executing; use it on `queued` or `failed` runs.

### ConfirmRollout — pipeline

`{"runId":"..."}`. Requeues an `awaiting_contract` run with `phase = contract`. Anything else: `run is not awaiting contract`.

### RevertRun — pipeline

```bash
call RevertRun '{"runId":"0d3c6c6e-...","acknowledgeHazards":["H002"]}'
# {"runId":"7a1b...."}
```

Queues a run whose plans are the down sides of the original's files, newest version first, with `reverts` set. Same admission as `CreateRun` minus the order guard. Original must be `succeeded`, `awaiting_contract`, `failed` or `needs_attention`, be the newest non-reverted run on its target, with nothing `queued`/`running` there; baseline runs are refused.

### CheckDrift — operator

```bash
call CheckDrift '{"target":"app"}'
# {"drifted":true,"diff":"--- snapshot\n+++ target\n..."}   or   {}
```

Fingerprints the target now, records a drift event when it differs from the snapshot, resolves open events when it matches again.

### ListDriftEvents — read

`{"target":"app"}` or `{}`. Returns `events[]` with `id`, `target`, `diff`, `detectedAt`, `resolvedAt`.

### AcceptBaseline — operator

`{"target":"app"}`. Takes a fresh snapshot, resolves open drift events, audits `drift.accept`.

### BaselineTarget — operator

```bash
call BaselineTarget '{"target":"legacy","version":"20260801000000","files":[...]}'
# {"runId":"..."}
```

Records every migration with `version <= version` in the target's `godwit.migrations` without executing it, and stores the files as a `kind = baseline` run so validation replays from here. Refused with `target already has applied migrations` when the target's journal is not empty; `no migration at or below version N` when the files do not reach the version.

### GetTargetStatus — read

```bash
call GetTargetStatus '{"target":"app","files":[...]}'
```

```json
{"target":"app","provider":"static","lockTimeout":"5s",
 "applied":[{"version":"20260901120000","name":"create_orders","checksum":"9f...","appliedAt":"..."}],
 "pending":[{"version":"20260901120500","name":"orders_customer_idx"}],
 "lastRun":{"id":"...","state":"RUN_STATE_SUCCEEDED"},
 "driftBaseline":{"takenAt":"...","runId":"...","unresolvedDrift":false}}
```

`files` is optional; with it, `pending` lists versions in the files not yet applied and `applied[].checksumMismatch` marks versions whose file changed.

### ListAudit — read

`{"target":"app","runId":"","limit":50}`; every field optional, `limit` 0 means 100. Newest first. Entries: `id`, `at`, `actor`, `action` (`target.register`, `target.baseline`, `run.create`, `run.revert`, `run.resume`, `run.park`, `run.confirm`, `drift.accept`), `runId`, `target`, `detail`.

## Generated clients

`gen/godwit/v1` holds the Go types and the connect client (`godwitv1connect.NewGodwitServiceClient(http.DefaultClient, url)`); `buf generate` regenerates them from the proto (`make proto-lint` checks it). Any connect or gRPC toolchain can generate a client for another language from `api/proto`.
