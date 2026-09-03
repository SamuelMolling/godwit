# Concepts

What godwit stores where, what it promises across a crash, and the vocabulary the CLI, API, logs and metrics share.

## Two databases

| | Lives in | Owned by | Holds |
|---|---|---|---|
| **Journal** | every target database, schema `godwit` | the executor (engine) | `migrations` (applied versions), `repeatables` (the content last applied under each repeatable name), `runs` (one row per migration × direction attempt), `journal` (one `intent`/`done` row per statement, carrying the cursor and row counts of a batched one) |
| **Store** | the service's own database (`--store-dsn`), default schema | the control plane | `cp_targets`, `cp_runs`, `cp_run_files`, `cp_leases`, `cp_snapshots`, `cp_drift_events`, `cp_notifications`, `cp_audit` |

The journal is the truth about what happened on a target. The store is the truth about who asked for what, who is executing it, and what the schema looked like last time. The store's own schema is applied with the same executor at `serve` start-up, so the store database also carries a `godwit` schema tracking the control-plane migrations.

## Migrations, plans, statements

A migration is a version (14-digit timestamp), a name and an up/down SQL pair. The checksum recorded on the target is `sha256` of the up file.

A **repeatable** migration has no version: it is a pair named `R__<snake_name>.up.sql` / `.down.sql`, and it is applied whenever its checksum differs from the one the target recorded under that name. Same file, same checksum, nothing runs. See [repeatable migrations](#repeatable-migrations).

Before anything runs, each side is parsed with libpg_query into a **plan**: an ordered list of statements, each with its own `sha256` hash, a mode and zero or more hazards.

| Mode | Statements | How it runs |
|---|---|---|
| `tx` | everything else | `BEGIN` → `SET LOCAL lock_timeout` / `statement_timeout` → statement → journal `done` → `COMMIT`. DDL and progress commit together. |
| `no-tx` | `CREATE INDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`, `VACUUM`, `REFRESH MATERIALIZED VIEW CONCURRENTLY`, `REINDEX ... CONCURRENTLY` | journal `intent` → `SET` timeouts → statement → `RESET` → journal `done`. A verifier decides what to do if the process died between `intent` and `done`. |
| `batch` | a statement carrying a batch spec (a backfill) | journal `intent` once, then per batch, in one transaction: `SET LOCAL` timeouts → the statement with the cursor as `$1` → `UPDATE godwit.journal SET cursor, rows_done` → `COMMIT`, sleeping `pause` between batches until a batch returns fewer than `size` rows. Then journal `done`. |

`CREATE INDEX CONCURRENTLY` must name its index; an anonymous one is a plan error, because the verifier has nothing to look up after a crash.

### Batched statements

A backfill is **one** plan statement, not N unrolled ones: the row count is unknown when the plan is built and changes before it runs. The statement is the rendered single batch, so its hash is stable and the "statement _i_ changed since run _X_ started" guard needs no special case; the batch spec beside it carries the cursor column and its kind (`int`, `uuid`, `text`), the batch size, the pause and an optional estimate query that fills `godwit.journal.rows_total` once.

The contract on the SQL: it takes the cursor as `$1`, touches at most `size` rows ordered by the key, and returns the key of every row it touched. `$1` is bound as `bigint` / `uuid` / `text`, so a key narrower than `bigint` needs an explicit `$1::bigint` — the initial cursor is the low end of the bound type. The predicate must exclude rows already done (`... IS DISTINCT FROM ...`), which is what makes re-running a batch a no-op.

```sql
WITH b AS (SELECT "id" FROM "public"."users"
           WHERE "id" > $1::bigint AND age_new IS DISTINCT FROM age::bigint
           ORDER BY "id" LIMIT 5000)
UPDATE "public"."users" AS t SET "age_new" = t."age"::bigint
  FROM b WHERE t."id" = b."id" RETURNING b."id"
```

The new cursor is the highest key the batch returned. A key the database orders above the one picked here was in the same batch, so picking low can only repeat work that the predicate then skips — it can never skip a row. `godwit.journal` carries `cursor`, `rows_done` and `rows_total` for the statement, and the cursor advances in the **same transaction** as the batch, so the journal never claims more progress than the database holds. Each batch is one statement, so `statement_timeout` bounds a batch rather than the whole backfill.

| Verifier | Used by | After a crash with a pending intent |
|---|---|---|
| `create_index_concurrently` | `CREATE INDEX CONCURRENTLY` | index exists and `pg_index.indisvalid` → mark `done`; exists but invalid → `DROP INDEX` and run again; absent → run again |
| `drop_index_concurrently` | `DROP INDEX CONCURRENTLY` | index gone → mark `done`; still there → run again |
| `rerun` | `VACUUM`, `REFRESH ... CONCURRENTLY`, `REINDEX ... CONCURRENTLY` | idempotent: run again |
| `batch` | a batched backfill | resume the loop from the journalled `cursor`; the batch that was in flight rolled back, and its rows come back in the next batch |

## The journal protocol

Applying a run on a target:

1. `pg_advisory_lock(fnv64a("godwit:" || current_database()))` — a session lock, one executor per database at a time. A second executor blocks here until the first releases.
2. Bootstrap the `godwit` schema if missing.
3. For each plan, in version order (descending for a revert):
   - up and already recorded (`godwit.migrations` for a version, `godwit.repeatables` for a repeatable) with the same checksum → skip; a version recorded with a different checksum → error `version N already applied with different content`; a repeatable recorded with a different checksum → run it again; down and not recorded → skip.
   - Reopen the newest `godwit.runs` row for this migration and direction in state `running` or `failed`, else insert a new one. A versioned run is keyed by `(version, direction)`; a repeatable run by `(repeatable, checksum, direction)`, so editing the file after a crash starts a run of its own instead of resuming one it no longer matches. Read its journal: every row's `sql_hash` must equal the plan's hash for that index, otherwise `statement i changed since run X started; refusing to resume`. `lastDone` is the highest `done` index; `pendingIntent` is an `intent` above it without a `done`.
   - Execute statements from `lastDone + 1`, as in the table above.
   - On the first error: `godwit.runs.state = 'failed'` with the error text; stop.
   - After the last statement, in one transaction: record the migration — insert into `godwit.migrations`, or upsert `godwit.repeatables` with the new checksum and `applied_at` (up); delete the row (down) — and set the run `succeeded`.
4. Unlock.

The `godwit.runs.id` in the target is generated by the engine per migration and direction; it is not the control-plane run id in `cp_runs`.

### Crash timeline

A run of one migration with three statements; the executing replica is killed at each point.

```
stmt 0  tx     ALTER TABLE orders ADD COLUMN note text
stmt 1  no-tx  CREATE INDEX CONCURRENTLY orders_note_idx ON orders (note)
stmt 2  tx     ALTER TABLE orders ALTER COLUMN note SET DEFAULT ''

time ─────────────────────────────────────────────────────────────────────►
   BEGIN  ALTER  done(0)  COMMIT │ intent(1)  CREATE INDEX ...  done(1) │ BEGIN  ALTER  done(2)  COMMIT │ migrations+succeeded
     ▲              ▲            │     ▲            ▲              ▲     │
     A              B            │     C            D              E     │
```

| Killed at | What the target holds | What the next attempt does |
|---|---|---|
| A (inside the transaction) | nothing: the transaction rolled back, journal has no row for 0 | starts at statement 0 |
| B (after `done(0)`, before `COMMIT`) | nothing: `done(0)` was in the same transaction | starts at statement 0 |
| C (after `intent(1)`, index build not started) | `intent(1)`, no index | verifier finds no index → builds it |
| D (index build interrupted) | `intent(1)`, index present but `indisvalid = false` | verifier drops the invalid index, builds it again |
| E (index built, before `done(1)`) | `intent(1)`, valid index | verifier marks `done(1)`, moves to statement 2 |
| after `done(2)` before finalize | all three statements applied, no `godwit.migrations` row | loop finds nothing pending, writes the `migrations` row and `succeeded` |
| mid-backfill, had statement 1 been batched | `intent(1)` with the `cursor` and `rows_done` of the last **committed** batch; the batch in flight rolled back | resumes at that cursor; the rows of the lost batch are still pending, so they come back |

There is no dirty flag and no repair command: the next attempt reads the journal and continues. The control plane's job is to make sure there is a next attempt ([leases](#leases)).

## Runs and states

A **run** is one request to apply a set of files to one target. `cp_runs` carries `state`, `attempts`, `rollout`, `phase`, `kind`, `reverts`, `created_by`, `source`, per-run timeouts and the error.

```
              CreateRun / RevertRun                       ConfirmRollout
                     │                                          │
                     ▼          claim                           ▼
                  queued ───────────────► running ◄────────── queued (phase = contract)
                                            │
             ┌──────────────┬───────────────┼───────────────────┐
             ▼              ▼               ▼                   ▼
         succeeded      failed      awaiting_contract     needs_attention
             │              │                                   │
             │              └─────── ResumeRun ──► queued ◄──────┘
             ▼
         reverted   (set on the original when its revert succeeds)
```

| State | Meaning | Terminal |
|---|---|---|
| `queued` | admitted, waiting for a replica to claim it | no |
| `running` | a replica holds the lease and is executing | no |
| `succeeded` | every statement of the phase applied | yes (until reverted) |
| `awaiting_contract` | `expand-contract` run: expand phase done, contract statements held back | until `ConfirmRollout` |
| `failed` | a statement failed with a genuine SQL error (`sql:`); the journal keeps the progress | until `ResumeRun` |
| `needs_attention` | the attempt budget is exhausted (`transient: gave up after N attempts`) or an operator parked it with `ParkRun` | until `ResumeRun` |
| `reverted` | a later revert run of this run succeeded | yes |

A genuine SQL failure goes straight to `failed`. A transient one (`40001`, `40P01`, `55P03`, `57014`, `53300`, `57P01`–`57P03`, class `08`, a lost connection, a deadline) puts the run back in `queued` with `not_before` set: the wait is `--tick-interval` doubled per attempt, capped at 5 minutes, with ±20% jitter, and `retries` counts them. `needs_attention` is reached when the run has used `--max-attempts` attempts, whether lost leases or transient failures, or by `ParkRun`. `ResumeRun` puts either back in `queued` with `attempts = 0` and no wait.

A pipeline re-run does not queue a second run: `CreateRun` with the same files, target and rollout as a bound plan re-attaches to that plan's run (`reattached` in the response, `run.reattach` in the audit). A `queued`, `running` or `awaiting_contract` run is simply followed; a `succeeded` one is followed too if the target still has everything it applied; a `failed` or `needs_attention` one is resumed when the only history the plan did not know is the run's own progress; a `reverted` one releases the plan and a fresh run is created. An explicit `--plan <id>` skips re-attaching.

`kind` is `migrate` or `baseline`; `phase` is `expand` or `contract`; `reverts` links a revert run to the run it undoes.

## Leases

Runs are executed by whichever replica claims them. Every replica ticks every `--tick-interval` (2s):

- `Claim`: pick one `queued` or `running` run whose lease is missing or expired, at most one per target, `FOR UPDATE SKIP LOCKED`. Set it `running`, `attempts + 1`, write `cp_leases(run_id, holder, expires_at = now + ttl)`.
- Heartbeat every `ttl / 3` while executing. A heartbeat that fails (store unreachable, lease taken by another holder) logs `heartbeat lost`, counts `godwit_heartbeat_failures_total` and stops renewing; the attempt itself keeps running.
- On finish, delete the lease and set the final state.

A replica that dies stops heartbeating; after `--lease-ttl` (30s) another replica claims the same run, `attempts` becomes 2, and the executor resumes from the journal. If `attempts` exceeds `--max-attempts` (5) at claim time the run is finished as `needs_attention` without executing; a run held back by `not_before` is not claimed until the store's clock passes it. `holder` is the replica's hostname.

One target executes one run at a time: the claim query hands out one lease per target, and the advisory lock on the target itself serialises executors that end up overlapping (a stalled replica whose lease expired and its successor), so the second one waits rather than interleaving statements.

## Admission

`CreateRun`, `PlanRun` and `RevertRun` go through the same gate, in this order, before anything is queued:

1. **Target exists** — `not_found` otherwise.
2. **Hazard gate** — every hazard code in the plans must be in `acknowledge_hazards`; otherwise `failed_precondition` listing them.
3. **Out-of-order guard** — a pending version below the newest version in the target's history (files of `succeeded` runs, migrate and baseline) is refused with `failed_precondition` unless `allow_out_of_order`; allowed ones are logged. Reverts skip this check, and repeatables carry no version so they are never out of order.
4. **Scratch validation** — a database `godwit_validate_<id>` is created on the store server, the files of every succeeded run of the target are replayed in order, then the new plans one at a time, snapshotting the schema after the history and after each plan; the database is dropped `WITH (FORCE)`. A failure in the new plans is `invalid_argument: migration failed validation: ...`; a failure in the history replay is `internal: replay history run i: ...`. Skipped with `skip_validation` or `serve --skip-validation`. The snapshots feed [already-applied detection](#already-applied-migrations) when the plan is persisted.

### Hazards

The planner tags statements that hurt a live database. Each code names the safe form in its message and carries a **recipe**: the safe form as ready-to-copy SQL, built from the parsed statement with its real table, column, type, index and constraint names (generated names where the statement had none, e.g. `users_email_idx`, `orders_user_id_fkey`, `users_pkey`). Recipes appear indented under the finding in `lint` and `plan` text output, in a `<details>` block per finding in markdown, as `recipe` in JSON, and as `PlannedHazard.recipe` on the API.

| Code | Statement | Recipe | Phase under `expand-contract` |
|---|---|---|---|
| H001 | `CREATE INDEX` without `CONCURRENTLY` | `CREATE INDEX CONCURRENTLY <name> ON <t> ...;` with the same columns, method and predicate | expand |
| H002 | `DROP TABLE` | text: ship the application version that no longer uses the table, then drop it in a contract migration | contract |
| H003 | `DROP COLUMN` | text: ship the application version that no longer reads or writes the column, then drop it in a contract migration | contract |
| H004 | `ALTER COLUMN ... TYPE` | `ADD COLUMN c_new <type>`, a trigger that keeps `c` and `c_new` in sync (honours the `USING` expression), a batched `UPDATE ... WHERE id BETWEEN $1 AND $2` backfill template, then in a later migration drop the trigger and `RENAME COLUMN c TO c_old` / `c_new TO c`, and `DROP COLUMN c_old` in a contract migration | expand |
| H005 | `ADD COLUMN ... NOT NULL` without `DEFAULT` | `ADD COLUMN c <type>` nullable, backfill, then the H007 recipe; or `ADD COLUMN c <type> NOT NULL DEFAULT $1` when a constant default fits (PostgreSQL 11+, metadata-only) | expand |
| H006 | `ADD CONSTRAINT ... FOREIGN KEY` / `CHECK` without `NOT VALID` | `ADD CONSTRAINT <n> ... NOT VALID;` then `VALIDATE CONSTRAINT <n>;` | expand |
| H007 | `ALTER COLUMN ... SET NOT NULL` | `ADD CONSTRAINT c_not_null CHECK (c IS NOT NULL) NOT VALID;` / `VALIDATE CONSTRAINT c_not_null;` / `ALTER COLUMN c SET NOT NULL;` / `DROP CONSTRAINT c_not_null;` | expand |
| H008 | `RENAME` table or column | text: add the new one, dual-write and backfill, ship the application version that uses it, drop the old one in a contract migration | contract |
| H009 | `DROP INDEX` without `CONCURRENTLY` | one `DROP INDEX CONCURRENTLY <name>;` per index | expand |
| H010 | `ADD PRIMARY KEY` / `UNIQUE` without `USING INDEX` | `CREATE UNIQUE INDEX CONCURRENTLY <n>_idx ON <t> (...);` then `ADD CONSTRAINT <n> PRIMARY KEY USING INDEX <n>_idx;` | expand |

Contract hazards (H002, H003, H008) are the ones after which the previous application version stops working; the others block or fail while the statement runs and are covered by acknowledgement and timeouts. H007 fires even when the matching CHECK already exists: the planner is offline and has no catalog. `godwit lint` reports the up side only. The recipe is a hint, never executed by godwit: the H004 backfill names `id` as the batching key because the planner has no catalog to read the primary key from, and the `$1` placeholders are yours to fill. Where the safe form can be stated as a [directive](#directives), the recipe opens with it.

```
$ godwit lint --dir db/migrations
20260901120000_users_email_idx.up.sql: error H001 CREATE INDEX without CONCURRENTLY blocks writes on users
    CREATE INDEX CONCURRENTLY users_email_idx ON users USING btree (email);
20260901120100_users_email_required.up.sql: error H007 SET NOT NULL on email scans the table under an exclusive lock; add CHECK (email IS NOT NULL) NOT VALID, VALIDATE CONSTRAINT it, then SET NOT NULL is instant on PostgreSQL 12+
    ALTER TABLE users ADD CONSTRAINT email_not_null CHECK (email IS NOT NULL) NOT VALID;
    ALTER TABLE users VALIDATE CONSTRAINT email_not_null;
    ALTER TABLE users ALTER COLUMN email SET NOT NULL;
    ALTER TABLE users DROP CONSTRAINT email_not_null;
2 finding(s), 2 blocking
```

### Directives

A hazard recipe hands over the safe SQL as text. A **directive** is the other direction: the migration says what it
wants and lets godwit produce the lock-safe statements. It is a SQL line comment, so the file stays a plain `.sql`
that any other tool can read (the syntax precedent is Atlas's `-- atlas:txmode none`).

```sql
-- godwit: change-type users.age bigint using='age::bigint' batch=5000 pause=100ms
-- godwit: backfill users set='age_new = age::bigint' where='age_new IS NULL' key=id batch=5000
-- godwit: add-not-null users.email
-- godwit: add-column users.joined 'timestamp with time zone' not-null
-- godwit: add-index users (email) where='deleted_at IS NULL'
-- godwit: drop-index users_email_idx
-- godwit: add-fk orders.user_id -> users.id on-delete=cascade
-- godwit: add-check users users_age_check 'age > 0'
-- godwit: drop-column users.age_old
```

**Grammar.** A line whose first non-space text is `-- godwit:`, then the operation, its positional arguments, then
`key=value` options and bare flags. One directive per line, and the line must be its own: a `-- godwit:` that trails
a statement is an error, and one inside a string literal, a dollar-quoted body or a `/* */` block is not a directive
at all. Values containing spaces are single-quoted and `''` is a literal quote; a parenthesised list counts as one
argument, so `numeric(10, 2)` and `(email, id)` need no quotes. A directive occupies the **position in the file**
where its statements are spliced, so directives compose with ordinary SQL in the same file.

| Op | Arguments | Options | Flags |
|---|---|---|---|
| `change-type` | `<t>.<c> <type>` | `using=` `key=` `batch=` `pause=` `keep-old=` | `not-null` |
| `backfill` | `<t>` | `set=` (required) `where=` `key=` `batch=` `pause=` | |
| `add-column` | `<t>.<c> <type>` | `default=` | `not-null` |
| `add-not-null` | `<t>.<c>` | | |
| `add-index` | `<t> (<cols>)` | `name=` `using=` `where=` | `unique` |
| `drop-index` | `<name>` | | |
| `add-fk` | `<t>.<c> -> <rt>.<rc>` | `name=` `on-delete=` | |
| `add-check` | `<t> <name> '<expr>'` | | |
| `drop-column` | `<t>.<c>` | | |
| `revert` | | | |

`rename-column`, `rename-table` and `drop-table` have no directive: a safe rename needs the application to read
either name during the transition, which pgroll buys with versioned views over the physical table. godwit's unit is
a versioned SQL file and it will not start owning the application's view of the schema; `add-column` + `backfill` +
`drop-column` expresses the same change in three reviewable migrations behind the expand/contract gate.

`batch=`, `key=` and `pause=` are the knobs of a [batched statement](#batched-statements): the cursor column, the
rows per batch and the sleep between them.

`keep-old=` defaults to `true`: a `change-type` leaves `<c>_old` in place as the rollback, and a later migration
removes it with `-- godwit: drop-column <t>.<c>_old`. `keep-old=false` drops it in the contract phase and makes
that phase irreversible; the plan says so. The default is per target (`godwit target add --keep-old=false`, stored
as the `keep_old` target config key) and a directive's own `keep-old=` still wins over it.

**Parsed offline, expanded on the plan.** Loading a migration parses its directives with no database in sight:
the operation must be known, the arity right, the option names known, durations and integers parseable, and
`<table>.<column>` well formed. That is exactly what `godwit lint` runs, and a failure is **`E004`**. Turning a
directive into statements is a separate stage on the control plane, against the scratch database that already has
the target's history replayed — the expansion needs the primary key, the column type, `relkind` and function
volatility, and `internal/engine` is offline by design. That is also why the H004 recipe has to name `id` as its
batching key while an expansion reads the real one.

The file checksum is unchanged: `Checksum` stays `sha256(UpSQL)` of the file as committed, and the plan key stays a
pure function of the files and the pending set. The expansion is derived, never committed.

**Where a directive may not appear.** `E004` covers placement as well as syntax:

- in an `R__` repeatable — a repeatable re-applies on every content change, and a phased directive cannot;
- in a `.down.sql`, except the lone `-- godwit: revert` sentinel, which asks for the generated inverse. A down side
  is either hand-written SQL or that sentinel, never both.

**Recipes point at them.** Every hazard whose safe form a directive can express prints it as the recipe's first
line, so the reader can copy either the SQL or the one-liner:

```
-- or let godwit run it: -- godwit: add-not-null users.email
ALTER TABLE users ADD CONSTRAINT email_not_null CHECK (email IS NOT NULL) NOT VALID;
ALTER TABLE users VALIDATE CONSTRAINT email_not_null;
ALTER TABLE users ALTER COLUMN email SET NOT NULL;
ALTER TABLE users DROP CONSTRAINT email_not_null;
```

H001, H003, H004, H005, H006, H007 and H009 carry the full equivalent. H010 carries `-- or let godwit run the index
build:` instead, because `add-index ... unique` builds the concurrent index but the `ADD CONSTRAINT ... USING INDEX`
that follows it stays yours. A recipe whose statement the grammar cannot express — a multi-column foreign key, an
index with an ordering or an operator class, a `DROP INDEX` naming several indexes, an `ADD COLUMN` carrying more
than `NOT NULL` — prints no directive line rather than a lossy one.

### The expansion

`Validator.Validate` expands every directive migration in turn, against the scratch database the plans before it
have already touched, and replaces the plan with `BuildPlan` over the expanded body. `Effects`, `Fingerprints`,
`Detect` and already-applied detection then describe the real effect for free.

`-- godwit: change-type public.users.age bigint` on a table whose `age` is `NOT NULL` and whose primary key is
`id bigint` becomes:

```sql
-- expand
ALTER TABLE public.users ADD COLUMN age_new bigint;
CREATE FUNCTION public.users_age_sync() RETURNS trigger LANGUAGE plpgsql AS $godwit$
  BEGIN SELECT age::bigint INTO new.age_new FROM (SELECT new.*) AS users; RETURN new; END $godwit$;
CREATE TRIGGER users_age_sync BEFORE INSERT OR UPDATE ON public.users
  FOR EACH ROW EXECUTE FUNCTION public.users_age_sync();
WITH b AS (SELECT id AS godwit_key FROM public.users
           WHERE id > $1::bigint AND (age_new IS DISTINCT FROM age::bigint) ORDER BY id LIMIT 5000)
UPDATE public.users AS t SET age_new = age::bigint FROM b WHERE t.id = b.godwit_key RETURNING b.godwit_key;
ALTER TABLE public.users ADD CONSTRAINT users_age_new_not_null CHECK (age_new IS NOT NULL) NOT VALID;
ALTER TABLE public.users VALIDATE CONSTRAINT users_age_new_not_null;
-- contract
DROP TRIGGER users_age_sync ON public.users;
DROP FUNCTION public.users_age_sync();
ALTER TABLE public.users RENAME COLUMN age TO age_old;
ALTER TABLE public.users RENAME COLUMN age_new TO age;
ALTER TABLE public.users ALTER COLUMN age SET NOT NULL;
ALTER TABLE public.users DROP CONSTRAINT users_age_new_not_null;
```

The trigger is what makes the window safe: it exists before the first batch and is dropped after the last, so a
write that lands during the backfill sets both columns and the backfill's `IS DISTINCT FROM` predicate skips it.
The `UPDATE` is one plan statement with a `BatchSpec`, not N unrolled ones: the executor loops over it, commits the
cursor with the rows, and resumes from the cursor after a crash. `$1::bigint` is explicit because a key narrower
than `bigint` would otherwise refuse the `int8` the executor binds.

The other operations are the cheap half of the same machinery — no trigger, no batches except where a value has
to be filled in, and only `drop-column` produces a contract statement:

| Op | Expands into | Phase |
|---|---|---|
| `add-not-null <t>.<c>` | `ADD CONSTRAINT <c>_not_null CHECK (<c> IS NOT NULL) NOT VALID` → `VALIDATE CONSTRAINT` → `SET NOT NULL` → `DROP CONSTRAINT`. Only the `VALIDATE` reads the rows, and it does so under a lock that lets writes through. | expand |
| `add-column <t>.<c> <type>` | `ADD COLUMN` nullable, then `ALTER COLUMN <c> SET DEFAULT` when `default=` is given. With `not-null`, a batched backfill of the rows that already exist and then the `add-not-null` block; `not-null` without `default=` is refused. | expand |
| `add-index <t> (<cols>)` | `DROP INDEX CONCURRENTLY IF EXISTS` when an **invalid** index of that name is left over from an interrupted build, then `CREATE [UNIQUE] INDEX CONCURRENTLY`. The name is `name=` or `<t>_<cols>_idx`, the same one the H001 recipe prints. | expand |
| `drop-index <name>` | `DROP INDEX CONCURRENTLY IF EXISTS`, so a retry after an interrupted drop is a no-op. | expand |
| `add-fk <t>.<c> -> <rt>.<rc>` | `ADD CONSTRAINT <name> FOREIGN KEY … NOT VALID` → `VALIDATE CONSTRAINT`. The name is `name=` or `<t>_<c>_fkey`. | expand |
| `add-check <t> <name> '<expr>'` | `ADD CONSTRAINT <name> CHECK (<expr>) NOT VALID` → `VALIDATE CONSTRAINT`. | expand |
| `drop-column <t>.<c>` | `ALTER TABLE … DROP COLUMN`, in the **contract** phase — so the run parks at `awaiting_contract` until a human confirms, and `rollout: direct` is refused. | contract |

The generated names are the ones the #40 recipes already print, so a hazard's recipe and the directive that
replaces it produce the same schema. `add-not-null` and `add-column`'s `not-null` reuse a CHECK that already says
the column is not null instead of adding a second one, and drop it afterwards only when it carries godwit's own
`<c>_not_null` name — a constraint someone else wrote is validated, used and left alone.

`-- godwit: revert` generates the inverse for every operation but `drop-index`, `drop-column` and `backfill`,
which have none that is lossless.

Expand statements are spliced where the directive stood; contract statements are appended at the **end** of the
body, so the contract phase is always a suffix and `Plan.HoldFrom` can name a single index. The generated
statements carry no hazards: godwit wrote them, and the hazard gate speaks about what the author wrote.

**The expansion is frozen.** It is stored on `cp_plans.expansions` and on `cp_runs.expansions`, and the scheduler
substitutes the run's expansion for the file bodies before planning: the run applies byte for byte what the pull
request showed. The plan key stays a pure function of the files, but `shape()` carries the expansion hash, so a
re-plan whose expansion changed — a column appeared, the primary key moved — fails `SameStatements` and refuses
with `PlanStale{history}` at bind. `godwit.migrations` records the checksum of the **file**, never of the expansion.

**A directive is expanded once.** The validator replays the target's history on the scratch database before it
looks at the submitted files, and a migration that replay already carried is left exactly as its own run left it:
no fresh expansion, no entry in the plan's `expansions`, no statements. Otherwise every plan, `migrate --dry-run`
and `migrate` on the target would be re-computing the recipe against a catalog that already holds what the
migration created, and every refusal in the table below — `<c>_old already exists`, `already NOT NULL`, `does not
exist in the schema this migration starts from` — would fire forever on a migration nobody is going to run again.
The same holds for the executor: a migration the target has recorded is skipped whatever its body says. A target
that has *not* applied it yet expands it as usual, so the same directory can be pending on one target and history
on another.

**A directive does not need a stored plan.** In an implicit run the expansion is computed at admission through the
same code path and recorded in the run's audit detail and notification (`expands <id> <hash>`). `require_plan` on
the target still applies as usual.

**A directive that produces two phases forces the rollout.** `rollout: direct` on such a plan is refused with
`<id> expands into expand and contract phases; use rollout: expand-contract` — the rollout is part of the plan key,
and godwit will not silently upgrade what the reader approved.

**A target mid-rollout is not plannable.** Between the phases the schema matches no recorded state, so `PlanRun`
and `CreateRun` refuse with `target <t> has run <id> awaiting contract; confirm or revert it first`.

**Never already applied.** The expanded body carries DML (the backfill) and objects a snapshot cannot read back
(the trigger and its function), so `Plan.Opaque()` already stops `Detect`'s prefix walk. That is the right answer
for the right reason; there is no special case for it.

**Progress.** The scheduler writes the newest statement event to `cp_runs.progress` under the heartbeat, so
`godwit runs`, `godwit run get` and the UI show `backfill 320000/~1240000 rows (batch 64)` while it runs. A run
that backfills for an hour notifies once, not once per batch.

**Revert.** `-- godwit: revert` asks for the generated inverse, and godwit stores two: the pre-swap one for a run
parked at `awaiting_contract` (`DROP TRIGGER IF EXISTS`, `DROP FUNCTION IF EXISTS`, `DROP CONSTRAINT IF EXISTS`,
`DROP COLUMN IF EXISTS` — idempotent from any point in the expand phase) and the post-swap one for a completed run
(rename back, drop the new column). Both are lossless because `age_old` is still there. Reverting a migration
parked between its phases has no `godwit.migrations` row to key on, so the executor looks for the still-open up run
instead, and discards its journal once the down has finished. A run that failed *inside* the contract phase is a
needs-attention case for a human, not something the generated down can guess at.

**Retired columns.** A completed `change-type` records `<t>.<c>_old` in `cp_retired_columns`, with the run that
retired it. `godwit diff` takes the drop of a retired column out of the generated `up_sql`, so an ORM that never
knew about the column stops proposing to drop it on every pull request; a revert forgets the row again.

### What the expander refuses

Every refusal is `invalid_argument` from `PlanRun`, before anything is stored, naming the directive line.

| Case | Reason |
|---|---|
| the table or column does not exist in the schema the migration starts from | the expansion is computed before the migration's own SQL runs |
| the relation is partitioned (`relkind = 'p'`) or is not an ordinary table | the swap would have to run per partition |
| identity or generated column | the sequence or expression stays bound to the physical attribute across the rename |
| the column takes part in a foreign key, either side | after the swap the constraint still points at the renamed physical column |
| `<c>_new` or `<c>_old` already exists on the table | the expansion would collide |
| no single-column primary key and no `key=` | nothing to batch on; the message names the option |
| `key=` that does not exist, is nullable, or has no single-column unique btree index | a cursor over it can skip or repeat rows |
| `key=` whose type is not integer, `uuid` or text | the cursor cannot be carried between batches |
| `using=` calling a function `pg_proc` reports as `VOLATILE` | the trigger and the backfill would disagree |
| `using=` containing a subquery or a column of another table | the trigger form `SELECT expr INTO new.c_new FROM (SELECT new.*) AS t` cannot express it |
| two directives naming the same subject in one migration | ambiguous order |
| a directive in a migration whose own SQL carries H002, H003 or H008 | the contract block is a suffix, so the destructive statement would run in the expand phase; split them |
| a directive that splits a statement in two | a directive sits between whole statements |
| `add-not-null` on a column that is already `NOT NULL` | the migration would do nothing |
| `add-column` naming a column the table already has | the expansion would collide |
| `add-column … not-null` with no `default=` | nothing would fill the rows that already exist |
| `add-index` whose name is taken by a valid index, or by anything that is not an index | only an **invalid** leftover is cleared automatically |
| `drop-index` naming something that is not an index, or an index that backs a constraint | drop the constraint instead |
| `add-fk` pointing at a column with no single-column unique index | PostgreSQL cannot point a foreign key at it |
| `add-fk` or `add-check` whose constraint name the table already carries | pass `name=` to choose another |
| `-- godwit: revert` with `keep-old=false`, or against a `backfill`, a `drop-index` or a `drop-column` | there is no lossless inverse; write the `.down.sql` by hand |
| `skip_validation` with a directive migration still to apply | no scratch, no catalog, no expansion; one the target already holds passes, it is never expanded again |
| a directive in a repeatable, or in a `.down.sql` beyond the sentinel | `E004`, above |

## Repeatable migrations

A file pair named `R__<snake_name>.up.sql` / `R__<snake_name>.down.sql` has no version. It is meant for objects that are declared rather than migrated — a view, a function, a trigger body — where the file *is* the desired state and `CREATE OR REPLACE` makes re-running it safe.

**When it runs.** Repeatables are ordered after every versioned migration of the run, among themselves by name. On each run the executor compares the file's `sha256` with the checksum recorded for that name in `godwit.repeatables`: equal → skipped, and the plan shows the migration as `unchanged`; different or absent → the up side runs and the row is upserted with the new checksum and `applied_at`. Nothing about a repeatable ever enters `godwit.migrations`, so the version-keyed history stays exactly what it was.

**Crash safety** is the versioned one, unchanged: the journal is per statement, and a run row is opened per `(repeatable, checksum, direction)`. Killing a replica halfway resumes from the last `done` statement of that run. Editing the file after a crash produces a different checksum and therefore a different run, so the resume never has to reconcile a journal against statements it no longer matches.

**The plan contract** treats a repeatable like any other pending file. Its content is part of the plan key, so editing the file makes an existing plan stop covering the set (`PlanRequired` on a `require_plan` target); its recorded checksum is part of the observation's `history_hash`, so a repeatable re-recorded on the target by something other than a run refuses the bind with `PlanStale{history}`. Scratch validation replays repeatables with the history, in the same order, so already-applied detection and `godwit diff` see the same schema the target has.

**Hazards apply unchanged**: a repeatable is still DDL and goes through the same gate, the same acknowledgement and the same `expand-contract` split. `lint` accepts the filename and reports the same codes, with one exception: `E003` ("migration modified after merge") never fires on an `R__` file — editing it in place is the point.

**Down.** The `.down.sql` is required, like a versioned one, and it is used only when the run that applied the repeatable is reverted; then it runs and the `godwit.repeatables` row is deleted. godwit does not store previous file bodies, so reverting a run that *re-applied* a repeatable drops the object rather than restoring the body it had before — write the down side as `DROP ... IF EXISTS`, and roll forward by editing the file when you want the previous content back.

## Rollout policies

| Policy | Behaviour |
|---|---|
| `direct` (default) | every plan runs in the expand phase; the run ends `succeeded` |
| `expand-contract` | statements up to the first contract statement run now; that statement and everything after it are held; the run ends `awaiting_contract` (or `succeeded` when nothing was held). `ConfirmRollout` re-queues it with `phase = contract`; the executor skips the already-applied plans and runs the rest. |

The split is by statement, and a migration whose statements carry no phase of their own has a single phase. A statement belongs to the contract phase when it says so (`Statement.Phase`, which only a directive expansion sets today) or, failing that, when its migration carries a contract hazard — so a hand-written migration mixing `ADD COLUMN` and `DROP COLUMN` still lands in the contract phase whole, and only an expansion splits a migration down the middle.

On a pull request the GitHub Action makes the hold visible: an apply that ends `awaiting_contract` leaves the `godwit/applied` commit status at `pending` ("expand applied; comment `/godwit confirm` to run the contract phase"), so branch protection keeps the pull request unmergeable until `/godwit confirm` releases the same run and the status turns `success` ([CI/CD](ci-cd.md#pull-request-confirm-the-contract-phase)).

A migration split down the middle is **not** run twice. The expand phase stops at `Plan.HoldFrom`, the index of the first held statement, and returns without recording the migration: the target's `godwit.runs` row stays `running` and no `godwit.migrations` row appears. `ConfirmRollout` re-queues the same control-plane run with `phase = contract`, which rebuilds the plan with every statement and no hold; `openRun` finds that still-open target run, `loadProgress` checks the hash of each journalled statement against the rebuilt plan — the list is identical, only the hold differed — and execution resumes at `lastDone + 1` on the same run id before finalising it. A crash anywhere in either phase resumes through the same journal, with no extra state.

## Revert

`RevertRun{run_id}` queues a new run of kind `migrate` with `reverts` set, whose plans are the down sides of every file in the original run, newest version first. It goes through the same hazard gate (the `DROP TABLE` in a down file needs `--ack H002`), the same validation, the same lease. Allowed only when the original is `succeeded`, `awaiting_contract`, `failed` or `needs_attention`, is the newest non-reverted run on the target, and nothing is queued or running there. Baseline runs cannot be reverted. When the revert succeeds the original is marked `reverted` and its versions become pending again. The plan the original was bound to stays `bound` until the next `CreateRun` with the same key, which retires it (`superseded`) and starts fresh; a re-plan stores a new `ready` plan alongside it, and there is no plan state for "reverted" — the run carries that. A revert is all-or-nothing per run: there is no single-version revert out of a multi-file run.

## Drift

After every successful run (and after a baseline) the scheduler stores a **snapshot** of the target schema in `cp_snapshots`: columns of base tables, constraints, indexes, and an `md5` of each view definition, excluding `pg_catalog`, `information_schema` and godwit's own tables, sorted, with a `sha256` fingerprint.

The monitor fingerprints every snapshotted target every `--drift-interval` (5m). A different fingerprint opens a `cp_drift_events` row with the diff (`- expected` / `+ live` lines), notifies, and logs `schema drift detected`; a matching one resolves any open event and notifies `resolved`. A partial unique index keeps one open event per target and diff, so replicas ticking together record once. `CheckDrift` runs the same comparison on demand; `AcceptBaseline` snapshots the live schema as the new baseline and resolves the open events.

## Baseline

`BaselineTarget{target, files, version}` adopts a database that already has a schema: every migration with `version <= N` is inserted into the target's `godwit.migrations` with its checksum, without running it, in one transaction under the advisory lock. The store records a `succeeded` run of kind `baseline` holding those files, so scratch validation of later runs replays them, and a drift snapshot is taken. Refused with `failed_precondition` when the target already has any applied version. The usual first file is a schema dump named like `20260101000000_baseline.up.sql` with an empty-effect down side.

## Timeouts

Every statement runs under `lock_timeout` (default 5s) and `statement_timeout` (default 0, disabled). Both can be set on the target at registration and overridden per run; the run value wins field by field, then the target's, then the default. Values are Go durations (`5s`, `2m`, `0`); a lock timeout below 1ms is refused. A statement that hits one fails the run with PostgreSQL's `55P03` (lock) or `57014` (statement) error, counted in `godwit_statement_failures_total{reason="lock_timeout"|"statement_timeout"}`.

## search_path

A target may declare a `search_path` (`godwit target add --search-path app,public`, `RegisterTarget.search_path`), and every session godwit opens on it carries that value as a connection parameter: the executor, the revert, `Observe`, `Snapshot`, `Diff`, and the scratch database validation replays on. Unqualified names in a migration then resolve where the application expects them instead of wherever the migration role's own default points. Unset, nothing changes: sessions keep the role's default, which is what earlier versions did.

The value is a comma-separated list of unquoted schema names, folded to lower case the way PostgreSQL folds identifiers. Quoted identifiers and `$user` are refused — a declared path is meant to be explicit, and `$user` resolves per role, which is how a schema named after the migration role silently captured unqualified tables in the first place. `godwit` is refused too: the journal lives there.

**The journal is never on the path.** `godwit.migrations`, `godwit.runs` and `godwit.journal` are schema-qualified in every statement godwit issues, so the search path cannot move them, and refusing `godwit` as a path element means a migration's unqualified `CREATE TABLE migrations` lands in the application's schema rather than colliding with the journal. `engine.Snapshot` keeps hiding those three tables from drift.

The schemas must exist. PostgreSQL silently drops missing ones from a session's effective path, so on a fresh target the first migration should `CREATE SCHEMA IF NOT EXISTS app` — from then on the path resolves fully. `godwit target status` prints the **declared** path and `godwit plan` the **effective** one (`current_schemas`); if they differ, a schema is missing. Scratch validation creates the schemas on the scratch database before setting the path, so the replay puts unqualified objects in the same schema the target does and fingerprints keep matching (which is what already-applied detection compares).

The effective path is part of a plan's observation. A plan taken under one path does not bind under another: the diff shows `- search_path <then>` / `+ search_path <now>` and the refusal is `PlanStale{schema}`. Plans stored before the path was recorded carry an empty value and are never stale for this reason alone.

## Plans

`PlanRun{persist}` stores the admitted plan in `cp_plans` / `cp_plan_files` together with an **observation** of the target at that moment: a `history_hash` over the live `godwit.migrations` (version and checksum, ascending) and `godwit.repeatables` (name and checksum, by name), the schema fingerprint and definition (`engine.Snapshot`), and the time. The **plan key** is `sha256` of the target, the rollout and the ordered *pending set* — the files whose version is not yet applied plus the repeatables whose content differs from what the target recorded, with their up and down checksums. It is a pure function of the files and of the target's history: not a git SHA (squash merges change it), not the plan id. One `ready` plan exists per `(target, key)`; re-planning the same set refreshes the row under a new id. An applied migration whose file body differs from the recorded checksum cannot be planned (`invalid_argument`) nor bound (`PlanStale{content}`).

`CreateRun` computes the key from its files and looks for a ready plan not older than `--plan-ttl`:

- **fresh** — history hash and schema fingerprint match the observation: the run binds to the plan (`plan_id` on the run and on the response), the plan becomes `bound`.
- **explained** — every migration added to the history since the plan came from a run that succeeded after the plan was created, nothing was removed except the old content of a repeatable that came back under a new checksum, and the live schema matches the baseline snapshot taken by the last run: the set is re-planned; if the statements are identical, the old plan is `superseded`, the new one bound, audit `plan.supersede`. A set that now falls below the newest applied version is `PlanStale{order}` unless `allow_out_of_order`; a set that no longer validates is `PlanStale{validation}`.
- **stale** — anything else: `failed_precondition` with a `PlanStale` detail (`reason` history / schema / order / validation / content, the added and removed versions, the `+`/`-` schema lines, a hint) before any row is written on the store or the target.

With no matching plan the run is admitted as today (implicit plan, empty `plan_id`) unless the target was registered with `require_plan` or the service runs with `--require-plan`: then `failed_precondition` with a `PlanRequired` detail naming the nearest stored plans and the difference between their files and the set.

`CreateRun{plan_id}` skips the key lookup and binds that plan (`godwit migrate --plan`): target, rollout and files come from the plan unless given, and given ones must agree with it; the plan must still be `ready`, younger than `--plan-ttl` and fresh or explained. Stored plans are readable through `GetPlan` / `ListPlans` (`godwit plan show`, `godwit plans`); `bound` and `superseded` plans older than `--plan-retention` are deleted by the drift ticker, except those of runs that have not finished, and the run's `plan_id` is cleared while its `run.create` audit entry keeps the id.

### Already-applied migrations

A persisted, validated plan compares the target with what the scratch database looked like after the history (`S_0`) and after each pending migration in turn (`S_1 … S_n`). When the target's fingerprint equals some `S_k`, migrations `1..k` had their effect applied by hand: each is reported with `already_applied` and its `effect` (the `+`/`-` snapshot lines it adds), and the run that binds to the plan **records** them in `godwit.migrations` with a `succeeded` run of `stmt_count 0` instead of executing them. Detection stops at the first migration it cannot vouch for, and a plan whose marks changed since it was taken is `PlanStale{history}` (re-plan). Before recording, the scheduler observes the target again: an unrecorded mark whose fingerprint no longer matches the plan fails the run with `target schema changed since plan ... was taken`, and any `INVALID` index (a `CREATE INDEX CONCURRENTLY` that failed halfway) fails it with `index ... exists but is INVALID`.

Only what the snapshot sees can be matched: columns of base tables, constraints, indexes and the `md5` of view definitions. Everything else is refused, and the plan says why in `note`:

| Situation | `note` | What to do |
|---|---|---|
| The migration has DML (`INSERT`, `UPDATE`, `DELETE`, `MERGE`, `COPY`, `TRUNCATE`, `SELECT`, `CALL`, `DO`) | `has DML, must execute` | Run it; data is never inferred from a schema. |
| The migration creates or alters something the snapshot cannot see (functions, types, triggers, policies, grants, sequences, tablespaces, partitions, identity or generated columns, collations, unlogged or temporary tables, view options…) or has no effect on the scratch schema | `effect not inspectable` | Run it, or baseline it explicitly. |
| The hand changes match the migration's effect but not as a prefix of the pending set (`S_k` never equals the target) | `effect is present but not as a prefix` and the difference in `drift` | Reorder or split the migrations so the hand-applied ones come first, or `godwit drift accept` and baseline them. |
| An applied migration's body differs from its checksum | `invalid_argument: ... applied with different content` | Restore the file. |
| `skip_validation` | no note; `drift` falls back to the last snapshot | Validate to detect. |

`S_0` is the scratch database after the history, not the target's drift baseline: a hand change blessed with `AcceptBaseline` still shows as `drift` in the plan and blocks detection until it is captured in a migration or a baseline.

## Generating migrations from a schema

`Diff{target, schema}` (`godwit diff`) turns a description of the whole database you want into the next migration. The **before** side is the target as the plan machinery observes it (`Observe`: applied versions, schema definition, `search_path`), not its recorded history. The **after** side is `schema`, applied as-is on an empty scratch database on the control-plane PostgreSQL with the target's `search_path`. The two are compared with [pg-schema-diff](https://github.com/stripe/pg-schema-diff) in both directions: live → desired is the `up` SQL, desired → live the `down` SQL. Every statement is classified by the same planner a run uses, so the response carries hazards and recipes, and `godwit diff` writes `<timestamp>_<name>.up.sql` / `.down.sql` in the migration directory. Equal schemas produce empty SQL and nothing is written.

Because the starting point is the live schema, hand changes that are not in the history become part of the generated migration. When validation is on, the response also carries `drift`: the `+`/`-` lines between the history replayed on a scratch database and the live target, so you can tell which part of the `up` captures drift and which part is new ([drift](#drift) explains the format). With `--skip-validation` on the service, `drift` is empty.

### Schema sources

The `schema` the service receives is always DDL; where it comes from is the client's business. `godwit diff` has one **schema source** per flag, each an implementation of `schemasource.Source` (`Load(ctx) (ddl, error)`), all of them running next to the repository with the project's own toolchain — the service never sees a Prisma schema, a Go package or a Django project, and never gains a Node, Go or Python dependency:

| Flag | `kind` | What it runs | Refuses |
|---|---|---|---|
| `--schema <file>` | `file` | nothing; the file is plain DDL | — |
| `--prisma <schema.prisma>` | `prisma` | `prisma migrate diff --from-empty --to-schema[-datamodel] <file> --script`, the flag chosen by the CLI's major version | a datasource provider other than `postgresql`, before running anything |
| `--exec '<command line>'` | `command` | the argv as given, stdout is the DDL | empty stdout |
| `--gorm <package>` | `gorm` | `go run <package>` | an empty stdout, with the compiler's stderr surfaced on a build failure |
| `--django <manage.py>` | `django` | `python manage.py showmigrations --plan --no-color`, then `python manage.py sqlmigrate <app> <name> --no-color` for every migration in plan order | a `DATABASES` `ENGINE` that is not PostgreSQL, before running anything |

`--exec` is the escape hatch: any command that prints the whole desired database on stdout. `--gorm` is a thin wrapper over it, because GORM's dry-run migrator is a Go API over your model structs, not a CLI: the package is yours, godwit only runs it and reports a build failure with the package name instead of `exit status 1` ([examples/gorm/schema/main.go](../examples/gorm/schema/main.go) is a copyable 20-line one). `--django` concatenates `sqlmigrate`'s output, dropping the `BEGIN;`/`COMMIT;` lines Django wraps an atomic migration in. **Django's constraint, documented rather than hidden:** `sqlmigrate` opens the configured connection to introspect, so `DATABASES` must point at a reachable PostgreSQL (`--django-database <alias>` picks which); teams for whom that does not hold use `--exec` with their own dump. `--go-bin`, `--python-bin` and `--prisma-bin` (and their `GODWIT_*_BIN` variables) name the interpreter when it is not on `PATH`; a missing one is reported as a godwit message, not as a bare `exec` error.

The source is also a property of the directory, not only of the command line: a `schema_source` block in `godwit.yaml` says which ORM the migrations next to it follow, and `godwit diff` falls back to it when no source flag is given ([configuration](configuration.md#godwityaml) has the keys). `schema_source.path` is resolved relative to the file that declares it, and `godwit.yaml` is looked up from the working directory upward, so a monorepo puts one next to each migration directory and every directory keeps its own source. The block is also what the lint check below compares the committed migrations against; the flags stay the override for a one-off diff.

### Keeping the generated SQL and the ORM schema together

A Prisma or GORM team edits the ORM schema, `godwit diff` writes the pair, both are committed. Nothing then stops someone from editing the ORM schema without regenerating, or hand-editing the generated `.sql`: the pull request looks fine and the two drift apart silently. `godwit lint` catches it.

The check cannot use the live target as the before side — the pending files are not applied there, so the diff would re-propose everything pending. `DiffRequest.base` therefore takes a second starting point:

| `base` | Before side | Reads |
|---|---|---|
| `DIFF_BASE_LIVE` (default) | the target as `Observe` sees it | the live database |
| `DIFF_BASE_FILES` | `DiffRequest.files` replayed on top of the target's recorded history, on a scratch database | the store and the files in the request |

With `base: files` the before side is `S_n`, the schema the committed files claim to produce, so `up_sql` is empty **exactly when** the committed migrations already express the ORM schema. Anything left is the residue, whatever caused it. The replay is the validator's own: the recorded history first (each run with the directive expansion *it* froze, never a fresh one), then the request's files, which the journal skips where the history already covers them.

`godwit lint --server <url> --target <t>` renders the declared source client-side, sends the whole directory as `files`, and reports the residue:

```
$ godwit lint --dir db/migrations --server https://godwit.internal --target orders
prisma/schema.prisma: error E005 the migration generated from prisma/schema.prisma is out of date
    ALTER TABLE "public"."users" ADD COLUMN "email" text;
1 finding(s), 1 blocking
```

`E005` blocks (exit 1) unless `schema_source.lint` is `false`, which makes it a warning. Without `--server` the check reports `W002` (`<path> not checked: no server configured`) and lint stays entirely offline — the local/CI parity the check is for is "same command, same config", not "same connectivity". `--no-schema-check` turns it off. The ORM itself always runs client-side, next to the repository: the service only ever receives DDL.

What the diff covers is what pg-schema-diff covers: schemas, extensions, enums, tables (columns with type, default, nullability, collation, identity and generated expression; check constraints; partitions; row-level security and policies; replica identity; table grants), primary and unique keys, foreign keys, indexes (`CREATE INDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`, online replacement of a changed index), sequences, functions, procedures, triggers, views and materialized views. Not covered: types other than enums (domains, composite types), exclusion constraints, comments, roles, grants on anything but tables; keep those in hand-written migrations. Index names in the output are unquoted; everything else is schema-qualified. The scratch database is empty apart from the target's `search_path`, so the schema must declare what it relies on (`CREATE SCHEMA`, `CREATE EXTENSION`) or qualify names; a schema that fails to apply is refused with `invalid_argument` and PostgreSQL's error. Data is never inferred: a column rename comes out as drop + add, a type change as `ALTER COLUMN ... TYPE`, both flagged by their hazards with the expand/contract recipe.

## Target status

`GetTargetStatus` reads `godwit.migrations` and `godwit.repeatables` on the live target without creating them (a never-migrated database reports nothing), compares against optional files (pending versions, `checksum_mismatch` when an applied migration's up file changed, repeatables listed as applied when their content matches and as pending when it does not), and adds the last run, the drift baseline (`taken_at`, the run that took it, whether drift is open), the provider and the registered timeouts. Repeatable rows carry `repeatable = true` and no version.

## Actors and provenance

Every token has a name; the name is the **actor** on the access log, on notifications, on `cp_runs.created_by` and on every `cp_audit` row. `CreateRun{source}` is free text stored on the run; the GitHub Action fills it with `<host>/<owner>/<repo>@<sha>[:<dir>]`. Every mutating RPC writes an audit row (`target.register`, `target.baseline`, `run.create`, `run.revert`, `run.resume`, `run.park`, `run.confirm`, `drift.accept`, `plan.create`, `plan.supersede`) after it succeeds; reads are not audited (`Diff` creates and drops a scratch database on the control plane but writes nothing in the store). `PlanRun{persist}` is the one `read`-scope call that writes: the plan and its `plan.create` row.
