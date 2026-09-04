# Testing

Four suites, three gates and one thing that is not a gate.

| Suite | Command | In CI | What it is |
|---|---|---|---|
| unit + in-process integration | `make cover` (inside `make all`) | yes | 100% of statements, testcontainers for anything that talks to PostgreSQL. A gate. |
| end to end | `make e2e` | no | The built binary, two replicas, a real target: crash recovery, hazards, expand/contract, revert, drift, baseline. Minutes. |
| load | `make load` | no | How godwit behaves at sizes a laptop test never reaches: a backfill over tens of millions of rows, a target with a thousand migrations, many targets at once. Prints measurements. Tens of minutes. |
| chaos | `make chaos` | no | Adversarial failure at points the crash rig does not reach: mid-batch, between the intent and the statement, between the statement and the journal, inside `finalize`, inside a revert, with the store or the target severed at the network, and with someone else's index or advisory lock already in the way. Minutes. |

`make load` and `make chaos` are deliberately outside `make all`: they are slow, they want Docker to themselves, and their output is numbers rather than a pass mark. They share the rig in `test/e2e` — one PostgreSQL container, one database per rig, the real `godwit serve` binary driven through the real CLI and the connect API.

Every measurement is printed as a single line beginning `RIG`, so a run is read with `make load | grep RIG`.

## Knobs

The load rig sizes itself from the environment; the defaults are what the numbers below were taken at.

| Variable | Default | Sizes |
|---|---|---|
| `GODWIT_LOAD_ROWS` | `10000000` | the table the backfill walks |
| `GODWIT_LOAD_BATCH` | `20000` | rows per backfill transaction |
| `GODWIT_LOAD_WRITE_ROWS` | `2000000` | the table the concurrent-write case walks |
| `GODWIT_LOAD_HISTORY` | `1000` | migrations in the deep target's history |
| `GODWIT_LOAD_CHUNK` | `100` | migrations admitted per run while that history is built |
| `GODWIT_LOAD_TARGETS` | `24` | targets in the contention case |
| `GODWIT_LOAD_REPLICAS` | `3` | replicas in the contention case |

A quick pass that exercises every path in a couple of minutes:

```
GODWIT_LOAD_ROWS=200000 GODWIT_LOAD_WRITE_ROWS=100000 \
GODWIT_LOAD_HISTORY=50 GODWIT_LOAD_CHUNK=25 \
GODWIT_LOAD_TARGETS=6 GODWIT_LOAD_REPLICAS=2 make load
```

## How the faults are injected

The crash rig in `crash_test.go` kills a replica at a statement boundary it reaches by making the statement slow. Three devices in `harness_test.go` reach the rest.

- **`holdLock`** takes a named lock in its own transaction and holds it. Because the executor's every write goes through a table, a lock is a pause button at an exact instruction: `LOCK TABLE godwit.journal` freezes the `done` write of a statement that has already run; `LOCK TABLE godwit.migrations` freezes `finalize`; `LOCK TABLE <t> IN SHARE MODE` freezes a `CREATE INDEX CONCURRENTLY` before it starts without also freezing the schema snapshot that admission takes.
- **`faultProxy`** is a TCP hop in front of PostgreSQL. `proxyCut` severs every live session and refuses new ones — the target or the store disappears. `proxyHang` stops forwarding without closing anything — the connection is up and nothing moves.
- **`reap`** kills a replica and then terminates the backends it left behind. A PostgreSQL backend waiting on a lock does not notice that its client is gone, so without this the dead replica's statement still lands when the lock frees, which is not what a crash looks like.

## Load

Measured on an Apple M5 Pro (18 cores, 48 GB), macOS 15.6, Docker Desktop (12 CPUs, 16 GB), `postgres:17-alpine` with `max_connections=300`, one suite at a time, on a machine that was not idle. The point-to-point noise is large; the trends and the ratios are the signal, not the third digit.

### A batched backfill over 10 000 000 rows

`-- godwit: backfill bf set='w = v * 2' where='w IS DISTINCT FROM v * 2' key=id batch=20000`

The box was shared while these were taken and the spread is wide, so each row is the **median of four runs** with the range beside it.

| | |
|---|---|
| seed | 10 000 000 rows, 668 MB |
| backfill | **30.0 s, ~333 000 rows/s**, 500 batches (29.3–41.1 s) |
| the same run without the sync trigger | 28.2 s, ~355 000 rows/s (24.6–45.0 s) |
| the same run with the guard inside the plpgsql body | 53.0 s, ~189 000 rows/s (41.3–95.5 s) |
| peak RSS of the replica | 44–48 MB |
| `godwit.journal` rows when the run ends | 8: the earlier migration's, the backfill's six statements, and the batch's `intent` row beside its `done` |
| `godwit.journal` on disk, before and after | 32 KB → 32 KB |
| the table on disk, before and after | 668 MB → 1.41 GB |
| journalled `rows_done` | exactly 10 000 000 |

The journal does not grow with the backfill: a batched statement keeps one `intent` row whose `cursor` and `rows_done` are updated in place, in the same transaction as the rows it counts. Memory is flat — the executor holds one batch of keys at a time, never the result set. The cursor never went backwards across the run.

**The sync trigger costs about 6%, and where its guard sits is why.** The backfill's own batches fire the trigger for every row they touch, so whatever it does it does ten million times. With the guard inside the plpgsql body (`IF EXISTS (SELECT 1 FROM (SELECT new.*) …)`) every one of those rows entered plpgsql and ran an SPI query: **1.9× the run**. The same predicate as the trigger's `WHEN` clause is evaluated by the executor in C, and the function is entered only for a row that needs it — which, once a batch has written the row, is none of them. That is 30.0 s against a 28.2 s floor.

### `pause` does what it claims

40 000 rows, `batch=1000`, 40 batches: 0.53 s with no pause, 8.92 s with `pause=200ms`. The 8.39 s it added clears the 7.8 s floor (39 gaps × 200 ms) with scheduling on top. `pause` sleeps between batches only, so the last batch is not followed by one.

### A backfill under a live write workload

2 000 000 seeded rows, one writer appending 100 rows every 5 ms and one updating 100 existing rows every 5 ms inside the first fifth of the key space. The writers are stopped once the journalled cursor is past the middle of the table, so everything they wrote — the reserved band above all, which the cursor left behind in the first fractions of a second — landed while the sync trigger was installed. What is written after the trigger goes is past the migration and is not what the backfill promised.

| | before the sync trigger | with it |
|---|---:|---:|
| the run | 6.0 s, `succeeded` | 5.5 s, `succeeded` |
| appended during the run | 58 800 | 49 400 |
| updated during the run | 55 800 | 47 800 |
| left stale in the band the updater wrote to | **34 402** | **0** |
| left stale above the seeded rows | 0 | 0 |
| left stale where nobody wrote | 0 | 0 |
| left stale, all told | **34 402** | **0** |

Before, the run reported `succeeded` over 34 402 stale rows: a row written below the cursor after the cursor passed it was never revisited, and a row appended after the final batch was never seen. `rows_done` was exactly what the database changed, so the journal did not lie — but "the backfill succeeded" did not mean "every row now satisfies the predicate". `change-type` never had the problem, because its expansion installs a sync trigger before the backfill; a plain `backfill` installed nothing.

Now it installs the same thing, and refuses to finish without counting what is left ([concepts](concepts.md#backfill-keeps-its-rows-in-sync-while-it-runs)). `rows_done` is no longer the seeded row count and should not be: the trigger reaches some rows before the batches do, and the batches then skip them. The rig bounds it from below by `seeded − appended − updated` instead, and the check that matters is that **no row is left**.

### A target with a long history

One target grown to 1000 migrations, 100 at a time, each a `CREATE TABLE`. `godwit plan` here is the cost of planning *one* new migration against a history of that size — the cost of `Validator.Validate`'s scratch replay, which every `plan`, `migrate`, `verify` and `diff` on the target pays. The last row is the same measurement after `godwit checkpoint` collapsed the thousand into one file and the target recorded it.

| History | `godwit plan` | `godwit target status` | `godwit diff` | `godwit migrate` of the next 100 |
|---|---:|---:|---:|---:|
| 100 | 4.1 s | 0.03 s | 2.3 s | 3.4 s |
| 200 | 18.5 s | 0.09 s | 5.0 s | 11.8 s |
| 300 | 6.2 s | 0.15 s | 8.9 s | 43.8 s |
| 400 | 7.4 s | 0.06 s | 13.5 s | 9.0 s |
| 500 | 10.3 s | 0.14 s | 20.2 s | 10.9 s |
| 600 | 20.9 s | 0.16 s | 30.3 s | 17.8 s |
| 700 | 19.4 s | 0.38 s | 40.1 s | 29.0 s |
| 800 | 21.2 s | 0.18 s | 49.1 s | 21.9 s |
| 900 | 28.6 s | 0.21 s | 63.8 s | 29.7 s |
| 1000 | 31.9 s | 0.24 s | 79.2 s | 34.6 s |
| **1000, checkpointed** | **36.3 s** | 0.28 s | **81.1 s** | — |

Re-measured after the checkpoint body was rendered for an empty database, on a machine running three other suites at the same time — so the absolute seconds are inflated and the curve above is not comparable row by row. The two rows below are one run, back to back:

| History | `godwit plan` | `godwit target status` | `godwit diff` |
|---|---:|---:|---:|
| 1000 | 46.4 s | 0.53 s | 111.4 s |
| **1000, checkpointed** | **46.8 s** | 0.49 s | **104.5 s** |

`diff` is the clean line: it replays the history twice — once for the drift report and once to build the base the committed files claim to produce — and grows from 2.3 s to 79.2 s over the ten points. `plan` replays it once and ends at 31.9 s. Both grow with the history at **about 40 ms per migration the target has ever applied**, and `diff`'s two replays against `plan`'s one is why its line is twice as steep. Building the whole thing took 15 minutes.

Those 40 ms are **not** all replay, which the checkpoint work below had to establish before it could tell whether a checkpoint helps: timed directly, `Validator.Validate` over 1000 of these migrations is 6.1 s, an eighth of the `plan` it sits inside. The rest is the request carrying 2001 files, `CreateRun`/`PlanRun` persisting them, and libpg_query parsing every one.

`Validate` creates a scratch database, replays the history migration by migration through the same `Executor` a target uses, then applies the pending plans and snapshots the schema after each one. Those 6 ms per migration are 27 round trips: two for the advisory lock, **eleven for `bootstrap`**, and the rest for the run row, the statement, the journal and `finalize`. `bootstrap` is idempotent DDL that `Executor.apply` re-runs for every plan, so two in five of the replay's round trips re-create tables that already exist.

`GetTargetStatus` never replays: it reads the applied set out of the store and compares it with the files it was sent, so it grows with the directory and not with the history.

**What the checkpoint bought, before it was fixed: nothing, on this shape.** `plan` went from 31.9 s to 36.3 s and `diff` from 79.2 s to 81.1 s. The collapse itself worked — `collapseAtCheckpoint` records the thousand migrations in one `INSERT … SELECT unnest(…)` instead of replaying them. What replaced them was the checkpoint's own body, and pg-schema-diff renders every primary key as three statements: `CREATE TABLE`, `CREATE UNIQUE INDEX CONCURRENTLY`, `ALTER TABLE … ADD CONSTRAINT … USING INDEX`. For 1000 tables that was 275 KB, **3001 statements, 1000 of them `CONCURRENTLY`** — non-transactional index builds, each several table scans and a wait for concurrent transactions, on a database that has neither rows nor concurrent transactions.

**The body is now rendered for the database it meets**, and the same rig reports it at **1001 statements, 0 `CONCURRENTLY`, 147 222 bytes** — a third of the statements and 46% of the bytes. Generating it took 61.2 s and recording it on the target 74.5 s on the loaded machine.

**What that is worth, measured on the replay itself.** End to end, `plan` at 1000 migrations barely moves (46.4 s → 46.8 s) and `diff` gains 6% — because at that directory size the scratch replay is not what `plan` is spending its time on. Timing `Validator.Validate` directly, on the same two shapes the repository's own checkpoint tests use, one store, both targets validated three times alternately and the best of each taken:

| 1000 migrations | replayed whole | replayed from the checkpoint | |
|---|---:|---:|---|
| purely additive (`longHistory`) | 6.12 s, 1000 executed | **3.17 s**, 1 executed | 1.9× |
| churning (`churningHistory`) | 10.61 s, 1000 executed | **0.14 s**, 1 executed | 76× |

The replay is 1.9× faster on the shape a checkpoint helps least — a history that only ever adds, so the checkpoint has to build every table the history built, and all it saves is the per-migration overhead — and 76× faster on the shape it is for, where the body is 430 bytes because the history built and dropped as it went.

**So the checkpoint pays, and `godwit plan` at 1000 migrations is not bounded by the replay.** 6.1 s of a 46 s `plan` is the replay; the rest is the request carrying 2001 files, the store persisting them, and libpg_query parsing every one of them on the way through. Making *that* cheaper is the whole-directory finding below, not the checkpoint's.

The store at 1000 migrations across ten runs: `cp_run_files` 2.88 MB, `cp_plan_files` 2.82 MB, `cp_run_applied` 229 KB, `cp_snapshots` 213 KB. Every run persists the *whole* directory, so those two tables hold 11 000 file bodies for 1000 migrations.

**1000 migrations is also where the default request limit stops.** Every `plan`, `migrate`, `verify` and `diff` sends the whole directory, two files per migration, and `--max-files` defaults to 2000. At exactly 1000 the directory fits; the 1001st does not, and neither does the one extra file `godwit checkpoint` writes. This rig raises `--max-files` so it can measure past it.

### Many targets, many replicas

24 targets, 3 replicas, one run submitted to each at once, each run a `CREATE TABLE` and a `pg_sleep(1)`.

| | |
|---|---|
| submitting 24 runs | 6.74 s |
| draining them | 1.81 s |
| end to end | 8.54 s, 2.81 runs/s |
| claims per replica | 10, 7, 7 |
| peak backends on the store | 9 |

`Scheduler.dispatch` starts one `Tick` per free slot on its own goroutine, `--max-concurrent-runs` (4) per replica, so the 24 runs drain in 1.8 s once they are admitted. Admission is now the bottleneck: 24 concurrent `CreateRun` calls each validate on a scratch database, and `--max-concurrent-diffs` bounds how many do that at once, which is where the 6.7 s goes. The claim query excludes a target that already has a live lease, so two runs on one target still never overlap. `TestChaosTargetHangsMidStatement` pins the other half: with a target black-holed at the network, a run on an unrelated target claimed and finished in 0.47 s while it hung.

## Chaos

Each case injects a fault and then asserts the same three things: the journal never lies, a resume converges, and what the plan showed is what ran.

| Case | Fault | What it pins |
|---|---|---|
| `KillMidBatch` | SIGKILL during a batched backfill | the resumed cursor is the last committed batch, `rows_done` ends at exactly the row count, the crash costs the in-flight batch and nothing else, and the resume leaves no sync trigger behind |
| `KillBetweenIntentAndStatement` | SIGKILL while `CREATE INDEX CONCURRENTLY` waits for a lock, after the `intent` row | an intent with no index means "run it", and the index is built once |
| `KillBetweenStatementAndJournal` | the `done` write frozen on a table lock after the index is valid, then SIGKILL | the survivor adopts the index by OID instead of rebuilding it |
| `KillDuringFinalize` | `godwit.migrations` frozen while `finalize` waits, then SIGKILL | the resume records the migration once and re-runs no statement |
| `KillDuringContractPhase` | SIGKILL inside the phase a human confirmed | the expand phase is not undone and both versions are recorded once |
| `KillDuringRevert` | SIGKILL inside a `.down.sql` | the revert completes, the version leaves `godwit.migrations`, the original run reads `reverted` |
| `StoreOutageMidRun` | the control-plane database severed from the executing replica, a second replica healthy | the run converges through the other replica, applied exactly once |
| `StoreBlipMidRun` | the control-plane database severed from the only replica for two seconds mid-statement | the beats retry and the lease survives: one attempt, no reclaim, no lost heartbeat |
| `OrphanHoldsTheAdvisoryLock` | a session with godwit's own `application_name` holding the target's `pg_advisory_lock` | the wait is bounded at 30 s and reported as transient with the holder named, not left to the run timeout |
| `IndexOfThatNameIsNotTheOnePlanBuilds` | a foreign `big_v_idx ON big (id)` created while the plan's `CREATE INDEX CONCURRENTLY big_v_idx ON big (v)` is between its intent and its statement | the resume refuses instead of adopting it; nothing is recorded and the foreign index is left alone |
| `DiskFullRetries` | `53100` raised once on the target and not on the scratch | a full disk is transient: one retry, then success |
| `TargetCutMidStatement` | the target's TCP session severed mid-statement | the failure reads `transient:` and the run retries to success |
| `TargetHangsMidStatement` | the target black-holed at the network | `--run-timeout` cuts the run loose as transient, and a run on another target still starts and finishes while it hangs |
| `BackendTerminatedMidStatement` | `pg_terminate_backend` from another session | `57P01` is transient and the run retries to success |
| `LockTimeoutInContractPhase` | the table held under `ACCESS EXCLUSIVE` across `run confirm` | `55P03` in the contract phase retries rather than losing the phase |
| `ConcurrentDDLBreaksAStatement` | another session drops the object statement 2 needs | a clean `sql:` failure, the statements that committed stand, the version is not recorded |
| `PrivilegeLostBetweenPlanAndApply` | `REVOKE CREATE` between `plan` and the run | `42501` is not transient: one attempt, nothing applied |
| `ResumeAfterTheMigrationShrank` | a version re-submitted with fewer statements than its failed run journalled | the run refuses to resume instead of taking the service down |

Every case above passed; the suite takes 69 s. What the timings say:

- **A kill costs one lease.** `--lease-ttl 5s` in the rig, and convergence after a kill was 4.2–12.1 s across the cases — the lease expiry plus the work that had to be redone. `attempts` went from 1 to 2 in every kill case and no further.
- **A crash mid-backfill costs the in-flight batch and nothing else.** Killed at cursor 24 000 of 400 000 rows with `batch=2000`, the resumed run ended with `rows_done` at exactly 400 000.
- **A crash between the statement and its journal write costs nothing.** The index OID before the kill and after the resume was the same: the survivor adopted the index rather than rebuilding it.
- **A store outage does not lose the run, and a store blip does not cost the lease.** With the store gone for good, the islanded replica logged `heartbeat lost` once, cancelled the run it could no longer prove it owned, the spare claimed it once, and it converged in 25 s. With the store gone for two seconds, the same replica retried two beats, kept the lease and finished the run on its first attempt.
- **An orphaned advisory lock costs 30 s, not a run timeout.** A session holding the target's `pg_advisory_lock` made the run report `transient: acquire advisory lock on <db> (held by pid N, application_name "godwit", idle for 30s): … (SQLSTATE 57014)` after 30.3 s, and it converged 0.3 s after the holder let go.
- **A severed connection costs one retry**, a `pg_terminate_backend` costs one retry, and a lock timeout in the contract phase costs one retry. All three converged.
- **A black-holed target costs one run timeout and blocks nothing else.** With `--run-timeout 8s` the hung run was cut loose after 7.5 s with `transient: … timeout: context deadline exceeded`, and a run on an unrelated target claimed and finished in 0.47 s while it hung. What it leaves behind is one PostgreSQL session still holding the target's advisory lock; keepalives on the executor's session now make PostgreSQL notice within about a minute, and the next attempt's bounded wait rides that out.

## What the rigs found

| Finding | Where | Status |
|---|---|---|
| A version re-submitted with fewer statements than a failed run journalled panicked `loadProgress` with an index out of range. Nothing recovers a panic, so the replica died, the next replica claimed the same run and died too. | `internal/engine/journal.go` | fixed: the journal row's index is bounds-checked against the plan and the run refuses to resume, the way a changed statement hash already did |
| A severed target connection surfaces as pgx's `ErrConnClosed`, which is neither a `net.Error` nor an `io.EOF` nor a `PgError`, so `classify` called it permanent: a network blip failed the deploy on the first attempt with no `transient:` prefix and no retry. | `internal/controlplane/retry.go` | fixed: `pgconn.ErrConnClosed` joins the network class the decision record already puts there |
| A `backfill` directive silently left rows stale when the table was written to during the run: 34 402 of 2 000 000, reported as `succeeded`. | `internal/controlplane/expand.go` | fixed: the expansion now installs the sync trigger `change-type` always had, and generates a closing assertion that counts the rows still matching before the run may succeed |
| A checkpoint does not make the scratch replay cheaper: `plan` 31.9 s → 36.3 s and `diff` 79.2 s → 81.1 s at 1000 migrations. The collapse works; what replaces the replayed migrations is a 3001-statement body with 1000 `CREATE INDEX CONCURRENTLY` in it. | `internal/controlplane/checkpoint.go` | fixed: the body is rendered for the empty database it meets — no `CONCURRENTLY`, and the index/constraint pairs folded back into their `CREATE TABLE`. 1001 statements, and the replay 1.9× faster on this shape and 76× on a churning one. Numbers above |
| `godwit checkpoint` renders an empty checkpoint and refuses on the setup every doc example uses. `Checkpointer.Generate` replays the collapsed migrations on a scratch database and renders the result with pg-schema-diff, which excludes schema `godwit`. Nothing pins that session's `search_path`, and the replay's own `bootstrap` creates schema `godwit` there — so when the scratch role is also called `godwit`, `search_path = "$user", public` resolves `$user` to it and every unqualified `CREATE TABLE` lands in the excluded schema. Three migrations of `CREATE TABLE h0 (id bigint PRIMARY KEY)` reproduce it; the same three written `CREATE TABLE public.h0 …` generate correctly. `Validator.Validate` is immune because it mirrors the target's observed `search_path` onto the scratch; `Generate` takes no target and mirrors nothing. | `internal/controlplane/checkpoint.go` | fixed: generation pins `search_path` to `public`, so no scratch role's name can decide where the collapsed migrations land. `TestCheckpointRendersUnqualifiedDDL` is the regression |
| A directory of 1000 migrations is the ceiling on the default `--max-files 2000`: every request carries the whole directory at two files per migration, and a checkpoint adds a file rather than removing any — the collapsed migrations stay until every target is above the checkpoint and someone deletes them by hand. | `internal/api/limits.go` | reported, not fixed: raising the default, or sending only the pending set, are both product decisions |
| Two in five of the scratch replay's round trips are `bootstrap` re-creating tables that exist, once per migration. | `internal/engine/executor.go` | reported, not fixed: one `Executor` serves every plan of a run over one connection, so memoising `bootstrap` on it is small. A checkpoint already pays it once for the whole collapsed range |
| `reconcileCreateIndex` adopts *any* valid index with the planned name. Given a plan statement `CREATE INDEX CONCURRENTLY big_v_idx ON big (v)` and an existing `big_v_idx ON big USING btree (id)` built by someone else, it returns "done". After a crash between the intent and the `done` write, that index is journalled `done` and the migration is recorded as applied, and the plan is not what ran. | `internal/engine/verify.go` | fixed: the planned statement and `pg_get_indexdef` are both normalised through libpg_query and compared, and the index's table is compared by OID; an index of that name that is not the one the statement builds refuses the run rather than being adopted or dropped |
| A black-holed target leaves a PostgreSQL session holding `pg_advisory_lock` on it. `acquireLock` blocks with no bound of its own — `lock_timeout` does not cover advisory locks — so once the network heals every retry blocks there until the run's own timeout fires. Measured with `--run-timeout 8s` and `--max-attempts 10`: ten attempts, every one `transient: acquire advisory lock: timeout: context deadline exceeded`, the run parked in `needs_attention` after 368 s, and the target stayed stuck until the orphan session was terminated by hand. In production the orphan lives until TCP keepalive gives up, which is two hours by default. | `internal/engine/lock.go` | fixed: the wait runs under `statement_timeout` (which does cover advisory locks) and is bounded at 30 s per attempt, the error names the holding pid, and every executor session sets `application_name = 'godwit'` and TCP keepalives so an unreachable replica's backend dies in about a minute instead of two hours |
| A single heartbeat failure stops the heartbeat goroutine for the rest of the run. The run keeps executing, the lease expires, and another replica claims it. Correctness holds — the target's advisory lock serialises the two executors and the journal makes the second one a no-op — but the second replica blocks in `pg_advisory_lock` inside `Tick`, which is the only slot it has. | `internal/controlplane/scheduler.go` | fixed: a failed beat retries every `lease-ttl/10` until a fifth of the lease is left; past that, and on a lease taken by another holder, the run's context is cancelled and the replica writes nothing more about a run it no longer owns |
| `53100` (disk full) and `53200` (out of memory) are not in the transient set, while `53300` (too many connections) is. | `internal/controlplane/retry.go` | fixed: classes `08`, `53` and `57` are transient as classes, plus `58000` and `58030`; `57P04` (database dropped), `58P01` and `58P02` (a missing or duplicated file) stay permanent |
