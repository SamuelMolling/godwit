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

**Meaning.** An `expand-contract` run applied every migration before the first one carrying a contract hazard (`DROP TABLE`, `DROP COLUMN`, `RENAME`: H002, H003, H008) and waits for `ConfirmRollout` to run the rest. Nothing is wrong; the target is usable by both the old and the new application version. It does not block a new `migrate` on the target (only `queued`/`running` runs do), but once a newer run stands on the target, reverting this one takes `godwit revert <run-id> --force`.

```sql
SELECT id, target, created_by, source, created_at, now() - created_at AS waiting
FROM cp_runs WHERE state = 'awaiting_contract' ORDER BY created_at;
```

**Action.** Once the application that no longer reads the old columns is fully rolled out (ArgoCD PostSync does this automatically):

```bash
godwit run confirm <run-id>                        # pipeline scope
godwit run confirm --latest --target <target>      # or by target
```

Take a [restore point](operations.md#backups-and-pitr) first. If the deploy was rolled back instead, `godwit revert <run-id>` runs the down side of the migrations that run applied.

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

## The target's advisory lock is held by a session that is gone

**Symptom.** Every attempt fails with `transient: acquire advisory lock on <db> (held by pid N, application_name "godwit", idle for 900s): ... canceling statement due to statement timeout (SQLSTATE 57014)`, 30s apart, on a target nothing else is migrating.

**Meaning.** A replica whose network was cut, rather than killed, left a backend behind: PostgreSQL has not noticed its client is gone, and that backend still holds the target's session advisory lock. Every executor session sets `application_name = 'godwit'` and asks for TCP keepalives (`tcp_keepalives_idle = 30`, `interval = 10`, `count = 3`), so an unreachable peer is detected in about a minute and the lock is released without anyone intervening. The run rides that out on its own: the lock wait is bounded at 30s per attempt rather than the run timeout, and the attempt retries with backoff.

```sql
-- in the target, the session holding godwit's advisory lock
SELECT a.pid, a.application_name, a.state, now() - a.state_change AS idle_for, a.client_addr
FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1;
```

**Action.** Wait one minute. godwit never terminates the holder itself: a peer executor between two statements, or one pausing between backfill batches, is indistinguishable from an orphan, and killing that one mid-migration costs more than a lock wait. If the session survives past the keepalive window — a proxy or NAT that answers keepalives for a dead client — confirm from `client_addr` that no replica is on the other end and terminate it by hand with `pg_terminate_backend(pid)`; the run's next attempt takes the lock.

## Validation refused the run

**Symptom.** `godwit migrate` exits 1 with `migration failed validation: ...` (InvalidArgument) or `replay history run <i>: ...` (Internal); `godwit_validation_failures_total` rises; log line `run refused by validation`.

**Meaning.** Before queueing, the service creates `godwit_validate_<run>` on the store server, replays what every run of the target applied and no revert undid (the standing `cp_run_applied` rows, in the order they were applied, with bodies from their run's `cp_run_files`) and then the new files. `migration failed validation` means the new SQL errors on a schema that looks like the target; `replay history` means the recorded history itself does not replay (a migration that depended on data, an extension missing on the store server, a run applied with `--skip-validation` that a later file contradicts).

```sql
-- the history that is replayed
SELECT r.id, r.state, r.created_at, string_agg(a.migration, ', ' ORDER BY a.seq) AS applied
FROM cp_runs r JOIN cp_run_applied a ON a.run_id = r.id
WHERE r.target = :'target' AND NOT a.held AND a.reverted_by IS NULL
GROUP BY r.id ORDER BY r.created_at;
```

**Action.** For `migration failed validation`: fix the migration. For `replay history`: put the missing extension in the database `--scratch-template` names, or, when the history is legitimately unreplayable, baseline the target (`godwit target baseline <target> --version <newest applied>`, see [adopting an existing database](concepts.md#adopting-an-existing-database)) so replay starts from the baseline run. When the replay is *short* rather than broken — it rebuilds less than the target holds — the ledger is behind the target's journal; see [below](#the-ledger-is-behind-a-target). `--skip-validation` on a single run is the escape hatch; it leaves no trace in the audit trail, so note it in the pull request.

Scratch databases are dropped after every validation; a leftover after a crash is harmless. On the scratch server (the store server when `--scratch-dsn` is unset):

```sql
SELECT datname FROM pg_database WHERE datname LIKE 'godwit_validate_%' OR datname LIKE 'godwit_diff_%';
DROP DATABASE godwit_validate_xxx WITH (FORCE);
```

## A `change-type` left a view or index on `<c>_old`

**Symptom.** A `change-type` applied cleanly, and something that reads the column started failing or quietly
returning the old type. The classic is a repeatable view refusing to replace:

```
statement 0 of R__order_stats (up): exec: ERROR: cannot change data type of view column "customer_id" from bigint to text (SQLSTATE 42P16)
```

An index or constraint gives no error at all — it simply stops covering the column the application now reads.

**Meaning.** The contract phase renames `<c>` to `<c>_old` and `<c>_new` to `<c>`, and PostgreSQL moves every
dependency with the *physical* attribute. Everything that was bound to the column is now bound to `<c>_old`.
godwit refuses this at plan time now (see [what the expander refuses](concepts.md#what-the-expander-refuses)), but a
migration applied by an older version is already in this state.

**Find everything that is still on the retired column**, before deciding anything:

```sql
SELECT DISTINCT pg_describe_object(d.classid, d.objid, d.objsubid) AS dependent, d.deptype
FROM pg_depend d
JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
WHERE d.refclassid = 'pg_class'::regclass
  AND d.refobjid = 'public.orders'::regclass
  AND a.attname = 'customer_id_old'
  AND d.deptype IN ('a', 'n')
ORDER BY 1;
```

Anything it lists is reading the pre-migration column. `pg_get_viewdef`, `pg_get_indexdef`,
`pg_get_constraintdef` and `pg_get_triggerdef` on those objects will show `customer_id_old` in their bodies.

**Action.** Recreate each dependent against the new column, in a new migration, one object at a time. A view
must be **dropped and recreated**, not replaced: `CREATE OR REPLACE VIEW` cannot change a column's type, which is
the same error above. Check what depends on the view first (`\d+`, or the query above against the view's own oid),
because `DROP VIEW` is refused while something else reads it.

```sql
DROP VIEW IF EXISTS public.order_stats;
CREATE VIEW public.order_stats AS SELECT customer_id, count(*) FROM public.orders GROUP BY 1;
```

An index is safest rebuilt concurrently and then swapped, which `-- godwit: add-index` and `-- godwit: drop-index`
already generate:

```sql
-- godwit: add-index orders (customer_id) name=orders_customer_id_idx2
-- godwit: drop-index orders_customer_id_idx
```

Do **not** rename `<c>_old` back: the dependents follow the physical attribute, so a rename moves the problem
without fixing it. Once nothing is left on `<c>_old`, retire it with `-- godwit: drop-column orders.customer_id_old`,
which now refuses if anything still depends on it.

Two things godwit cannot see and this query will not list: a `COMMENT ON COLUMN`, which stays on `<c>_old`, and a
`plpgsql` function body naming the column, for which PostgreSQL records no dependency. Grep your functions.

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
| `run "x": run is not revertable: ...` | failed_precondition | revert of an older run, of a run that applied nothing, or of one on a busy target | see [reverting a run](#reverting-a-run) |
| `revert would destroy data: ...` | failed_precondition | the revert plan drops a non-empty table or column | `--allow-data-loss`, or roll forward |
| `baseline runs cannot be reverted` | failed_precondition | revert of a `kind = baseline` run | drop `godwit.migrations` rows by hand if you really need it |
| `target already has applied migrations` | failed_precondition | baseline with nothing left to record on either side | nothing to do; the target is already adopted |
| `<id>: recorded on the target under different content` | failed_precondition | a file's checksum differs from the row the target recorded for that version | restore the file, or fix the target's row; one of the two is wrong |
| `target records migrations the ledger does not: ...` | failed_precondition | the target's journal is ahead of the control plane's ledger | [the ledger is behind a target](#the-ledger-is-behind-a-target) |
| `target and ledger disagree on <t>: ...` | failed_precondition | `reconcile` found a divergence it will not decide | [the ledger is behind a target](#the-ledger-is-behind-a-target) |
| `drift detection is not enabled` / `baselining is not enabled` / `reconciling is not enabled` | unimplemented | server built without the feature wired (tests only) | — |

## Which plan did this run apply

```bash
godwit run get <run-id>            # read; the `plan:` line is the bound plan id (empty for an implicit plan)
godwit plan show <plan-id>         # statements, hazards and recipes, observation, drift, state and the run
godwit plans --target app          # every stored plan on the target, newest first, with state and run
```

`plan show` answers as long as the plan exists. Bound and superseded plans are deleted by the drift ticker once they are older than `--plan-retention` (90 days by default); after that `run get` shows an empty `plan:` and `godwit audit --run <run-id>` still carries `plan=<id>` on the `run.create` entry, which is the durable answer. Ready plans and the plans of runs that have not finished are never swept.

To apply one specific stored plan instead of whatever `migrate` matches by key, `godwit migrate --plan <plan-id>` (pipeline): the plan supplies target, rollout and files; `--target`, `--rollout` or `--dir` given alongside must agree with it. It is refused when the plan is already bound, superseded or older than `--plan-ttl`, and it is subject to the same stale checks as a matched plan.

## Reverting a run

A revert undoes **what that run actually applied** — the per-migration ledger the scheduler writes as
each migration lands (`cp_run_applied`), never the directory the run submitted. `godwit migrate --dir
db/migrations` carries the whole directory on every run, and the migrations it skips as already applied
are not the reverting run's to undo.

```bash
godwit revert --target app --dry-run   # read; prints the plan and queues nothing
godwit revert --target app --ack H002  # pipeline; the newest un-reverted run on the target
godwit revert <run-id> --ack H002      # that run, if it is still the newest un-reverted one
```

Two migrations applied by two runs, then a revert of the second:

```
$ godwit revert 71a9be1b-44eb-4db4-95c5-af7cc0138e17 --ack H002
revert of run 71a9be1b-44eb-4db4-95c5-af7cc0138e17 on app: 1 migration(s), reverse order of application
  20260101000001_b (down): 1 statement(s)
    [0] tx    DROP TABLE b;
run 07ba15e8-b53c-4eb4-a40d-c7bb18339065: queued
run 07ba15e8-b53c-4eb4-a40d-c7bb18339065: succeeded (attempt 1) [statement 0 of 20260101000001_b]
```

The first run's table and its `godwit.migrations` row are untouched. `godwit run get <run-id>` lists the
same ledger under `applied:`, with `(reverted by <run>)` on the rows a revert has undone.

**The plan is always printed before anything runs**, and `--dry-run` is that plan on its own — it is what
the pull-request comment shows when the Action runs `command: revert`.

### What it refuses

| Refusal | Why | Release |
|---|---|---|
| `run "x": run is not revertable: run y is newer and still stands` | reverting behind a newer run is almost always a mistake | revert the newer run first, or `--force` |
| `revert would destroy data: <migration> drops table public.t holds 12 row(s)` | the plan would drop a non-empty table or column | `--allow-data-loss`, or roll forward instead |
| `run "x": run is not revertable: it applied no migration that still stands` | the run applied nothing, or a revert already undid all of it | nothing to do |
| `run "x": run is not revertable: target app has a queued or running run` | the target is busy | wait |
| `unacknowledged hazards (...)` | the down files carry hazards | `--ack H002` (down plans are hazard-gated like up plans) |

The data-loss gate reads the down files **you** wrote. A `-- godwit: revert` inverse is godwit's own, and
godwit refuses to generate one wherever it would not be lossless (`drop-column`, `drop-index`, `backfill`,
`keep-old=false`), so generated inverses are exempt.

`--force` and `--allow-data-loss` are independent: forcing past a newer run does not allow data loss, and
allowing data loss does not let you skip a newer run.

### What it leaves behind

A revert is itself a run (`reverts = <original>`) and it is added to the history, never subtracted from it:
when it succeeds the original run becomes `reverted`, its ledger rows point at the revert with
`reverted_by`, and both runs stay in `godwit runs` and in the audit. Reverting the same run twice is
refused because the ledger says there is nothing left standing.

A revert that fails part-way leaves the ledger honest: the migrations it undid are marked reverted, the
rest still stand, and a second `godwit revert` picks up exactly what is left.

**Production policy is roll forward.** Every vendor in this space says so, including the ones selling the
feature: a down file is a review artifact, and the answer to a bad migration in production is usually a new
migration or a restore from backup. `revert` is for the minutes after a bad apply and for the pull request
that gets abandoned. Take a [restore point](operations.md#backups-and-pitr) before using it on anything
that matters.

## The ledger is behind a target

**Symptom.** Any of:

```
failed_precondition: target records migrations the ledger does not: app records 20260101000000_orders; run `godwit target reconcile app --dir <migrations>` to adopt what it already has
```

`godwit targets` reports fewer applied migrations than `godwit target status <t>` lists. A plan shows a migration as pending that the target plainly has. The scratch replay rebuilds less than the target holds.

**Meaning.** The target's own journal — which is the fact about what it has applied — is ahead of `cp_run_applied`. Something applied migrations godwit's store never saw: an earlier tool, a hand `psql`, `godwit apply`, another godwit instance, or this one with a store that was rebuilt or restored from an older backup.

**Action.** Reconcile from the directory that built the target. It reads the target and writes only the store:

```bash
godwit target reconcile app --dir db/migrations
# target app: adopted 2 migration(s) from its journal (run 6f2c…): 20260101000000_orders, 20260101000001_total
```

Check what it will find first, without the service, if you want to see it:

```sql
-- on the target
SELECT version, name, checksum FROM godwit.migrations ORDER BY version;
SELECT name, checksum FROM godwit.repeatables ORDER BY name;
```
```sql
-- on the store: what it thinks that target has
SELECT a.migration, a.adopted, r.kind FROM cp_run_applied a JOIN cp_runs r ON r.id = a.run_id
WHERE r.target = :'target' AND NOT a.held AND a.reverted_by IS NULL ORDER BY a.migration;
```

**When reconcile refuses.** It repairs one direction only, and names what it will not decide:

| Refusal | What happened | What to do |
|---|---|---|
| `recorded under different content than the directory carries` | the file in the repository is not the file that was applied | restore the file from the commit that was deployed; only if you are certain the target's row is wrong, correct `godwit.migrations.checksum` by hand |
| `recorded on the target and absent from the directory` | the directory does not carry a migration the target ran | check out the branch or tag the target was migrated from; the replay needs the SQL |
| `standing in the ledger and absent from the target` | the *target* lost history, not the store — a restore from a backup older than the last run, or a hand-emptied journal | this is an incident, not a repair: compare the target's schema with `cp_snapshots`, and decide whether to restore the target further forward or to re-apply |

**After a store restore.** Reconcile every target, not only the noisy one; the drift snapshot is retaken as part of it. Runs the restored store has lost are gone from the history, but the migrations they applied come back as adopted rows, which is what the applied set, the order guard and the replay need. What does not come back is provenance: who ran what, and the directive expansions frozen on those rows — so an adopted `change-type` cannot be reverted, the same as a baselined one.

## Store unreachable

`/readyz` returns 503, `api call` lines fail with `internal`, no run is claimed (`GodwitQueuedNotClaimed`). Runs in flight keep going on their target connections. A heartbeat failure logs `heartbeat lost` and stops renewing; if the attempt finishes while the store is still down, its `Finish` is lost and the run stays `running` until its lease expires, after which the next claim reopens the journal, finds every version applied (checksum-checked, skipped) and finishes it properly. If the attempt is still executing when another replica claims it, the advisory lock in the target serialises the two. Fix the store; nothing in godwit needs a restart.
