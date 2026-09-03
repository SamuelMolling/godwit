# Runbook

One section per symptom. Each gives where to look (store tables in the store database, `godwit.*` tables in the target), what it means, and the command that fixes it. `psql` variables: `:'run'` is the run id from the alert, `:'target'` the target name.

Two ids per run: the store's `cp_runs.id` (what the CLI and alerts show) and the engine's `godwit.runs.id` in the target (one per migration version and direction). They are unrelated; join them by time and version.

## Run in `needs_attention`

**Meaning.** Either the scheduler gave up (`error = 'transient: gave up after N attempts: ...'`: the run used its `--max-attempts` (5) attempts, each one lost its lease or hit a transient error such as a lock timeout, deadlock or lost connection, and the backoff between them did not help) or an operator called `ParkRun`. Nothing runs against the target; the lease is gone.

```sql
SELECT id, target, state, attempts, error, rollout, phase, created_by, source, created_at, finished_at
FROM cp_runs WHERE id = :'run';

SELECT at, actor, action, detail FROM cp_audit WHERE run_id = :'run' ORDER BY at;
```

Then in the target, what actually got applied:

```sql
SELECT r.version, r.direction, r.state, r.stmt_count, r.error, r.started_at, r.finished_at,
       count(*) FILTER (WHERE j.state = 'done')   AS done,
       count(*) FILTER (WHERE j.state = 'intent') AS intents
FROM godwit.runs r LEFT JOIN godwit.journal j ON j.run_id = r.id
WHERE r.started_at > now() - interval '1 day'
GROUP BY r.id ORDER BY r.started_at DESC;
```

**Why the attempts keep dying.** Look at `run finished` / `heartbeat lost` lines for the run in the logs and at `godwit_heartbeat_failures_total`. Usual causes: pods restarted on a rolling update while a long `no-tx` statement ran (raise `--lease-ttl` or wait for a quiet window), or the store was unreachable for longer than the TTL.

**Action.**

```bash
godwit run resume <run-id>        # operator scope; back to queued, attempts = 0
godwit run watch <run-id>
```

Resume is safe: the next attempt reopens the same `godwit.runs` row, skips every `done` statement, verifies any pending `intent` and continues. If the run must not continue, leave it parked and create a [revert](#reverting-a-run) instead; a parked run blocks nothing (only `queued` and `running` runs hold a target).

## Run `failed`

**Meaning.** A statement returned a genuine SQL error (`error` starts with `sql:`). Transient errors never land here: they retry on their own with backoff (`retrying` notification, `godwit_run_retries_total`) and only park as `needs_attention` when `--max-attempts` is exhausted. The error is in `cp_runs.error` and in `godwit.runs.error`. Everything before the failing statement is committed (`tx` statements commit one by one).

```sql
SELECT error FROM cp_runs WHERE id = :'run';
-- which statement, in the target
SELECT r.version, r.direction, r.error, max(j.stmt_idx) FILTER (WHERE j.state = 'done') AS last_done
FROM godwit.runs r LEFT JOIN godwit.journal j ON j.run_id = r.id
WHERE r.state = 'failed' GROUP BY r.id;
```

**Action.** Fix the cause in the database (missing privilege, conflicting object, data that violates the new constraint) and `godwit run resume <run-id>`: it resumes at `last_done + 1` with the same SQL. If the SQL itself is wrong, do not edit the file and resume: `loadProgress` refuses a run whose journal hashes no longer match the plan (`statement N changed since run ... started; refusing to resume`). Revert the run, fix the migration in a new version, migrate again.

## Run stuck in `awaiting_contract`

**Meaning.** An `expand-contract` run applied every migration before the first one carrying a contract hazard (`DROP TABLE`, `DROP COLUMN`, `RENAME`: H002, H003, H008) and waits for `ConfirmRollout` to run the rest. Nothing is wrong; the target is usable by both the old and the new application version. It does not block a new `migrate` on the target (only `queued`/`running` runs do), but a newer run makes this one non-revertable.

```sql
SELECT id, target, created_by, source, created_at, now() - created_at AS waiting
FROM cp_runs WHERE state = 'awaiting_contract' ORDER BY created_at;
```

**Action.** Once the application that no longer reads the old columns is fully rolled out (ArgoCD PostSync does this automatically):

```bash
godwit run confirm <run-id>                        # pipeline scope
godwit run confirm --latest --target <target>      # or by target
```

Take a [restore point](operations.md#backups-and-pitr) first. If the deploy was rolled back instead, `godwit revert <run-id>` runs the down side of every migration in the run.

### A pull request stuck in `awaiting_contract`

**Symptom.** `/godwit apply` succeeded, the `## godwit apply` comment says **awaiting_contract**, and the `godwit/applied` status is `pending` ("expand applied; comment `/godwit confirm` to run the contract phase"), so branch protection will not let the pull request merge. Nothing is broken: the expand phase is on the database, the contract phase is not, and the status is telling the truth.

**Action.** Once the application version that reads both shapes is out, comment `/godwit confirm` on the pull request. It releases the same run (not a new one), the status turns `success` and the pull request becomes mergeable ([CI/CD](ci-cd.md#pull-request-confirm-the-contract-phase)).

Three ways it does not work, and what to do instead:

- **The head moved after the apply.** The `confirm` step refuses (`checked-out commit … is not the head of pull request #N`), and a new `/godwit apply` is refused too (`target <t> has run <id> awaiting contract; confirm or revert it first`). Release the run from the CLI — `godwit run confirm --latest --target <target>` — then re-plan on the new head, or `/godwit revert` and start over.
- **Nothing is awaiting.** `no run of pull request #N is awaiting its contract phase`: the run was already confirmed, or it belongs to another pull request. `godwit runs --target <target>` shows who owns it; the status of this pull request is left untouched.
- **The contract phase failed.** The status goes `failure` and the run is `failed` or `needs_attention` with its error in the comment. That is the [`failed`](#run-failed) case: fix the cause and `godwit run resume <run-id>`, or `godwit revert <run-id>`. Do not re-apply from the pull request; the run is still the one that owns the migration.

## Lock timeouts

**Symptom.** `godwit_statement_failures_total{reason="lock_timeout"}` and `godwit_run_retries_total{code="55P03"}` rise, runs show `transient: ... canceling statement due to lock timeout (SQLSTATE 55P03) (retry in 4s)` and go back to `queued`.

**Meaning.** The statement waited longer than the run's `lock_timeout` (the target's registered default, 5s unless set, or the value passed to `migrate`) for a lock held by application sessions. Failing fast is the point: a DDL waiting on a lock queues every later query behind it.

```sql
-- in the target, who holds what
SELECT pid, state, wait_event_type, now() - xact_start AS xact_age, left(query, 80)
FROM pg_stat_activity
WHERE datname = current_database() AND state <> 'idle' ORDER BY xact_start;

SELECT l.pid, l.mode, l.granted, c.relname
FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
WHERE NOT l.granted OR l.mode LIKE '%Exclusive%';
```

**Action.** End or wait out the long transaction (`idle in transaction` sessions are the usual holder); the run retries by itself (backoff doubles per attempt from `--tick-interval`, capped at 5 minutes) and only needs `godwit run resume <run-id>` once it parked as `needs_attention`. If contention is structural, run the migration with a longer wait in a quiet window: `godwit migrate --lock-timeout 30s` for that run, or `godwit target add` again with a different `--lock-timeout` to change the default. A `statement_timeout` failure (`SQLSTATE 57014`) is the same procedure with the other knob.

## Replica lost mid-run

**Symptom.** A pod disappeared; the run shows `running` with an `updated_at` that stopped moving; `GodwitRunStuckRunning` may fire.

```sql
SELECT r.id, r.target, r.state, r.attempts, l.holder, l.expires_at, l.expires_at < now() AS expired
FROM cp_runs r LEFT JOIN cp_leases l ON l.run_id = r.id
WHERE r.state = 'running';
```

**Meaning.** Until `expires_at`, the dead replica still owns the run; after it, any replica claims it on its next tick (2s default) and resumes from the journal. `godwit_run_resumes_total{source="reconciler"}` counts this. In the target, a `no-tx` statement interrupted between `intent` and `done` is verified before being re-run (index validity, existence), so a half-built `CREATE INDEX CONCURRENTLY` is dropped and recreated, a finished one is journaled as done.

**Action.** None, unless the lease never expires (clock skew between store and replicas: `expires_at` is computed with the store's `now()`; heartbeats use it too) or the run flips to `needs_attention` after `--max-attempts` claims, in which case see [needs_attention](#run-in-needs_attention).

Two replicas never execute the same run: the lease is exclusive per target in the store, and the engine takes `pg_advisory_lock` in the target for the duration of every attempt, so a zombie attempt still holding the lock blocks the newcomer instead of racing it.

## Validation refused the run

**Symptom.** `godwit migrate` exits 1 with `migration failed validation: ...` (InvalidArgument) or `replay history run <i>: ...` (Internal); `godwit_validation_failures_total` rises; log line `run refused by validation`.

**Meaning.** Before queueing, the service creates `godwit_validate_<run>` on the store server, replays the files of every succeeded run for the target (from `cp_run_files`, in order) and then the new files. `migration failed validation` means the new SQL errors on a schema that looks like the target; `replay history` means the recorded history itself does not replay (a migration that depended on data, an extension missing on the store server, a run applied with `--skip-validation` that a later file contradicts).

```sql
-- the history that is replayed
SELECT r.id, r.created_at, count(f.*) AS files
FROM cp_runs r JOIN cp_run_files f ON f.run_id = r.id
WHERE r.target = :'target' AND r.state = 'succeeded' AND r.kind = 'migrate'
GROUP BY r.id ORDER BY r.created_at;
```

**Action.** For `migration failed validation`: fix the migration. For `replay history`: install the missing extension on the store server, or, when the history is legitimately unreplayable, baseline the target (`godwit target baseline <target> --version <newest applied>` needs an empty `godwit.migrations`, see [baseline](concepts.md#baseline)) so replay starts from the baseline run. `--skip-validation` on a single run is the escape hatch; it leaves no trace in the audit trail, so note it in the pull request.

Scratch databases are dropped after every validation; a leftover after a crash is harmless:

```sql
SELECT datname FROM pg_database WHERE datname LIKE 'godwit_validate_%';
DROP DATABASE godwit_validate_xxx WITH (FORCE);
```

## Drift detected

**Symptom.** `schema drift detected` in the log, `godwit_drift_checks_total{result="drifted"}`, a `drift detected` notification, `GodwitSchemaDrift`.

**Meaning.** The target's schema fingerprint (columns, constraints, indexes and views outside `pg_catalog`/`information_schema`) differs from the snapshot taken after the last successful run or baseline. Someone changed the schema outside godwit, or a run was reverted by PITR.

```sql
SELECT id, detected_at, resolved_at, diff FROM cp_drift_events
WHERE target = :'target' ORDER BY detected_at DESC LIMIT 5;

SELECT taken_at, run_id FROM cp_snapshots WHERE target = :'target';
```

```bash
godwit drift check <target>        # operator; re-checks now, prints the diff
```

**Action.** Either revert the manual change in the target (the next check logs `schema drift resolved` and sets `resolved_at`) or make it the new expectation:

```bash
godwit drift accept <target>       # operator; new snapshot, open events resolved, audit drift.accept
```

Accepting does not create a migration; the next `migrate` still replays history without the manual change, so put the change into a migration file too or validation will not see it.

## Checksum mismatch

**Symptom.** `godwit status --dsn ...` prints `applied <ts> (checksum drift!)`; a run fails with `version N already applied with different content`; `target status` marks the version.

**Meaning.** The file `<N>_<name>.up.sql` in the repository no longer hashes to what `godwit.migrations.checksum` recorded when it was applied. Someone edited a merged migration. `lint --base` flags this in the pull request as `E003`.

```sql
SELECT version, name, checksum, applied_at FROM godwit.migrations WHERE version = :version;
```

**Action.** Restore the file to its applied content (`git log -p -- migrations/<N>_*`) and add a new migration for the intended change. If the applied content is gone for good and the edit is cosmetic, update the recorded checksum by hand, once, knowing validation replays the file as it is now:

```sql
UPDATE godwit.migrations SET checksum = :'new_checksum' WHERE version = :version;
```

The checksum is the SHA-256 hex of the up file body: `sha256sum migrations/<N>_<name>.up.sql`.

## Run refused at admission

| Message | Code | Cause | Fix |
|---|---|---|---|
| `target "x": not found` | not_found | not registered | `godwit target add` (admin) |
| `unacknowledged hazards (...)` | failed_precondition | H001–H010 on the planned side | rewrite in the safe form or `--ack CODE` |
| `out-of-order migrations ...: newest applied version on x is N` | failed_precondition | a new file older than the newest applied version | `--allow-out-of-order` if intended |
| `run "x": run is not failed or parked` | failed_precondition | `resume` on a run in another state | nothing to do |
| `run "x": run is not awaiting contract` | failed_precondition | `confirm` on a run in another state | nothing to do |
| `run "x": run is not the latest on its target or the target is busy` | failed_precondition | revert of an older run, or a `queued`/`running` run exists | revert newer runs first, or wait |
| `baseline runs cannot be reverted` | failed_precondition | revert of a `kind = baseline` run | drop `godwit.migrations` rows by hand if you really need it |
| `N applied versions: target already has applied migrations` | failed_precondition | baseline on a non-empty target | see [baseline](concepts.md#baseline) |
| `drift detection is not enabled` / `baselining is not enabled` | unimplemented | server built without the feature wired (tests only) | — |

## Which plan did this run apply

```bash
godwit run get <run-id>            # read; the `plan:` line is the bound plan id (empty for an implicit plan)
godwit plan show <plan-id>         # statements, hazards and recipes, observation, drift, state and the run
godwit plans --target app          # every stored plan on the target, newest first, with state and run
```

`plan show` answers as long as the plan exists. Bound and superseded plans are deleted by the drift ticker once they are older than `--plan-retention` (90 days by default); after that `run get` shows an empty `plan:` and `godwit audit --run <run-id>` still carries `plan=<id>` on the `run.create` entry, which is the durable answer. Ready plans and the plans of runs that have not finished are never swept.

To apply one specific stored plan instead of whatever `migrate` matches by key, `godwit migrate --plan <plan-id>` (pipeline): the plan supplies target, rollout and files; `--target`, `--rollout` or `--dir` given alongside must agree with it. It is refused when the plan is already bound, superseded or older than `--plan-ttl`, and it is subject to the same stale checks as a matched plan.

## Reverting a run

```bash
godwit revert <run-id>             # pipeline; down files of that run, newest version first
godwit run watch <new-run-id>
```

Only the latest run on the target can be reverted, and only while no run is `queued`/`running` there. A revert is itself a run (`reverts = <original>`); when it succeeds the original becomes `reverted`. Down plans are hazard-gated like up plans: a `DROP TABLE` in a down file needs `--ack H002`. Take a restore point first.

## Store unreachable

`/readyz` returns 503, `api call` lines fail with `internal`, no run is claimed (`GodwitQueuedNotClaimed`). Runs in flight keep going on their target connections. A heartbeat failure logs `heartbeat lost` and stops renewing; if the attempt finishes while the store is still down, its `Finish` is lost and the run stays `running` until its lease expires, after which the next claim reopens the journal, finds every version applied (checksum-checked, skipped) and finishes it properly. If the attempt is still executing when another replica claims it, the advisory lock in the target serialises the two. Fix the store; nothing in godwit needs a restart.
