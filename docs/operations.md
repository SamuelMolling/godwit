# Operations

What to set up and watch once `godwit serve` runs for real. Flags and variables are defined once in [configuration](configuration.md); this page says why you would change them.

## High availability

Every replica runs the same four loops: API, scheduler, drift monitor, validator. Nothing is elected.

- **Runs** are serialised per target by a lease row in `cp_leases` (one lease per target at a time, `FOR UPDATE SKIP LOCKED` on claim). Any replica claims a `queued` run, or a `running` one whose lease expired; a claim increments `attempts`. Heartbeats renew the lease every `lease-ttl/3`. A replica that dies mid-run loses its lease after `--lease-ttl`; the next claim resumes from the journal in the target ([concepts: leases](concepts.md#leases)).
- **Drift** is checked by every replica on its own `--drift-interval`; the partial unique index `cp_drift_events_open_idx` keeps duplicate open events out, so two replicas detecting the same diff produce one row.
- **Notifications** are per replica and in-memory (queue of 256 per provider); Slack thread ids live in `cp_notifications`, so any replica continues a thread.
- **Transient failures** (`SQLSTATE 40001`, `40P01`, `55P03`, `57014`, `53300`, `57P01`–`57P03`, class `08`, connection resets, deadline exceeded): the run goes back to `queued` with `not_before = now + backoff` (`--tick-interval` doubled per attempt, capped at 5 minutes, ±20% jitter) and `retries + 1`; nobody is paged, `godwit_run_retries_total` counts it, the run's `error` starts with `transient:`. Any other SQL error finishes the run as `failed` at once with `sql:` in front.
- **Store outage**: `/readyz` fails (it pings the store), the scheduler logs and retries on the next tick, in-flight statements finish or fail on their own timeouts. No run is lost: the journal is in the target, the run row in the store.

Defaults that matter: `replicaCount: 2`, a PodDisruptionBudget with `minAvailable: 1`, soft pod anti-affinity, `terminationGracePeriodSeconds: 30`, graceful HTTP shutdown of 5s. A rolling update kills a replica with a run in flight; the run stays `running` until its lease expires (30s by default), then another replica takes it, and `godwit_run_resumes_total{source="reconciler"}` goes up by one. Set `--lease-ttl` above your longest expected GC pause and below how long you tolerate a stalled run; `--max-attempts` (5) bounds how many attempts a run gets, whether they ended in a lost lease or a transient failure, before it is finished as `needs_attention` (`transient: gave up after N attempts`).

## The store

One PostgreSQL database, tables in the default schema of the store role, plus a `godwit` schema because the store migrates itself with the same engine.

| Table | Rows | Grows with |
|---|---|---|
| `cp_targets` | one per target; `config` holds the encrypted DSN or the provider config | targets |
| `cp_runs` | one per run: state, attempts, rollout, phase, `reverts`, timeouts, kind, `created_by`, `source`, error | runs |
| `cp_run_files` | every migration file body sent with a run (replayed for validation and used for revert) | runs × files; the largest table |
| `cp_leases` | one per claimed run | runs (never pruned; tiny) |
| `cp_snapshots` | one per target: schema fingerprint and definition after the last successful run or baseline | targets |
| `cp_drift_events` | one per detected diff, `resolved_at` when it goes away or is accepted | drift |
| `cp_notifications` | Slack root message ts per run / drift key | runs |
| `cp_audit` | one per admitted mutation (`target.register`, `target.baseline`, `run.create`, `run.reattach`, `run.revert`, `run.resume`, `run.park`, `run.confirm`, `drift.accept`) | mutations |

Privileges for the store role:

- owner of the store database (it creates and alters its own tables);
- `CREATEDB`, because validation creates `godwit_validate_<run>` on the same server, replays the target's history from `cp_run_files` plus the new files, and drops it `WITH (FORCE)`. Without `CREATEDB`, every `CreateRun` fails with `replay history ...` / `create scratch database` and the only way forward is `--skip-validation`, which is not the fix.

Connections: one pool per replica for the store (pgx defaults), one single connection per claimed run, drift check, status inspection or baseline against the target, opened for the operation and closed after it, plus one connection to the scratch database per validation. Size `max_connections` on the store for `replicas × (pool + validations in flight)`; targets see one session per run plus one per drift check.

Sizing: the store is small. `cp_run_files` keeps the full text of every file for every run; a repository with 500 migrations of 2 KB sent on every run costs 1 MB per run. Prune it with the retention queries below rather than sending fewer files (the service needs the whole history for validation).

## Backups and PITR

godwit does not back anything up. Before these actions, take a backup or note a PITR restore point on the **target**:

- `godwit run confirm` (the contract phase runs the destructive statements you deferred);
- `godwit revert <run-id>` (down migrations are typically `DROP`);
- `godwit migrate --ack H002,...` (any acknowledged destructive hazard);
- `godwit target baseline` (not destructive to data, but rewrites `godwit.migrations`).

```sql
-- on the target, right before the run
SELECT pg_create_restore_point('before-godwit-' || to_char(now(), 'YYYYMMDDHH24MISS'));
```

Restore the store together with the target when you roll back a target by PITR: a `cp_runs` row that says `succeeded` for a migration the restored target no longer has is exactly the situation [drift](runbook.md#drift-detected) and [checksum mismatch](runbook.md#checksum-mismatch) describe.

## Retention

There is no retention command; run these from cron or a scheduled Job. Keep at least one `succeeded` run per target, because validation replays `cp_run_files` of every succeeded run to rebuild the target's history.

Store:

```sql
-- runs older than 90 days that are settled, and their files, leases and audit rows
WITH old AS (
  SELECT id FROM cp_runs
  WHERE finished_at < now() - interval '90 days'
    AND state IN ('failed', 'reverted', 'needs_attention')
)
DELETE FROM cp_run_files WHERE run_id IN (SELECT id FROM old);
-- repeat for cp_leases, cp_notifications (key = run_id::text), then cp_runs itself

-- resolved drift events
DELETE FROM cp_drift_events WHERE resolved_at < now() - interval '180 days';

-- audit trail (keep what your compliance window requires)
DELETE FROM cp_audit WHERE at < now() - interval '365 days';
```

Do not delete `succeeded` runs with `kind = 'migrate'` unless the target has been baselined since: [`BaselineTarget`](concepts.md#baseline) records the baseline run as the new history root, after which older runs are no longer replayed.

Target (`godwit` schema):

```sql
-- statement journal of finished runs; keep godwit.migrations and godwit.repeatables intact
DELETE FROM godwit.journal j USING godwit.runs r
WHERE j.run_id = r.id AND r.state = 'succeeded' AND r.finished_at < now() - interval '90 days';
DELETE FROM godwit.runs WHERE state = 'succeeded' AND finished_at < now() - interval '90 days';
```

Never delete a `running` or `failed` row from `godwit.runs`: the next attempt reopens it to know where to resume.

## Upgrades

1. Read the release notes for new store migrations (`internal/controlplane/schema.go`, `storeMigrations`).
2. Roll the image (`ghcr.io/samuelmolling/godwit`: `main` follows the branch, `sha-<short commit>` pins one build; set `image.tag` in the chart). On start every replica runs `Migrate` on the store under the store's own advisory lock; the first one applies, the others see nothing pending. The log line `store migrated` carries `applied=N`.
3. Store migrations are forward-only in practice; `DownSQL` exists but no command applies it. To roll back a release, restore the store from backup taken before step 2.
4. Old and new replicas share the store during the rollout; keep migrations additive (they are).

Target-side `godwit` schema changes are bootstrapped with `CREATE ... IF NOT EXISTS` on first contact, no explicit step.

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
