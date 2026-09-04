# 0002 — `-- godwit:` directives: the migration declares intent, godwit writes the lock-safe SQL

Shipped in #51 (per-statement hold), #52 (batched statements), #53 (grammar and lint), #56 (`change-type`, `backfill`), #57 (the simple operations), #59 (expand once), #69 (dependent objects). Backfill progress became visible in #70.

## The open question

A hazard recipe hands over the lock-safe SQL as text and the author copies it (#40). That text has to guess: the planner is offline, so it hardcodes `id` as the batching key and `$1`/`$2` as placeholders. The question was whether godwit should *be* the safe SQL — and if so, **where the expansion happens**, because that choice decides whether a reviewer can see what will run.

## The decision

A migration may carry `-- godwit: <op> …` lines. They are **parsed offline at load time** and **expanded against a catalog at plan time**, and the expansion is frozen before anything runs.

```sql
-- godwit: change-type users.age bigint using='age::bigint' batch=5000 pause=100ms
-- godwit: add-index users (email) where='deleted_at IS NULL'
-- godwit: add-fk orders.user_id -> users.id on-delete=cascade
```

Nine operations expand today: `change-type`, `backfill`, `add-column`, `add-not-null`, `add-index`, `drop-index`, `add-fk`, `add-check`, `drop-column`. A tenth, `revert`, is a `.down.sql` sentinel asking for the generated inverse.

**Two stages, deliberately split.**

1. **Parse** — `internal/engine/directive.go`, offline, at load time. Purely syntactic. Loading is strict and the error is typed: a `-- godwit: change-tpe` typo is `E004`, not an inert comment that would let the migration apply unexpanded. Values are validated by feeding them through libpg_query (`SELECT <v>`, `SELECT NULL::<type>`, `UPDATE t SET <v>`) rather than a regex, so quoting, casts and expressions come out right for free. The parser is a small SQL scanner, not a line match, so a `-- godwit:` inside a string literal, a dollar-quoted body or a block comment is genuinely not a directive — and a trailing one after a statement on the same line is an error rather than silently ignored.
2. **Expand** — `internal/controlplane/expand.go`, inside `Validator.Validate`, on the scratch database that already has the target's history replayed and its `search_path` mirrored. That connection is the exact catalog the migration will meet, it is deterministic (derived from files and history, not from live drift), and it already exists.

**Why not in the loader.** The expansion needs the primary key, the column type and nullability, `relkind`, identity and generated flags, foreign-key membership and `pg_proc.provolatile`. `internal/engine` is offline by design — that is precisely why the recipe has to hardcode `id`.

**Why not in the scheduler.** The reviewer must see the exact SQL before it runs. This is the same posture as already-applied marking: the plan is the contract, and a scheduler-side expansion would let the applied statements differ from the ones on the pull request.

## The machinery it needed

- **A hold inside one migration** (#51). `ExpandContract.Split` used to split *between* migrations; a `change-type` puts expand and contract statements in one. `engine.Plan` gained `HoldFrom int` — the index of the first statement not to execute, `0` meaning "run everything", so every existing caller is unchanged. `Executor.apply` runs up to it and returns without finalising; the target's `godwit.runs` row stays `running` with no `godwit.migrations` row. `ConfirmRollout` re-queues the same run with `phase = contract`, `PlansFromFiles` rebuilds the identical statement list with `HoldFrom = 0`, `openRun` finds the still-open row, and `loadProgress` resumes at `HoldFrom`. No new state, and the journal is reused exactly.

  *Rejected: slicing the plan into two plans.* Statement indices are the journal's primary key; a sliced contract plan would journal statement 2 as statement 0 and break `loadProgress`'s "statement *i* changed since run *X* started" invariant.

- **A batched statement with a journalled cursor** (#52). A backfill is **one** plan statement — the rendered single batch with the cursor as `$1` — plus a `BatchSpec`, not N unrolled statements, because N depends on a row count nobody knows at plan time. `godwit.journal` gained `cursor`, `rows_done`, `rows_total`. Each batch commits the cursor **in its own transaction, with its own rows**, so the journal is exactly true; a kill leaves the last committed batch's cursor and the in-flight batch rolls back with its rows untouched. The `IS DISTINCT FROM` predicate makes re-running a batch a no-op, so an ambiguous cursor is safe either way. No dirty flag, no repair command.

- **Freezing.** The expansion lands on `cp_plans.expansions` *and* on `cp_runs.expansions`, and the scheduler reads from the run. `shape()` carries the expansion hash, so a re-plan whose expansion changed (a column appeared, the primary key moved) fails `SameStatements` and refuses at bind. The **plan key stays a pure function of the files and the pending set** — a catalog-dependent key would make the same pull request produce different keys on different targets.

## The two owner decisions that changed the design

Both reversed what the design document specified.

1. **`change-type` keeps the old column, and godwit records that it did.** `<c>_old` is not dropped; it is the rollback, and a later `-- godwit: drop-column` removes it. That is what makes revert lossless and it is the same expand→contract discipline the hazard gate teaches. To stop `godwit diff` proposing to drop it on every ORM pull request, a completed `change-type` writes the column into `cp_retired_columns` and the diff suppresses its drop. `keep-old=false` drops it in the contract phase and makes that phase irreversible; the plan says so.

   **Divergence:** the decision asked for the default to be overridable "per directive and as a project/target default in `godwit.yaml`". What shipped is per directive and per target (`godwit target add --keep-old=false`, the `keep_old` target config key). There is **no `godwit.yaml` key**. The argument for stopping there is the one used for `schema_source`: the service is what expands, so a target-level default cannot disagree between a local `godwit plan` and CI. Adding the project key is additive and has not been done.

2. **Directives run without a stored plan.** The design made a stored plan mandatory for any file carrying a directive, which would have broken `apply-on-merge` and ArgoCD PreSync for teams that never adopted `command: plan`. Instead the expansion is computed at admission through the same code path, recorded in the run's audit detail and the notification, and frozen on `cp_runs.expansions` — which is why the run, not the plan, is what the scheduler reads. `require_plan` on the target still applies as usual.

## What the expander refuses

Every refusal is `invalid_argument` from `PlanRun`, before anything is stored, naming the directive line. `docs/concepts.md` carries the full table. The shape of it:

- **The catalog cannot support the operation** — no single-column primary key and no `key=`; a `key=` that is nullable or has no single-column unique btree index, or whose type is not integer/`uuid`/text; a partitioned or non-ordinary table; an identity or generated column; a `<c>_new`/`<c>_old` collision.
- **The generated SQL would be wrong** — a `using=` calling a `VOLATILE` function (the trigger and the backfill would disagree), or one with a subquery or another table's column (the trigger form cannot express it).
- **The plan would not be reviewable** — `skip_validation` (no scratch, no catalog); `rollout: direct` on a two-phase expansion (the rollout is in the plan key and godwit will not upgrade it silently); a target with a run in `awaiting_contract` (its schema matches no recorded state); a directive beside H002/H003/H008 in the same file (the contract block is a suffix, so that statement would run in the expand phase).
- **Other objects depend on the column** — see below.

## The dependents refusal (#69), and why it inverts the usual argument

A `change-type` on a column a view reads broke the view **silently**. The contract phase renames `<c>` → `<c>_old` and `<c>_new` → `<c>`, and PostgreSQL moves every dependency with the *physical attribute*, not with the name — so the view stayed bound to `<c>_old`, still the old type, and nothing errored. A reader following the documentation hit it within ten minutes.

Worth stating plainly: **the naive statement was safer.** `ALTER TABLE users ALTER COLUMN age TYPE bigint` with a view on `age` is refused loudly by PostgreSQL. The expand/contract dance replaced a loud refusal with silent corruption.

The refusal is not a guessed list. One dependent of each kind was built on PostgreSQL 17, the two renames were run, and `pg_depend`, `pg_get_viewdef`, `pg_get_indexdef`, `pg_get_constraintdef`, `pg_get_triggerdef`, `pg_rules`, `pg_policies`, `pg_publication_rel` and `pg_get_statisticsobjdef` were read back. **17 of 17 followed the rename; not one was refused by PostgreSQL.** Two more are invisible to `pg_depend` and were found the same way: the column's own `DEFAULT` and its `COMMENT` both stay on `<c>_old`. A `plpgsql` body naming the column records no dependency at all — undetectable, documented, not refused.

So `change-type` refuses **any** dependent: a rename breaks all seventeen kinds identically, and there is no line to draw. `drop-column` refuses two shapes rather than all — a *normal* dependent (PostgreSQL will refuse the `DROP COLUMN` itself, and `drop-column` is the contract phase, so that failure would land after a human confirmed the rollout), and an *auto* dependent that also covers other columns (a multi-column index or check silently takes the other columns' guarantees with it). An auto dependent covering this column alone is allowed — that is what a drop means. The `deptype` split predicts PostgreSQL's own behaviour exactly, 13 of 13, so `droppable` uses it instead of a hand-maintained list of kinds.

**One thing was automated: the column's own `DEFAULT`.** The expand phase now carries it to `<c>_new`, and it is validated on the scratch, so a default invalid for the new type fails in the pull request. It is the only case where nothing but the column is involved.

**Nothing else was.** `CREATE OR REPLACE VIEW` after the swap does not work — it returns the same `cannot change data type of view column` error the reader reported, because `REPLACE` cannot change a column's type, which is the entire point of `change-type`. The only route is `DROP VIEW` + `CREATE VIEW`, which cascades, drops grants and takes an `ACCESS EXCLUSIVE` lock — none of which the author asked for. Recreating an index is mechanical only for a plain btree. The rejected alternative in general shape is *a dependency-rewriting engine that drops and recreates other people's objects during a type change*; a precise refusal the author can act on is worth more than half of that.

## The bug that taught the most (#59)

Every refusal in the table above is a statement about a migration **that is about to run**. Applied to one that already ran, each of them is guaranteed to fire — and `admit` hands the *whole directory* to `Validator.Validate`, which expanded every plan carrying a directive, including migrations applied months ago, against a catalog that already holds what they created.

So once a `change-type` completed, the target was bricked: every later `plan`, `verify` and `migrate` on it was refused, forever, with `public.users.age_old already exists`. All eight operations reproduced it. Baselining hit the same wall from the other side, because a baseline records files with no expansion and the replay then reached a directive body with no statements.

The fix is consistent with the mechanism rather than an exception beside it: the history replay already carries the expansion each run froze, so `Validate` collects the migration ids the replay covered and leaves those plans exactly as their own run left them. The executor's unexpanded-body guard moved *after* the recorded/held skips — a migration the target has recorded never runs its body, so its body is not that run's business. `--skip-validation` narrowed to what is pending.

The frozen expansion is deliberately **not** re-attached to the applied plan, tempting as it is: a `change-type`'s expansion carries contract statements, and re-attaching them would make `checkRollout` refuse the next plain `godwit migrate` and let `ExpandContract.Split` hold every migration after one that finished long ago. An applied migration plans as what it is — history, zero statements.

## Refused or deferred

| Thing | Verdict | Reason |
|---|---|---|
| `rename-column`, `rename-table`, `drop-table` directives | deferred | A safe rename needs the application to read either name during the transition. pgroll buys that with versioned views over the physical table; godwit's unit is a versioned SQL file and it will not start owning the application's view of the schema. `add-column` + `backfill` + `drop-column` expresses the same change in three reviewable migrations. |
| Hazards on generated statements | refused | `checkHazards` runs on the file bodies, so the author is gated on what the author wrote. The expansion's own `RENAME COLUMN` and `DROP COLUMN` are godwit's safe recipe; acking them would train people to ack H008 by reflex. |
| Marking a directive migration `already_applied` | refused, and it falls out | The expanded body contains DML (the backfill) and non-inspectable objects (trigger, function), so `Plan.Opaque()` already stops the prefix walk. Correct for the right reason; documented rather than special-cased. |
| Splicing contract statements where the directive stood | refused | `HoldFrom` names one index, so the contract phase must be a suffix. Teaching `Split` to hold from the earliest of (first contract statement, first destructive statement) would change the split of every existing hand-written plan. |
| A generated `.down.sql` for `drop-column`, `drop-index`, `backfill`, or `change-type keep-old=false` | refused | No lossless inverse exists. A hand-written down always wins and is never substituted. |
| One down body with a `DO $$ … $$` catalog probe | refused | Correct, but the reviewer would be reading a string instead of SQL. Two bodies are stored — the pre-swap inverse and the post-swap one — and the ledger row picks by what that migration actually did. |
| A `batch_size` target config key | not built | The design specified one; `internal/controlplane/timeouts.go` defines `lock_timeout`, `statement_timeout`, `require_plan` and `keep_old` only. `batch=` per directive is the only knob. |

## Where the design document was overtaken

- **The UI does not render backfill progress** — that was written as if true. It was not: `Run.progress` carried the right shape but never moved during a backfill, because `Executor.execStatement` reports once, *after* the statement returns, and a batched statement is one statement. Measured against a real service: 200 000 rows, 40 seconds of polling, the same value every time, naming the `CREATE TRIGGER` before the loop. #70 added `StatementEvent.Partial`, an emit after every committed batch, and a one-second throttle in the scheduler, then rendered it. The estimate is `pg_class.reltuples` and the pages say so — `~200,000`, `≈41%`, never a bare number, and no rate or ETA, because deriving one honestly needs a start time `Run.created_at` is not.
- **`$1` is not bare.** The rendered batch uses `$1::bigint` (or the key's own cast) because the executor binds a typed value per `KeyKind`; a key narrower than `bigint` would otherwise refuse it. A missing cast fails loudly rather than a sentinel silently skipping rows.
- **The `change-type` expansion is 6 expand and 6 contract statements**, not the 5/6 sketched, and it carries the column's `DEFAULT` (#69).
- **`add-column` never puts the default inline.** `ADD COLUMN c type DEFAULT <expr>` is metadata-only in PostgreSQL 11+ *only* for a non-volatile expression; a `now()` default rewrites the table under `ACCESS EXCLUSIVE`. The column is added nullable, `SET DEFAULT` follows as its own metadata-only statement, and existing rows are filled by the batched update. `not-null` without `default=` is refused rather than guessed at.
