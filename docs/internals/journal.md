# The journal

What godwit writes into a target database, and what survives a crash: the two databases, the statement model, the journal protocol, repeatables, checkpoints, timeouts and `search_path`.

## Two databases

| | Lives in | Owned by | Holds |
|---|---|---|---|
| **Journal** | every target database, schema `godwit` | the executor (engine) | `migrations` (applied versions), `repeatables` (the content last applied under each repeatable name), `runs` (one row per migration × direction attempt), `journal` (one `intent`/`done` row per statement, carrying the cursor and row counts of a batched one) |
| **Store** | the service's own database (`--store-dsn`), default schema | the control plane | `cp_targets`, `cp_runs`, `cp_run_files`, `cp_leases`, `cp_snapshots`, `cp_drift_events`, `cp_notifications`, `cp_audit` |

The journal is the truth about what happened on a target. The store is the truth about who asked for what, who is executing it, and what the schema looked like last time. The store's own schema is applied with the same executor at `serve` start-up, so the store database also carries a `godwit` schema tracking the control-plane migrations.

**When the two disagree about what a target has applied, the target's journal is the fact** ([decision 0014](../decisions/0014-the-target-journal-is-authoritative.md)). It is written in the same transaction as the DDL and it survives losing the store; `cp_run_applied` is the control plane's copy of it, kept so the applied set, the out-of-order guard, the scratch replay and `godwit targets` can be answered without connecting to any target, and carrying what the journal has no room for: which run applied a migration, when, under whose token, with which frozen expansion, undone by which revert. Bringing the copy back into agreement is [adoption](drift.md#adopting-an-existing-database).

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
2. Bootstrap the `godwit` schema if missing: one transaction, under `pg_advisory_xact_lock(fnv64a("godwit:bootstrap"))`, because `CREATE ... IF NOT EXISTS` checks for the object before it locks it and two sessions creating the same object both pass that check.
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

There is no dirty flag and no repair command: the next attempt reads the journal and continues. The control plane's job is to make sure there is a next attempt ([leases](runs.md#leases)).

## Repeatable migrations

A file pair named `R__<snake_name>.up.sql` / `R__<snake_name>.down.sql` has no version. It is meant for objects that are declared rather than migrated — a view, a function, a trigger body — where the file *is* the desired state and `CREATE OR REPLACE` makes re-running it safe.

**When it runs.** Repeatables are ordered after every versioned migration of the run, among themselves by name. On each run the executor compares the file's `sha256` with the checksum recorded for that name in `godwit.repeatables`: equal → skipped, and the plan shows the migration as `unchanged`; different or absent → the up side runs and the row is upserted with the new checksum and `applied_at`. Nothing about a repeatable ever enters `godwit.migrations`, so the version-keyed history stays exactly what it was.

**Crash safety** is the versioned one, unchanged: the journal is per statement, and a run row is opened per `(repeatable, checksum, direction)`. Killing a replica halfway resumes from the last `done` statement of that run. Editing the file after a crash produces a different checksum and therefore a different run, so the resume never has to reconcile a journal against statements it no longer matches.

**The plan contract** treats a repeatable like any other pending file. Its content is part of the plan key, so editing the file makes an existing plan stop covering the set (`PlanRequired` on a `require_plan` target); its recorded checksum is part of the observation's `history_hash`, so a repeatable re-recorded on the target by something other than a run refuses the bind with `PlanStale{history}`. Scratch validation replays repeatables with the history, in the same order, so already-applied detection and `godwit diff` see the same schema the target has.

**`godwit diff` reads them too.** The desired schema a diff is measured against is the ORM's DDL *plus* every `R__` file in the directory, so what a repeatable declares is never proposed as a drop; a diff whose request carries no directory at all is refused instead ([generating migrations from a schema](drift.md#generating-migrations-from-a-schema) has the rules).

**Hazards apply unchanged**: a repeatable is still DDL and goes through the same gate, the same acknowledgement and the same `expand-contract` split. `lint` accepts the filename and reports the same codes, with one exception: `E003` ("migration modified after merge") never fires on an `R__` file — editing it in place is the point.

**Down.** The `.down.sql` is required, like a versioned one, and it is used only when the run whose ledger holds the repeatable is reverted; then it runs and the `godwit.repeatables` row is deleted. godwit does not store previous file bodies, so reverting a run that *re-applied* a repeatable drops the object rather than restoring the body it had before — write the down side as `DROP ... IF EXISTS`, and roll forward by editing the file when you want the previous content back.

## Checkpoints

Every plan replays the target's whole recorded history on a scratch database before it is admitted ([admission](admission.md#admission)). On a directory that has been accumulating for years that replay is the slowest thing godwit does, and it grows with every merge. A **checkpoint** collapses the history up to a version into one file: the replay executes that file and skips everything below it.

**A checkpoint is a migration file, not a row in the store.** It travels with the repository, it is reviewed in the pull request that adds it, it is part of the plan key and of what `godwit diff` and `godwit lint` compare against, and a target godwit has never seen can be built from the directory alone. A row in the control plane would be none of those things.

Its shape is Atlas's: an ordinary versioned file whose first line is a directive.

```sql
-- godwit: checkpoint through=20260430120000
-- 137 migrations, 20260101000000_init through 20260430120000_orders_index.
-- A target that has applied any of them records this file; one with no history runs it instead of them.

CREATE TABLE public.users (...);
CREATE INDEX CONCURRENTLY users_email_idx ON public.users (email);
...
```

`through=` names the newest version the body accounts for; it must be below the checkpoint's own version. **There is no `.down.sql`** — the loader requires one for every other migration and refuses one here, because an inverse for a hundred collapsed migrations would be a file nobody has run.

### Generating one

```
godwit checkpoint --name squash              # collapse the whole directory
godwit checkpoint --name squash --at 20260430120000
```

The service replays the versioned migrations at or below `--at` (the newest by default) on a scratch database, expanding any `-- godwit:` directive among them against the catalog the ones before it left, and renders the resulting schema as DDL with the same engine `godwit diff` uses. It then applies that DDL alone on a second scratch database and **refuses the checkpoint unless the schema fingerprint comes out identical** to the one the migrations produced. A generated file is worth exactly what a replay of it is, so the replay is part of generating it.

It is generated from a **scratch replay of the files**, never from a live target. Dumping a target would bake that target's drift — a hand-made `ALTER`, a column someone added at 3am — into the repository, and every other target would then be told it is missing it.

**The body is rendered for the database it will meet.** A checkpoint only ever runs on an empty scratch database or on a target with no history — never on one holding rows, readers or writers — so the online shape the DDL generator produces for a live table is pure cost. Indexes are rendered **without `CONCURRENTLY`**, so every statement runs inside a transaction rather than committing on its own, and the `CREATE UNIQUE INDEX` / `ALTER TABLE … ADD CONSTRAINT … USING INDEX` pair that every primary key and unique constraint would otherwise become is folded back into its `CREATE TABLE`. On a thousand single-table migrations that is one statement per table instead of three. Anything the fold cannot reproduce exactly — a partial index, an expression, a non-`btree` method, a deferrable constraint — is left as the generator wrote it, and the fingerprint check covers the difference either way.

For the same reason **a checkpoint's statements raise no hazards**. `godwit lint` gates `CREATE INDEX` without `CONCURRENTLY`, `DROP TABLE`, `ADD CONSTRAINT` without a prepared index and the rest on every other migration; each of those is about a table that already holds rows, readers or writers, and a checkpoint runs on a database with none of the three.

**Unqualified DDL lands in `public`.** Generation has no target whose `search_path` it could mirror, so the schema is pinned rather than resolved from the scratch role's name: `"$user"` otherwise resolves to whatever schema shares that name, and when that is godwit's own journal schema the render excludes it and the checkpoint comes out empty. Migrations that build objects in another schema have to name it, as they do to be portable at all.

The file is written into the migration directory with a fresh timestamp (one above the newest file when the directory is stamped in the future, so it always sorts last), and `--dry-run` prints it instead. Commit it; the collapsed files stay where they are.

### What each database does with it

The decision is a pure function of the files and of what the target has applied, so it is taken again at plan time, at apply time and inside the scratch replay, and can never go stale on a stored plan.

| The target has applied | The checkpoint | Everything it collapses |
|---|---|---|
| nothing | **runs** | recorded without running, in the same run |
| everything the checkpoint collapses | recorded without running | already applied |
| some of them (mid-history) | recorded without running, after the rest have run | the missing ones run from their own files, in order |

The fresh case is Atlas's rule — a new database starts from the checkpoint and skips what is below it — with one addition godwit needs: the collapsed migrations are **recorded** in `godwit.migrations` as the checkpoint runs, so the target's history is the same set of versions an old target has and the next `godwit plan` finds nothing pending below the checkpoint. Recording without running is the same `MarkOnly` path an adoption at a version and already-applied detection use.

**A target mid-history is not a special case**: the migrations between where it stopped and `through=` are simply pending, they run from their own files as they always did, and the checkpoint is recorded once the target reaches it. It only breaks if those files are gone from the directory, and then godwit refuses by name rather than guessing:

```
checkpoint 20260501000000_squash collapses history through 20260430120000, the newest applied
version is 20260301000000 and 20260430120000 is not in the migration directory; restore the
migrations below the checkpoint, or baseline at it
```

### What the replay does with it

`Store.History` returns what the target applied and no revert undid, oldest first. The replay looks for the newest row that is a checkpoint, executes that one first, drops every versioned row at or below its `through=`, and records those on the scratch database in one statement. Everything else keeps its order. So both an old target (which recorded the checkpoint) and a new one (which ran it) replay the same single file, and the two scratch databases come out with the same fingerprint.

The collapsed migrations are still counted as **replayed**, which is what the rest of the machinery consumes:

- **already-applied detection** ([already-applied migrations](admission.md#already-applied-migrations)) walks the fingerprints after the checkpoint, from a base that already holds everything below it;
- **directive expansion** is frozen once ([directives](admission.md#directives)): a `change-type` under the checkpoint is in the replayed set, so it is never expanded a second time — and its expansion is baked into the checkpoint's body, not left as a directive;
- **`godwit diff --base files`** and the **ORM drift gate** build their base through the same replay, so they get the short one too;
- **the ledger** ([revert](runs.md#revert)) is untouched: the checkpoint is a row like any other, and the collapsed rows stay exactly where they were.

**Repeatables are never collapsed.** A repeatable's identity is its body, it has no version, and the checkpoint's body is generated from the versioned migrations alone — so no `R__` object is inside it, and every repeatable in the history replays on top of the checkpoint as it always did. This is the reason to keep views, functions and triggers in `R__` files: they survive a checkpoint untouched.

### What is lost, on purpose

- **Nothing at or below a checkpoint can be reverted.** `godwit revert` refuses a run whose standing ledger holds the checkpoint itself (`it is a checkpoint, and a checkpoint has no inverse`) or any migration the checkpoint collapsed (`checkpoint <id> collapsed it: the target's history below version <v> cannot be reverted`). The reason is not squeamishness: on a target that started from the checkpoint those migrations never ran, their down files were written against states that target never passed through, and the replay would rebuild them from the checkpoint's body anyway — so a revert would "succeed" and leave permanent drift. `godwit down --version <v>` refuses the checkpoint offline for the same reason.
- **Data a collapsed migration inserted is not in the checkpoint.** The body is schema only, as in Atlas. A history whose migrations seed rows needs those `INSERT`s added to the checkpoint by hand, or kept above it with `--at`.
- **Anything the DDL generator cannot express is refused, not dropped.** The generated body has the same holes as `godwit diff` (domains, composite types, exclusion constraints, comments, roles — see [generating migrations from a schema](drift.md#generating-migrations-from-a-schema)), and the fingerprint check turns each of them into a refusal at generation time instead of a silent loss at apply time.
- **The collapsed files are still needed.** godwit does not delete them and neither should you until every target has passed the checkpoint: they are what carries a target that stopped below it.

## Timeouts

Every statement runs under `lock_timeout` (default 5s) and `statement_timeout` (default 0, disabled). Both can be set on the target at registration and overridden per run; the run value wins field by field, then the target's, then the default. Values are Go durations (`5s`, `2m`, `0`); a lock timeout below 1ms is refused. A statement that hits one fails the run with PostgreSQL's `55P03` (lock) or `57014` (statement) error, counted in `godwit_statement_failures_total{reason="lock_timeout"|"statement_timeout"}`.

## search_path

A target may declare a `search_path` (`godwit target add --search-path app,public`, `RegisterTarget.search_path`), and every session godwit opens on it carries that value as a connection parameter: the executor, the revert, `Observe`, `Snapshot`, `Diff`, and the scratch database validation replays on. Unqualified names in a migration then resolve where the application expects them instead of wherever the migration role's own default points. Unset, nothing changes: sessions keep the role's default, which is what earlier versions did.

The value is a comma-separated list of unquoted schema names, folded to lower case the way PostgreSQL folds identifiers. Quoted identifiers and `$user` are refused — a declared path is meant to be explicit, and `$user` resolves per role, which is how a schema named after the migration role silently captured unqualified tables in the first place. `godwit` is refused too: the journal lives there.

**The journal is never on the path.** `godwit.migrations`, `godwit.runs` and `godwit.journal` are schema-qualified in every statement godwit issues, so the search path cannot move them, and refusing `godwit` as a path element means a migration's unqualified `CREATE TABLE migrations` lands in the application's schema rather than colliding with the journal. `engine.Snapshot` keeps hiding those three tables from drift.

The schemas must exist. PostgreSQL silently drops missing ones from a session's effective path, so on a fresh target the first migration should `CREATE SCHEMA IF NOT EXISTS app` — from then on the path resolves fully. `godwit target status` prints the **declared** path and `godwit plan` the **effective** one (`current_schemas`); if they differ, a schema is missing. Scratch validation creates the schemas on the scratch database before setting the path, so the replay puts unqualified objects in the same schema the target does and fingerprints keep matching (which is what already-applied detection compares).

**A target that reaches the journal schema anyway is refused.** Declaring `godwit` is refused above, but a target can arrive there without declaring anything: PostgreSQL's default path is `"$user", public`, godwit creates schema `godwit` on every target it migrates, and a target connected with a role of that name therefore resolves unqualified names *into the journal schema*. `Observe` reads the effective path (`current_schemas`) and the session's `search_path` setting together with `current_user`, and refuses the target when either resolves to the journal schema — before a plan, a run, a diff or an adoption touches it:

```
the target's search_path reaches godwit's journal schema: element "$user" resolves to schema "godwit"
under role "godwit", so a migration's unqualified CREATE TABLE would land beside the journal's own
tables and out of drift's sight; give the target a search_path of its own (--search-path public), or
connect it with a role not named godwit
```

The setting is read as well as the effective path because the refusal has to arrive before the first run rather than after it: on a target godwit has not bootstrapped yet the schema does not exist, PostgreSQL drops it from the effective path, and the run that creates it is the same one that would put the migration's tables inside it. Declaring a path is the whole fix, and it holds for every session, because the declared value is pinned on the DSN and `"$user"` is never resolved again.

**Stripping the journal schema out of the observed path is not the fix**, which is why the refusal exists at all: on such a target the tables really are in `godwit`, so a validation replay that put them in `public` would produce a fingerprint the target can never match, and already-applied detection — which compares exactly those fingerprints — would be silently off. Nor can the path be corrected on the way in: godwit pins a path as a connection parameter, one choke point for every session it opens, and it cannot know what to strip until it has connected.

**A scratch session never resolves `"$user"`.** It carries the target's effective path when there is one to mirror, and `public` when there is not — a call that plans without observing the target, and the checkpoint generator, which has no target at all. PostgreSQL's default `"$user", public` is never left to resolve there, because on a scratch role named `godwit` — which is the role the quickstart creates — `"$user"` is the journal schema godwit puts on every scratch database, and an unqualified `CREATE TABLE orders` would replay into it instead of `public`.

The effective path is part of a plan's observation. A plan taken under one path does not bind under another: the diff shows `- search_path <then>` / `+ search_path <now>` and the refusal is `PlanStale{schema}`. Plans stored before the path was recorded carry an empty value and are never stale for this reason alone.

