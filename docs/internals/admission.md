# Admission

What a run must pass before anything is queued: the gate, the hazards and directives it reads, and the plan it binds to.

## Admission

`CreateRun`, `PlanRun` and `RevertRun` go through the same gate, in this order, before anything is queued:

1. **Target exists** — `not_found` otherwise.
2. **Out-of-order guard** — a pending version below the newest version in the target's history (the migrations any run applied to completion, migrate and baseline, that no revert undid — the state of the run that applied them does not come into it) is refused with `failed_precondition` unless `allow_out_of_order`; allowed ones are logged. Reverts skip this check, and repeatables carry no version so they are never out of order.
3. **Hazard gate** — every hazard code in the migrations this admission would *execute* must be in `acknowledge_hazards`; otherwise `failed_precondition` listing them. The plan report reads the gate's own verdict (`skipped` per migration), so it never counts a hazard the gate does not gate or asks for an `--ack` that would change nothing; a hazard on a statement that will not run stays in its table row, where its recipe hangs, and is reported separately from the count. The whole directory is submitted every time, so the gate reads only what is pending: a migration the target already holds contributes none, on the same predicate that makes it `applied` in the plan, and neither does a checkpoint that is only being recorded. A down plan is the exception — a revert runs it *because* the target holds it, so `DROP TABLE` in a down file still needs `--ack H002`.
4. **Scratch validation** — a database `godwit_validate_<id>` is created on the store server, every run of the target is replayed in order — the migrations that run applied to completion and no revert undid, in the order it applied them, each with the expansion frozen on its own `cp_run_applied` row, never the whole directory the run submitted — then the new plans one at a time, snapshotting the schema after the history and after each plan; the database is dropped `WITH (FORCE)`. A failure in the new plans is `invalid_argument: migration failed validation: ...`; a failure in the history replay is `internal: replay history run i: ...`. Skipped with `skip_validation` or `serve --skip-validation`. The snapshots feed [already-applied detection](#already-applied-migrations) when the plan is persisted.

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

Why the expansion runs where it does, and what it refuses: [decision 0002](../decisions/0002-directives-godwit-executes.md). Why a data condition is a directive and not a hook: [decision 0008](../decisions/0008-assertions-in-the-plan.md).

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
-- godwit: assert 'SELECT count(*) FROM orders WHERE total IS NULL' = 0
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
| `assert` | `'<select>' <cmp> <value>` | | |
| `revert` | | | |

`rename-column`, `rename-table` and `drop-table` have no directive: a safe rename needs the application to read
either name during the transition, which pgroll buys with versioned views over the physical table. godwit's unit is
a versioned SQL file and it will not start owning the application's view of the schema; `add-column` + `backfill` +
`drop-column` expresses the same change in three reviewable migrations behind the expand/contract gate.

`assert` is the one directive that reads rather than writes; it has a section of [its own](#assertions).

`batch=`, `key=` and `pause=` are the knobs of a [batched statement](journal.md#batched-statements): the cursor column, the
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

### Assertions

`-- godwit: assert '<select>' <cmp> <value>` states a condition about the **data** and makes it part of the plan.

```sql
-- godwit: assert 'SELECT count(*) FROM orders WHERE total IS NULL' = 0
-- godwit: assert 'SELECT count(*) FROM users' > 0
-- godwit: assert 'SELECT bool_and(email LIKE ''%@%'') FROM users' = true
```

The query is single-quoted, `''` is a literal quote inside it, and the comparison is `=`, `<>`, `!=`, `<`, `<=`, `>` or `>=` against an integer, or `=`/`<>` against `true`/`false`. There are no options and no flags: this is a condition, not a predicate language.

**It is a statement.** The assertion occupies the position in the file where it is written, gets a plan index, a hash and a journal row, and is rendered in `godwit plan`, in the pull-request comment and in the UI as a statement whose mode is `assert`, with its condition beside its SQL. Nothing runs outside what the pull request showed — which is the reason godwit has no SQL hooks.

**Where it runs.** Where you wrote it. Ahead of the migration's own SQL it is a **precondition** — the shape that guards a `DROP TABLE legacy` with `assert 'SELECT count(*) FROM legacy' = 0`, which is allowed precisely because an assertion generates no contract block of its own. After a `change-type` or a `backfill` it is the last statement of the **expand** phase, so a bad backfill can never become the irreversible swap: the generated contract block is always a suffix, so an assertion is always ahead of it. A run whose assertion does not hold fails before `awaiting_contract`, and there is then nothing to confirm. (Under `expand-contract` a hand-written destructive statement still moves its whole migration into the contract phase, assertion included — so the precondition above is checked when the human confirms, which is where it belongs.)

**A resume re-checks it.** The executor walks past an assertion again even when the journal says it is done — including the walk `ConfirmRollout` makes over the expand phase before it applies the contract statements. A condition that held an hour ago is not a condition that holds now, and re-running a `SELECT` costs nothing. The consequence is worth stating: an assertion whose subject the same migration changes must be written to stay true after the change, or placed after it. The one exception is a resume that has already begun the **contract** phase: there the columns an expansion's own assertion names have been renamed away, so it is past asking rather than re-asked, and the run finishes the statements it has left.

**What it refuses.** Offline, through libpg_query, exactly like every other directive value: anything that is not a single `SELECT` (`UPDATE`, `DELETE`, `CREATE`, a `DO` block), a `SELECT INTO`, a locking clause (`FOR UPDATE` takes row locks), a data-modifying CTE, and a query returning more than one column or a bare `*`. At run time the query executes in a **read-only transaction**, which is what stops a `VOLATILE` function that writes — volatility lives in the catalog and the offline check cannot see it. The value must be a single row of `smallint`/`integer`/`bigint` (for an integer comparison) or `boolean`; `sum()` returns `numeric`, so cast it. No rows, more than one row, or `NULL` fails the assertion by name.

**On the scratch database it is probed, not enforced.** Validation replays the target's history on a scratch database, which carries the schema and none of the rows, so `count(*)` there says nothing. The assertion is still executed: a table or column the query names but the schema does not have, and a column whose type the comparison cannot read, are refused at plan time — in the pull request. The row count and the value are checked on the target and only there.

**Failure.** The run ends `failed`, never `needs_attention`: the condition is deterministic, so a retry would fail identically, and the scheduler does not retry it. The error names the query, the value it got and the value it wanted — `assertion failed: SELECT count(*) FROM orders WHERE total IS NULL returned 3, want = 0` — and rides on the run row, so the pull-request comment, `godwit run get`, the UI and the Slack/webhook notification all carry it. Nothing after the assertion ran, and the migration is not recorded.

**What it does not interact with.** A migration carrying an assertion is never marked `already_applied`: a `SELECT` is DML, so `Plan.Opaque()` already stops the prefix walk, and marking it applied would skip the check. `-- godwit: revert` generates no inverse for an assertion (there is nothing to undo) and refuses outright when the assertion is the migration's only directive, rather than leaving an empty down body. A migration withheld by `--to` does not run its assertion, because it does not run. An assertion may not appear in an `R__` repeatable or in a `.down.sql`: that is `E004`, the placement rule every directive shares.

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
SELECT count(*) FROM public.users WHERE age_new IS DISTINCT FROM age::bigint;
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

**One predicate, three consumers.** `<c>_new IS DISTINCT FROM <expr>` is what the batches select, what the
trigger's assignment makes false for every row it is handed, and what the closing `SELECT count(*)` asks about, so
the three cannot disagree about what "converged" means. That count is a generated [assertion](#assertions), `= 0`,
and it is the **last statement of the expand phase** — the statement the run has to pass before `awaiting_contract`
and, because a resumed run re-evaluates an assertion it walks past, the statement `godwit confirm` has to pass
again before the rename. The rename is the irreversible half of a `change-type`, so nothing else in the expansion
is worth checking: a `using=` that does not converge — one whose value depends on something outside the row that
moved while the batches ran — used to leave the batches' own rows behind, report `succeeded` and then swap the
column anyway. It now fails the run with `assertion failed: … returned <n>, want = 0`, with `<c>` untouched and
the old column still there.

Once the contract phase *has* begun, the assertion is past asking rather than re-asked: the swap renames the
columns it names, so a run that died between two contract statements resumes into what is left instead of failing
on a schema that no longer answers the question.

#### `backfill` keeps its rows in sync while it runs

A batched `UPDATE` on its own is not write-safe. It walks the key space once: a row written **below** the cursor
after the cursor passed it is never looked at again, and a row appended after the last batch is never seen. The
run reports `succeeded` and the rows are stale. Measured under a live write workload, that was 34 402 rows of
2 000 000 ([testing](testing.md#a-backfill-under-a-live-write-workload)).

So `backfill` gets the same guarantee `change-type` has, from the same device.
`-- godwit: backfill users set='age_new = age::bigint' where='age_new IS NULL' key=id batch=5000` becomes:

```sql
-- expand
CREATE FUNCTION public.users_backfill_sync() RETURNS trigger LANGUAGE plpgsql AS $godwit$
  BEGIN SELECT age::bigint INTO new.age_new FROM (SELECT new.*) AS users; RETURN new; END $godwit$;
CREATE TRIGGER users_backfill_sync BEFORE INSERT OR UPDATE ON public.users FOR EACH ROW
  WHEN ((new.age_new IS NULL) AND (ROW(new.age_new) IS DISTINCT FROM ROW(new.age::bigint)))
  EXECUTE FUNCTION public.users_backfill_sync();
WITH b AS (SELECT id AS godwit_key FROM public.users
           WHERE id > $1::bigint AND ((age_new IS NULL) AND (ROW(age_new) IS DISTINCT FROM ROW(age::bigint)))
           ORDER BY id LIMIT 5000)
UPDATE public.users AS t SET age_new = age::bigint FROM b WHERE t.id = b.godwit_key RETURNING b.godwit_key;
SELECT count(*) FROM public.users WHERE (age_new IS NULL) AND (ROW(age_new) IS DISTINCT FROM ROW(age::bigint));
DROP TRIGGER users_backfill_sync ON public.users;
DROP FUNCTION public.users_backfill_sync();
```

**One predicate, three uses.** `(<where>) AND (ROW(<columns>) IS DISTINCT FROM ROW(<expressions>))` is what the
batches select, what the trigger fires on and what the closing count asks about, so the three cannot disagree
about what "backfilled" means. The `where=` alone is not enough: it is the author's filter, and after a
`set='norm = lower(name)' where='true'` every row still matches it. The distinctness half is what makes the
statement idempotent — a row the trigger already fixed is skipped rather than written twice — which is also what
makes an ambiguous cursor safe on resume.

**The guard lives in the trigger's `WHEN` clause**, not in the function body. The backfill's own batches fire the
trigger for every row they touch, and a `WHEN` clause is evaluated by the executor without entering plpgsql. Over
ten million rows the body form cost 1.9× the run; the `WHEN` clause costs about 6%
([testing](testing.md#a-batched-backfill-over-10-000-000-rows)).

**The closing `SELECT count(*)` is an [assertion](#assertions) godwit generates**, `= 0`, and it is the same
mechanism `-- godwit: assert` uses — journalled, read-only, and fatal without a retry. It runs **before** the
trigger is dropped, so a row written while it is being checked is still covered; a row written after the `DROP
TRIGGER` is past the migration and is not the backfill's business. This is what turns a silent 34 402 into a
failed run, and it is a `count(*)` over the table: on a very large one it needs a `statement_timeout` that allows
for it.

**A crash leaves the trigger, on purpose.** The trigger is created by statement 0 and dropped by statement 4 of
the same phase, and the journal resumes at the statement it stopped on — so a resume neither re-creates it nor
skips the rows written while the run was dead: the trigger was still there, keeping them in sync. Dropping it
when a run fails would reintroduce exactly the bug this fixes, because a run in `needs_attention` is one a human
may resume. A run that is abandoned rather than resumed leaves `<t>_backfill_sync` behind; the plan's notes name
it and the two statements that remove it.

**There is no contract phase to drop it in.** A plain `backfill` has one phase, so the trigger's whole life is
inside it — which is also why it composes with `ConfirmRollout` without a special case: a `backfill` beside a
`change-type` in one migration has its trigger created and dropped in the expand phase, long before the hold.

The other operations are the cheap half of the same machinery — no trigger, no batches except where a value has
to be filled in, and only `drop-column` produces a contract statement:

| Op | Expands into | Phase |
|---|---|---|
| `add-not-null <t>.<c>` | `ADD CONSTRAINT <c>_not_null CHECK (<c> IS NOT NULL) NOT VALID` → `VALIDATE CONSTRAINT` → `SET NOT NULL` → `DROP CONSTRAINT`. Only the `VALIDATE` reads the rows, and it does so under a lock that lets writes through. | expand |
| `add-column <t>.<c> <type>` | `ADD COLUMN` nullable, then `ALTER COLUMN <c> SET DEFAULT` when `default=` is given. With `not-null`, a batched backfill of the rows that already exist and then the `add-not-null` block; `not-null` without `default=` is refused. It needs no sync trigger: the `SET DEFAULT` runs before the batches, so a row written during them already carries the value, and the `VALIDATE CONSTRAINT` at the end fails loudly on anything missed. | expand |
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
on another — and a reverted migration is pending again, so it is expanded again, against the catalog the revert
left behind.

**A directive does not need a stored plan.** In an implicit run the expansion is computed at admission through the
same code path and recorded in the run's audit detail and notification (`expands <id> <hash>`). `require_plan` on
the target still applies as usual.

**A directive that produces two phases forces the rollout.** `rollout: direct` on such a plan is refused with
`<id> expands into expand and contract phases; use rollout: expand-contract` — the rollout is part of the plan key,
and godwit will not silently upgrade what the reader approved.

**A target mid-rollout is not plannable.** Between the phases the schema matches no recorded state, so `PlanRun`
and `CreateRun` refuse with `target <t> has run <id> awaiting contract; confirm or revert it first`.

**Never already applied.** The expanded body carries DML (the backfill), so `Plan.Opaque()` already stops
`Detect`'s prefix walk. That is the right answer for the right reason; there is no special case for it.

**Progress.** The scheduler writes the newest statement event to `cp_runs.progress` under the heartbeat, so
`godwit runs`, `godwit run get` and the UI show `backfill 320000/~1240000 rows (batch 64)` while it runs. A run
that backfills for an hour notifies once, not once per batch. Every transition that starts or ends an attempt —
claim, finish, retry, resume, confirm — clears the column, so a progress value always describes work in flight;
a run that is not running has none, and what it did is in the ledger and the journal.

**Revert.** `-- godwit: revert` asks for the generated inverse, and godwit stores two: the pre-swap one for a run
parked at `awaiting_contract` (`DROP TRIGGER IF EXISTS`, `DROP FUNCTION IF EXISTS`, `DROP CONSTRAINT IF EXISTS`,
`DROP COLUMN IF EXISTS` — idempotent from any point in the expand phase) and the post-swap one for a completed run
(rename back, drop the new column). Both are lossless because `age_old` is still there. Reverting a migration
parked between its phases has no `godwit.migrations` row to key on, so the executor looks for the still-open up run
instead, and discards its journal once the down has finished. A run that failed *inside* the contract phase is a
needs-attention case for a human, not something the generated down can guess at.

**Retired columns.** A completed `change-type` records `<t>.<c>_old` in `cp_retired_columns`, with the run that
retired it. `godwit diff` takes the drop of a retired column out of the generated `up_sql`, so an ORM that never
knew about the column stops proposing to drop it on every pull request. A revert forgets the row again, and so does the
`-- godwit: drop-column` that finally removes the column: the rollback it recorded is gone with it.

**Dependent objects follow the rename, so the expander refuses them.** The swap is two `RENAME COLUMN` statements,
and PostgreSQL moves every dependency with the *physical* attribute rather than with the name. A view, index,
constraint, trigger, policy or publication built on `<c>` therefore ends up silently reading `<c>_old`, still the old
type, with no error at any point. The first symptom is usually the dependent object's own migration failing much
later, for instance a repeatable view whose body no longer replaces:

```
statement 0 of R__order_stats (up): exec: ERROR: cannot change data type of view column "customer_id" from bigint to text (SQLSTATE 42P16)
```

So `change-type` reads `pg_depend` for the column and refuses when anything at all is bound to it, naming what it
found:

```
godwit directive on line 1 (-- godwit: change-type orders.customer_id text): view public.order_stats depends on
public.orders.customer_id; the swap renames the column and PostgreSQL moves every dependent with the physical
attribute, so each one would silently keep reading public.orders.customer_id_old. Drop and recreate them around
this migration, in their own migrations
```

Every kind `pg_depend` records is covered: views and materialised views, indexes (plain, expression and partial),
primary key, unique, check and exclusion constraints, foreign keys from other tables, `UPDATE OF` triggers,
generated columns elsewhere on the table, rules, publication column lists, row security policies, extended
statistics objects, and a sequence `OWNED BY` the column.

**What is not a dependent object.** The column's own `DEFAULT` does not survive a rename either — it stays on the
retired attribute — but nothing else is involved, so the expansion carries it over rather than refusing: the expand
phase adds `ALTER TABLE <t> ALTER COLUMN <c>_new SET DEFAULT <expr>` right after the `ADD COLUMN`, and a plan note
says so. An expression that is not valid for the new type fails on the scratch during validation, in the pull
request, not mid-run. A `COMMENT ON COLUMN` is *not* carried over; it stays on `<c>_old`. Neither is a `plpgsql`
function body that names the column: PostgreSQL records no dependency for it, so godwit cannot see it and cannot
refuse it. Grep for the column name in your functions before retyping it.

`drop-column` has the same problem in two different shapes, and refuses both. An object PostgreSQL would
*auto*-drop with the column (index, unique/check/exclusion constraint, extended statistics, owned sequence) goes
silently — fine when it exists only for that column, which is what a drop means, but a loss when it also covers
other columns, so a multi-column one is refused. An object with a normal dependency (view, materialised view,
foreign key from another table, trigger, generated column, rule, publication, policy) makes `DROP COLUMN` fail
outright — which, in the contract phase, means the run explodes *after* a human confirmed the rollout — so it is
refused at plan time instead.

### What the expander refuses

Every refusal is `invalid_argument` from `PlanRun`, before anything is stored, naming the directive line.

| Case | Reason |
|---|---|
| the table or column does not exist in the schema the migration starts from | the expansion is computed before the migration's own SQL runs |
| the relation is partitioned (`relkind = 'p'`) or is not an ordinary table | the swap would have to run per partition |
| identity or generated column | the sequence or expression stays bound to the physical attribute across the rename |
| the column takes part in a foreign key, either side | after the swap the constraint still points at the renamed physical column |
| `change-type` on a column anything in `pg_depend` is bound to — view, materialised view, index, primary key, unique, check or exclusion constraint, foreign key from another table, `UPDATE OF` trigger, generated column, rule, publication column list, row security policy, statistics object, owned sequence | a rename moves dependencies with the physical attribute, so every one of them would silently keep reading `<c>_old`; the refusal names each object it found |
| `drop-column` on a column a view, materialised view, foreign key, trigger, generated column, rule, publication or policy depends on | PostgreSQL refuses the `DROP COLUMN` itself, and the contract phase runs only after a human confirmed the rollout |
| `drop-column` on a column whose index, constraint or statistics object also covers other columns | the drop takes it silently, and the other columns lose what it gave them; replace it first |
| `<c>_new` or `<c>_old` already exists on the table | the expansion would collide |
| no single-column primary key and no `key=` | nothing to batch on; the message names the option |
| `key=` that does not exist, is nullable, or has no single-column unique btree index | a cursor over it can skip or repeat rows |
| `key=` whose type is not integer, `uuid` or text | the cursor cannot be carried between batches |
| `using=`, a `backfill`'s `set=` or its `where=` calling a function `pg_proc` reports as `VOLATILE` | the trigger and the batches would disagree |
| any of the three containing a subquery or a column of another table | the trigger form `SELECT expr INTO new.c FROM (SELECT new.*) AS t` cannot express it, and its `WHEN` clause cannot hold a subquery |
| `backfill` whose `set=` reads a column the same `set=` assigns (`set='v = v + 1'`) | applying it twice does not mean the same as applying it once, and the trigger and the batches would both apply it |
| `backfill` whose `set=` assigns the batching key | a row that moves under the cursor is skipped or repeated |
| `backfill` whose `set=` assigns a column the schema does not have, or one whose type has no equality operator (`json`, `xml`) | the run's guarantee is a count of the rows still matching, and that count is unaskable without `=`; cast it, or use `jsonb` |
| `backfill` whose `set=` writes into an element or field (`set='tags[1] = …'`), assigns `DEFAULT`, or uses the multi-column form `set='(a, b) = (…)'` | the trigger assigns whole columns, one at a time, from an expression it can also compare against |
| `backfill` on a table that already carries a trigger or a function named `<t>_backfill_sync` | the expansion would collide; a leftover from an abandoned run is named rather than discovered when the `CREATE` fails |
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
| `-- godwit: revert` on a migration whose only directives are assertions | an assertion has nothing to undo, and an empty down body is not one |
| an `assert` whose query is not a single read-only `SELECT` of one column | `E004`, offline: it would write, lock rows, or return something the comparison cannot read |
| an `assert` naming a table or column the migration's starting schema does not have | the scratch replay executes it, so a typo is refused in the pull request |
| `skip_validation` with a directive migration still to apply | no scratch, no catalog, no expansion; one the target already holds passes, it is never expanded again |
| a directive in a repeatable, or in a `.down.sql` beyond the sentinel | `E004`, above |

## Plans

Why a plan is a contract at all: [decision 0001](../decisions/0001-plan-as-contract.md).

`PlanRun{persist}` stores the admitted plan in `cp_plans` / `cp_plan_files` together with an **observation** of the target at that moment: a `history_hash` over the live `godwit.migrations` (version and checksum, ascending) and `godwit.repeatables` (name and checksum, by name), the schema fingerprint and definition (`engine.Snapshot`), and the time. The **plan key** is `sha256` of the target, the rollout and the ordered *pending set* — the files whose version is not yet applied plus the repeatables whose content differs from what the target recorded, with their up and down checksums. It is a pure function of the files and of the target's history: not a git SHA (squash merges change it), not the plan id. One `ready` plan exists per `(target, key)`; re-planning the same set refreshes the row under a new id. An applied migration whose file body differs from the recorded checksum cannot be planned (`invalid_argument`) nor bound (`PlanStale{content}`).

`CreateRun` computes the key from its files and looks for a ready plan not older than `--plan-ttl`:

- **fresh** — history hash and schema fingerprint match the observation: the run binds to the plan (`plan_id` on the run and on the response), the plan becomes `bound`.
- **explained** — every migration added to the history since the plan came from a run that succeeded after the plan was created, nothing was removed except the old content of a repeatable that came back under a new checksum, and the live schema matches the baseline snapshot taken by the last run: the set is re-planned; if the statements are identical, the old plan is `superseded`, the new one bound, audit `plan.supersede`. A set that now falls below the newest applied version is `PlanStale{order}` unless `allow_out_of_order`; a set that no longer validates is `PlanStale{validation}`.
- **stale** — anything else: `failed_precondition` with a `PlanStale` detail (`reason` history / schema / order / validation / content, the added and removed versions, the `+`/`-` schema lines, a hint) before any row is written on the store or the target.

With no matching plan the run is admitted as today (implicit plan, empty `plan_id`) unless the target was registered with `require_plan` or the service runs with `--require-plan`: then `failed_precondition` with a `PlanRequired` detail naming the nearest stored plans and the difference between their files and the set.

`CreateRun{plan_id}` skips the key lookup and binds that plan (`godwit migrate --plan`): target, rollout and files come from the plan unless given, and given ones must agree with it; the plan must still be `ready`, younger than `--plan-ttl` and fresh or explained. Stored plans are readable through `GetPlan` / `ListPlans` (`godwit plan show`, `godwit plans`); `bound` and `superseded` plans older than `--plan-retention` are deleted by the drift ticker, except those of runs that have not finished, and the run's `plan_id` is cleared while its `run.create` audit entry keeps the id.

### Already-applied migrations

A persisted, validated plan compares the target with what the scratch database looked like after the history (`S_0`) and after each pending migration in turn (`S_1 … S_n`). When the target's fingerprint equals some `S_k`, migrations `1..k` had their effect applied by hand: each is reported with `already_applied` and its `effect` (the `+`/`-` snapshot lines it adds), and the run that binds to the plan **records** them in `godwit.migrations` with a `succeeded` run of `stmt_count 0` instead of executing them. Detection stops at the first migration it cannot vouch for, and a plan whose marks changed since it was taken is `PlanStale{history}` (re-plan). Before recording, the scheduler observes the target again: an unrecorded mark whose fingerprint no longer matches the plan fails the run with `target schema changed since plan ... was taken`, and any `INVALID` index (a `CREATE INDEX CONCURRENTLY` that failed halfway) fails it with `index ... exists but is INVALID`.

Only what the snapshot sees can be matched ([drift](drift.md#drift) lists it). Everything else is refused, and the plan says why in `note`:

| Situation | `note` | What to do |
|---|---|---|
| The migration has DML (`INSERT`, `UPDATE`, `DELETE`, `MERGE`, `COPY`, `TRUNCATE`, `SELECT`, `CALL`, `DO`) | `has DML, must execute` | Run it; data is never inferred from a schema. |
| The migration creates or alters something the snapshot cannot see (policies, grants, comments, extensions, composite or domain types, tablespaces, collations, unlogged or temporary tables, view options, a function's `SET` or `LEAKPROOF`…) or has no effect on the scratch schema | `effect not inspectable` | Run it, or baseline it explicitly. |
| The hand changes match the migration's effect but not as a prefix of the pending set (`S_k` never equals the target) | `effect is present but not as a prefix` and the difference in `drift` | Reorder or split the migrations so the hand-applied ones come first, or `godwit drift accept` the schema change and adopt the migrations with `godwit target adopt`. |
| An applied migration's body differs from its checksum | `invalid_argument: ... applied with different content` | Restore the file. |
| `skip_validation` | no note; `drift` falls back to the last snapshot | Validate to detect. |

`S_0` is the scratch database after the history, not the target's drift baseline: a hand change blessed with `AcceptBaseline` still shows as `drift` in the plan and blocks detection until it is captured in a migration or a baseline.

