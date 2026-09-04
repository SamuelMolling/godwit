# Operations

What to set up and watch once `godwit serve` runs for real. Flags and variables are defined once in [configuration](configuration.md); this page says why you would change them.

## High availability

Every replica runs the same four loops: API, scheduler, drift monitor, validator. Nothing is elected.

- **Runs** are serialised per target by a lease row in `cp_leases` (one lease per target at a time, `FOR UPDATE SKIP LOCKED` on claim). Any replica claims a `queued` run, or a `running` one whose lease expired; a claim increments `attempts`. Heartbeats renew the lease every `lease-ttl/4`; a beat that fails is retried every `lease-ttl/10` until a fifth of the lease is left, and then the replica cancels the run it can no longer prove it owns rather than run past its lease. A replica that dies mid-run loses its lease after `--lease-ttl`; the next claim resumes from the journal in the target ([concepts: leases](concepts.md#leases)).
- **Drift** is checked by every replica on its own `--drift-interval`; the partial unique index `cp_drift_events_open_idx` keeps duplicate open events out, so two replicas detecting the same diff produce one row.
- **Notifications** are per replica and in-memory (queue of 256 per provider); Slack thread ids live in `cp_notifications`, so any replica continues a thread.
- **Transient failures** (classes `08` connection, `53` insufficient resources — a full disk and an exhausted memory budget included — and `57` operator intervention, plus `40001`, `40P01`, `55P03`, `58000`, `58030`, connection resets, deadline exceeded): the run goes back to `queued` with `not_before = now + backoff` (`--tick-interval` doubled per attempt, capped at 5 minutes, ±20% jitter) and `retries + 1`; nobody is paged, `godwit_run_retries_total` counts it, the run's `error` starts with `transient:`. `57P04` (database dropped), `58P01` and `58P02` (a missing or duplicated file) are the exceptions inside those classes and are permanent, like any other SQL error: the run finishes as `failed` at once with `sql:` in front.
- **Store outage**: `/readyz` fails (it pings the store), the scheduler logs and retries on the next tick, in-flight statements finish or fail on their own timeouts. No run is lost: the journal is in the target, the run row in the store.

Each replica executes up to `--max-concurrent-runs` (4) runs at once, each on its own goroutine: the scheduler's ticker claims work and hands it off, so a run that takes an hour occupies one slot and the replica keeps claiming for every other target. A run that outlives `--run-timeout` (24h) is cancelled and finished as `failed` — raise it above the longest backfill you expect to run in one go, and remember that `statement_timeout: "0"` on a run means *no statement limit*, so this is the only wall clock such a run has.

Defaults that matter: `replicaCount: 2`, a PodDisruptionBudget with `minAvailable: 1`, soft pod anti-affinity, `terminationGracePeriodSeconds: 30`, graceful HTTP shutdown of 5s. A rolling update kills a replica with a run in flight; the run stays `running` until its lease expires (30s by default), then another replica takes it, and `godwit_run_resumes_total{source="reconciler"}` goes up by one. Set `--lease-ttl` above your longest expected GC pause and below how long you tolerate a stalled run; `--max-attempts` (5) bounds how many attempts a run gets, whether they ended in a lost lease or a transient failure, before it is finished as `needs_attention` (`transient: gave up after N attempts`).

## The store

One PostgreSQL database, tables in the default schema of the store role, plus a `godwit` schema because the store migrates itself with the same engine.

| Table | Rows | Grows with |
|---|---|---|
| `cp_targets` | one per target; `config` holds the encrypted DSN or the provider config | targets |
| `cp_runs` | one per run: state, attempts, rollout, phase, `reverts`, timeouts, kind, `created_by`, `source`, error | runs |
| `cp_run_files` | every migration file body sent with a run (the source of the bodies a replay and a revert use, narrowed by `cp_run_applied`) | runs × files; the largest table |
| `cp_run_applied` | one row per migration a run actually applied: order, whether its contract phase is held, the directive expansion frozen for it, and the revert that undid it. This is what a revert, the applied set and the scratch-validation replay are all scoped to | runs × migrations applied |
| `cp_plans` | one per stored plan: key, rollout, state, observation, drift, directive expansions, the run it is bound to | `godwit plan --target` calls; swept by `--plan-retention` |
| `cp_plan_files` | the file bodies of a stored plan | plans × files; second largest |
| `cp_retired_columns` | one per `<c>_old` a completed `change-type` left behind, so `godwit diff` stops proposing to drop it; cleared by the revert or the `drop-column` that removes the column | `change-type` directives |
| `cp_leases` | one per claimed run | runs (never pruned; tiny) |
| `cp_snapshots` | one per target: schema fingerprint and definition after the last successful run or baseline | targets |
| `cp_drift_events` | one per detected diff, `resolved_at` when it goes away or is accepted | drift |
| `cp_notifications` | Slack root message ts per run / drift key | runs |
| `cp_audit` | one per admitted mutation (`target.register`, `target.baseline`, `run.create`, `run.reattach`, `run.revert`, `run.resume`, `run.park`, `run.confirm`, `drift.accept`) | mutations |

Privileges for the store role: owner of the store database, and that is all it needs once `--scratch-dsn` points scratch databases elsewhere. Left unset, it also needs `CREATEDB` — and then submitted DDL executes as the owner of the control-plane database, which [security](security.md#the-scratch-database) explains and `serve` warns about on every start.

The scratch role wants `LOGIN CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS` and `CONNECT` on the database its DSN names. Validation creates `godwit_validate_<id>` there, replays the target's history from `cp_run_files` plus the new files, and drops it `WITH (FORCE)`; `Diff` creates up to five `godwit_diff_<id>` databases per call the same way. Without `CREATEDB` every `CreateRun` fails with `replay history ...` / `create scratch database` and the only way forward is `--skip-validation`, which is not the fix. `serve` refuses to start when the scratch role is a superuser, owns the store database, is a member of `pg_execute_server_program` / `pg_read_server_files` / `pg_write_server_files`, or holds `CREATEROLE` or `REPLICATION`; the message names each finding.

Connections: one pool per replica for the store, `--store-max-conns` wide (20 by default, and it wins over `pool_max_conns` in the DSN), a second pool for the scratch server when `--scratch-dsn` is set, sized `max(4, 2 × --max-concurrent-diffs)` from the concurrency gate that is its only source of demand, one single connection per claimed run, drift check, status inspection or baseline against the target, opened for the operation and closed after it, plus one connection to the scratch database per validation. Size `max_connections` on the **scratch** server for `replicas × (its pool + validations and diffs in flight)` and give it its own disk — a submitted schema decides how much it uses; the store sees only the replica pools. The store pool used to be unsized, which left it at pgx's `max(4, NumCPU)` — different on every node size, and small enough that a burst of `Diff` calls left the scheduler waiting in `Acquire`.

Sizing: the store is small. `cp_run_files` keeps the full text of every file for every run; a repository with 500 migrations of 2 KB sent on every run costs 1 MB per run. Prune it with the retention queries below rather than sending fewer files (the service needs the whole history for validation).

## Backups and PITR

godwit does not back anything up. Before these actions, take a backup or note a PITR restore point on the **target**:

- `godwit run confirm` (the contract phase runs the destructive statements you deferred);
- `godwit revert` (down migrations are typically `DROP`; godwit refuses one that would drop a non-empty table or column unless `--allow-data-loss`);
- `godwit migrate --ack H002,...` (any acknowledged destructive hazard);
- `godwit target baseline` (not destructive to data, but rewrites `godwit.migrations`).

```sql
-- on the target, right before the run
SELECT pg_create_restore_point('before-godwit-' || to_char(now(), 'YYYYMMDDHH24MISS'));
```

Restore the store together with the target when you roll back a target by PITR: a `cp_runs` row that says `succeeded` for a migration the restored target no longer has is exactly the situation [drift](runbook.md#drift-detected) and [checksum mismatch](runbook.md#checksum-mismatch) describe.

## Retention

There is no retention command; run these from cron or a scheduled Job. Never delete a run that still has standing `cp_run_applied` rows: validation replays those rows, reading their bodies out of that run's `cp_run_files`, to rebuild the target's history.

Store:

```sql
-- runs older than 90 days that are settled and hold nothing the target still has
WITH old AS (
  SELECT id FROM cp_runs r
  WHERE finished_at < now() - interval '90 days'
    AND state IN ('failed', 'reverted', 'needs_attention')
    AND NOT EXISTS (
      SELECT 1 FROM cp_run_applied a
      WHERE a.run_id = r.id AND a.reverted_by IS NULL)
)
DELETE FROM cp_run_files WHERE run_id IN (SELECT id FROM old);
-- repeat for cp_leases, cp_notifications (key = run_id::text), then cp_runs itself

-- resolved drift events
DELETE FROM cp_drift_events WHERE resolved_at < now() - interval '180 days';

-- audit trail (keep what your compliance window requires)
DELETE FROM cp_audit WHERE at < now() - interval '365 days';
```

The `NOT EXISTS` is the load-bearing part: a `failed` run that applied three migrations before it stopped still owns their history, so deleting its files takes the replay's bodies with it. Do not delete runs with `kind = 'migrate'` that still have standing rows unless the target has been baselined since: [`BaselineTarget`](concepts.md#baseline) records the baseline run as the new history root, after which older runs are no longer replayed.

Target (`godwit` schema):

```sql
-- statement journal of finished runs; keep godwit.migrations and godwit.repeatables intact
DELETE FROM godwit.journal j USING godwit.runs r
WHERE j.run_id = r.id AND r.state = 'succeeded' AND r.finished_at < now() - interval '90 days';
DELETE FROM godwit.runs WHERE state = 'succeeded' AND finished_at < now() - interval '90 days';
```

Never delete a `running` or `failed` row from `godwit.runs`: the next attempt reopens it to know where to resume.

## Checkpoints

Reach for one when `godwit plan` has become slow and the log line for a plan shows the replay dominating it. The replay executes every migration the target has applied, on a fresh scratch database, before every plan — so its cost grows with the length of the history, not with the size of the change.

**Signals it is time.** A `godwit plan` on a pull request measured in minutes; a directory past a few hundred versioned files; the service logging `plan stored` long after the request came in. If the history is short and slow, a checkpoint will not help — the replay is executing real work, and the checkpoint's body would execute the same amount.

**How much it buys.** Two savings, and they are different sizes. The first is everything the history did and then undid: tables created and dropped, columns added and renamed, indexes rebuilt, `ALTER`s stacked on one table. A history with churn collapses hard — over 1000 churning migrations the scratch replay goes from 10.6 s to 0.14 s, and in the repository's own test a 24-migration churning history replays in about a quarter of the time, executing one migration instead of 24. The second is the per-migration overhead: the replay pays an advisory lock, a bootstrap, a run row and a `finalize` for every migration it executes, and a checkpoint pays them once for the whole collapsed range however additive it was. A purely additive history collapses into a body of roughly the same number of statements and still gains that: 1000 of them replay in 6.1 s whole and 3.2 s from the checkpoint. What it never buys is time the history spent doing real work: if the history is short and slow, the checkpoint's body executes the same work.

**Taking one.**

```
godwit checkpoint --name squash --dry-run   # read it first
godwit checkpoint --name squash
git add db/migrations/*_squash.up.sql && git commit
```

Then open it as an ordinary pull request: `lint` and `plan` run on it like any migration, and the plan says whether each target will run the checkpoint or record it.

**Before you merge it**, check `godwit targets`: every target's newest applied version must be at or above the checkpoint's `through=`, or the collapsed files must still be in the directory (they are, unless you deleted them). A target parked below the checkpoint with the files gone is refused at plan time, by name, and the way out is to restore the files or `godwit target baseline` it.

**After it is merged**, the migrations it collapsed are frozen: they can no longer be reverted on any target ([concepts](concepts.md#checkpoints)), and `godwit revert` says so instead of running their down files. Keep the files in the repository until every target has passed the checkpoint; they are what carries a target that stopped below it.

**Checkpoint or baseline?** A baseline adopts *one target* whose schema godwit did not build, by writing its history without running anything; it is per target, it does not travel, and the directory it takes still has to contain a file describing that schema. A checkpoint changes *the directory*, for every target, and is generated from the files rather than from a database. Baseline when you are adopting an existing database; checkpoint when the replay of a history godwit itself built has got too long.

## Upgrades

1. Read the release notes for new store migrations (`internal/controlplane/schema.go`, `storeMigrations`).
2. Roll the image (`ghcr.io/samuelmolling/godwit`: `main` follows the branch, `sha-<short commit>` pins one build; set `image.tag` in the chart). On start every replica runs `Migrate` on the store under the store's own advisory lock; the first one applies, the others see nothing pending. The log line `store migrated` carries `applied=N`.
3. Store migrations are forward-only in practice; `DownSQL` exists but no command applies it. To roll back a release, restore the store from backup taken before step 2.
4. Old and new replicas share the store during the rollout; keep migrations additive (they are).

Target-side `godwit` schema changes are bootstrapped with `CREATE ... IF NOT EXISTS` on first contact, no explicit step.

## Web UI

`serve --ui` mounts an operator UI at `/ui`. Sign-in and what each scope may press are in [security.md](security.md#web-ui); the pages themselves are read-mostly and every action they offer is an RPC the same token could call.

| Page | What it answers |
|---|---|
| `/ui/` | what is running, what needs a human, every run newest first; `?target=` filters. A running backfill carries its rows and batches under the state pill |
| `/ui/runs/{id}` | one run's timeline from `cp_audit`, the statement it is on, a live *Backfill* block while a batched statement runs, its error, the plan it is bound to, and resume / park / confirm / revert |
| `/ui/targets` | every registered target with its provider, `search_path`, timeouts, `require_plan`, applied count, ready plans, runs waiting for a human and open drift |
| `/ui/targets/{name}` | one target: what its journal has applied (checksum mismatches flagged) and its repeatables, what the newest ready plan still has to apply, the ready plans themselves, the open drift with check and accept, and the registered settings |
| `/ui/plans` | every stored plan newest first, filtered by target (`?target=`) and state (`?state=ready\|bound\|superseded`), with the key prefix, rollout, author, migration count and the run each one is bound to |
| `/ui/plans/{id}` | one plan in full: statements per migration grouped by phase, every hazard with its recipe, `already applied by hand` with the effect it recorded, the directives a migration carried and the expansion they produced, the observation the plan was taken against, and the drift the target had at that moment |
| `/ui/drift` | drift events per target, with check and accept-baseline |
| `/ui/diff` | the desired schema pasted as DDL against a target, answered with the up/down migration and the filenames to save it under; on a target that records repeatable migrations it supplies the `R__` pairs from the newest stored plan, the run that last succeeded, or boxes on the page |

The rail and every target list come from `ListTargets`, so a registered target that was never migrated appears from the moment it is registered. The plan list asks `ListPlans` once per target. A plan that retention has swept renders as *pruned* rather than a `404`: the run keeps the record of what it applied.

Both pages read `Run.progress`, which the executor reports after every committed batch and the scheduler saves at most once a second. Rows written and batches committed are counted; the total is `pg_class.reltuples` for the table taken once when the backfill started, so it is shown as `~n` and the percentage as `≈`, and a run can finish either side of it — the batch only touches rows that still need it. The 3s htmx poll the page already runs is what moves the numbers; a run that is not running shows no backfill block, because `cp_runs.progress` is cleared by every transition that starts or ends an attempt and a settled run therefore carries none.

`/ui/targets/{name}` takes its *pending* set from the target's newest **ready** plan, because the service holds no migration directory of its own; `godwit target status <name> --dir ./migrations` is the comparison against the files on disk, and it is also what flags a checksum mismatch.

## Admission limits

Set in [configuration](configuration.md#admission-limits); this is when to move them.

| Symptom | Raise |
|---|---|
| `invalid_argument: migration file <name> is N bytes, limit 4194304` | `--max-file-bytes`, and check whether the file is a schema dump that belongs in a `schema_source` instead |
| `invalid_argument: too many migration files: N, limit 2000` | `--max-files`; a directory past a thousand migrations is the only legitimate cause |
| `invalid_argument: schema is N bytes, limit 4194304` | `--max-file-bytes`; it bounds the desired schema `Diff` accepts as well |
| the client reports the message as too large before the service answers | `--max-request-bytes`, above the sum of what one run sends |
| `resource_exhausted: too many concurrent validation requests` on pull-request plans | `--max-concurrent-diffs`, and size the scratch server's `max_connections` and disk for it: each admitted call builds four to five databases there |
| a queued run waits while unrelated targets migrate | `--max-concurrent-runs`, and `--store-max-conns` with it |
| a long backfill is finished as `failed` with `context deadline exceeded` | `--run-timeout`, above the backfill's real duration |

`ListAudit` and `ListPlans` clamp `limit` to 1000 with no flag; page through with `--limit` and the newest-first order instead of asking for the whole table.

## Metrics

`GET /metrics`, Prometheus text format, unauthenticated, no access-log line. Every series is in `internal/metrics/metrics.go`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `godwit_build_info` | gauge | `version`, `commit` | always 1 |
| `godwit_runs` | gauge | `target`, `state` | current run count, computed at scrape time |
| `godwit_run_age_seconds` | gauge | `target`, `state` | age of the oldest run in that state |
| `godwit_run_resumes_total` | counter | `target`, `source` | `reconciler` (lease expired) or `manual` (`ResumeRun`) |
| `godwit_run_retries_total` | counter | `target`, `code` | transient failures retried on their own; `code` is the SQLSTATE, `network` or `timeout` |
| `godwit_run_attempts` | histogram | — | attempts a finished run took (buckets 1–5) |
| `godwit_heartbeat_failures_total` | counter | — | lease renewals that failed |
| `godwit_run_duration_seconds` | histogram | `target`, `result` | wall time of an attempt |
| `godwit_statement_duration_seconds` | histogram | `target`, `kind` | per statement, `tx` or `no_tx` |
| `godwit_statement_failures_total` | counter | `target`, `reason` | `lock_timeout`, `statement_timeout`, `sqlstate_<code>`, `error` |
| `godwit_hazards_total` | counter | `code`, `acked` | hazards seen at admission |
| `godwit_validation_failures_total` | counter | `target` | runs refused by the scratch replay |
| `godwit_drift_checks_total` | counter | `target`, `result` | `clean`, `drifted`, `accepted` |
| `godwit_api_requests_total` | counter | `method`, `code` | connect code per RPC |
| `godwit_api_request_duration_seconds` | histogram | `method` | |
| `godwit_notifications_total` | counter | `provider`, `result` | `delivered`, `failed`, `dropped` |

Alert rules to start from:

```yaml
groups:
- name: godwit
  rules:
  - alert: GodwitRunNeedsAttention
    expr: sum by (target) (godwit_runs{state="needs_attention"}) > 0
    for: 1m
    labels: {severity: page}
    annotations: {summary: "godwit run on {{ $labels.target }} gave up; see runbook: needs_attention"}
  - alert: GodwitRunFailed
    expr: sum by (target) (godwit_runs{state="failed"}) > 0
    for: 5m
    labels: {severity: ticket}
  - alert: GodwitRunStuckRunning
    expr: godwit_run_age_seconds{state="running"} > 3600
    for: 5m
    labels: {severity: page}
  - alert: GodwitAwaitingContractTooLong
    expr: godwit_run_age_seconds{state="awaiting_contract"} > 86400
    labels: {severity: ticket}
  - alert: GodwitQueuedNotClaimed
    expr: godwit_run_age_seconds{state="queued"} > 60
    for: 2m
    labels: {severity: page}
    annotations: {summary: "no replica is claiming runs (scheduler down or store unreachable)"}
  - alert: GodwitLockTimeouts
    expr: increase(godwit_statement_failures_total{reason="lock_timeout"}[15m]) > 0
    labels: {severity: ticket}
  - alert: GodwitSchemaDrift
    expr: increase(godwit_drift_checks_total{result="drifted"}[10m]) > 0
    labels: {severity: ticket}
  - alert: GodwitHeartbeatFailures
    expr: increase(godwit_heartbeat_failures_total[10m]) > 0
    labels: {severity: ticket}
  - alert: GodwitNotificationsDropped
    expr: increase(godwit_notifications_total{result=~"dropped|failed"}[10m]) > 0
    labels: {severity: ticket}
  - alert: GodwitApiErrors
    expr: sum(rate(godwit_api_requests_total{code=~"internal|unavailable"}[5m])) > 0
    labels: {severity: ticket}
  - alert: GodwitDown
    expr: absent(godwit_build_info)
    for: 2m
    labels: {severity: page}
```

`GodwitRunFailed` is a ticket, not a page: a `failed` run is a migration that errored on SQL (`sql:` in its error); transient errors never reach `failed`, they retry with backoff and the run says `retrying` in the meantime. The pipeline that created it already failed loudly.

## Notifications

Configured by environment only ([configuration](configuration.md#environment)). Both providers receive the same events:

| `kind` | `type` |
|---|---|
| `run` | `created`, `running`, `retrying`, `succeeded`, `failed`, `needs_attention`, `awaiting_contract`, `confirmed`, `resumed`, `parked`, `reverted` |
| `drift` | `detected`, `resolved`, `accepted` |

**Webhook** (`GODWIT_WEBHOOK_URL`): one `POST` per event, `Content-Type: application/json`, 10s timeout, any status ≥ 300 counts as `failed`. Body:

```json
{
  "kind": "run",
  "type": "failed",
  "target": "orders",
  "run_id": "0d3c6c6e-3f9b-4b8a-9c8e-1d1f0c1b2a3c",
  "state": "failed",
  "attempt": 1,
  "rollout": "expand-contract",
  "phase": "expand",
  "actor": "ci",
  "detail": "statement 2: ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)",
  "at": "2026-09-02T03:14:15Z",
  "text": "godwit run failed on orders (run 0d3c6c6e): statement 2: ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)"
}
```

`text` is the one-line rendering; every other field is the event. Drift events carry `target`, `detail` (the diff or the acceptance) and no `run_id`.

**Slack** (`GODWIT_SLACK_TOKEN` + `GODWIT_SLACK_CHANNEL`): Block Kit messages. `GODWIT_SLACK_MODE=thread` (default) posts one root per run (`created`) and replies in its thread as it progresses, updating the root's state line; drift gets a fresh root per detection under key `drift:<target>`, with `resolved`/`accepted` as replies. `edit` mode keeps a single message per key and rewrites it (`chat.update`). Delivery retries three times on 429 (honouring `Retry-After`), 5xx and network errors with 1s/2s/4s backoff; `detail` is cut at 500 characters. With `GODWIT_PUBLIC_URL` set, every run message has an "Open run" button to `<url>/ui/runs/<id>`. The bot needs `chat:write` in the channel.

Delivery is asynchronous: one worker per provider with a queue of 256 events; a full queue drops the event with `notification dropped` in the log and `result="dropped"` in the metric. Shutdown drains the queues.

## Logging

`--log-format json` (default) or `text`, `--log-level`. Every line carries `replica` (hostname) and `build`. What to grep for:

| Event | Level | Keys |
|---|---|---|
| `store migrated` | info | `applied` |
| `listening` | info | `addr`, `validation` |
| `api call` | info, warn on non-ok | `method`, `actor`, `scope`, `code`, `duration_ms`, `error` |
| `run created`, `revert created`, `run resumed`, `run parked`, `rollout confirmed`, `target registered`, `baseline accepted` | info | `run`, `target`, `actor` |
| `run refused by hazard gate`, `run refused by validation` | warn | `target`, `actor`, `error` |
| `run claimed` | info | `run`, `target`, `attempt`, `rollout`, `phase`, `reverts` |
| `run retrying` | warn | `run`, `target`, `attempt`, `wait`, `error` |
| `run re-attached` | info | `run`, `target`, `state`, `plan`, `resumed` |
| `run finished` | info, error when the run errored | `run`, `target`, `attempt`, `state`, `duration_ms`, `error` |
| `statement applied` / `statement failed` | debug / warn | `run`, `target`, `migration`, `stmt`, `kind`, `duration_ms`, `error` |
| `heartbeat lost` | warn | `run`, `error` |
| `drift checked` | debug clean, info drifted | `target`, `result` |
| `schema drift detected` / `schema drift resolved` | warn / info | `target` |
| `notification dropped` | warn | `provider`, `kind`, `type` |
| `audit write failed` | error | `action`, `error` |

Never logged: DSNs, tokens, master key, migration SQL text. `/metrics`, `/healthz` and `/readyz` produce no access-log line.

## Probes

`GET /healthz` returns 200 once the process listens. `GET /readyz` pings the store and returns 503 while it is unreachable; the Helm chart wires both. Kubernetes routing away from a replica whose store ping fails is what you want: its scheduler cannot claim anyway.
