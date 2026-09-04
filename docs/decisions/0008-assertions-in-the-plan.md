# 0008 — A data condition belongs in the plan, not in a hook

Shipped in #82.

## The open question

godwit verifies that DDL *worked*: the journal says which statements committed, `InvalidIndexes` catches a `CREATE INDEX CONCURRENTLY` that left an invalid index behind, `VALIDATE CONSTRAINT` proves a `NOT VALID` check holds, and a `change-type`'s trigger plus `IS DISTINCT FROM` predicate make the backfill converge. It verifies nothing about the **data**. "Does this column match the format we promised?", "do these two tables have the same count after the backfill?" — godwit had no way to ask, and no way to stop on the answer.

The obvious shape is a **SQL hook**: a file, or a config key, naming SQL to run before or after a migration. The comparison table has said "no SQL hooks" since the first release, with "needs a design decision (per run or per migration) that has not been taken" as the reason. That was not the real reason.

## The decision

**Not a hook. A statement.**

```sql
-- godwit: assert 'SELECT count(*) FROM orders WHERE total IS NULL' = 0
```

A hook is SQL godwit runs that the pull request never showed. Every other mechanism in godwit exists to make one promise — *what the pull request showed is what ran* — and a hook breaks it at the exact point where it would be hardest to notice: a file nobody diffs, running against production, outside the plan, outside the journal, outside the run's failure accounting. Decision [0001](0001-plan-as-contract.md) rules out expanding a directive in the scheduler for the same reason. A hook is that, with the reviewer removed entirely.

So the condition is a **directive** ([0002](0002-directives-godwit-executes.md)) like every other: parsed offline at load time, expanded at plan time, frozen onto the plan and the run, rendered in `godwit plan` and in the pull-request comment, executed by the executor at a plan index, and journalled. The only thing it does differently from the nine directives before it is **read**.

## The grammar, and why it is this small

```
-- godwit: assert '<select>' <cmp> <value>
```

`<cmp>` is `=`, `<>`, `!=`, `<`, `<=`, `>` or `>=` against an integer, or `=`/`<>` against `true`/`false`. The query is single-quoted with `''` for a literal quote — the quoting #53 already defined — and there are no options and no flags.

*Rejected: a predicate language.* `assert count > 0 and count < 100`, ranges, tolerances, several conditions per line. SQL is already the predicate language, and the author has all of it inside the quotes: `SELECT count(*) BETWEEN 1 AND 99` is a boolean assertion and needs nothing from the grammar. Every operator added to the directive is an operator that has to be parsed, rendered, hashed and explained, and buys nothing SQL did not already have.

*Rejected: a `name=` label.* The failure message names the query and the two values, which is what a reader needs. A label is a second thing to keep in sync with the query.

*Rejected: string and numeric-with-decimals comparison.* `sum()` returns `numeric` and would need a decimal comparison the grammar cannot express exactly; the honest answer is a cast (`sum(x)::bigint`) or a boolean (`SELECT sum(x) = 100`). Integer and boolean cover every condition that has come up, and the refusal for a `numeric` column names the cast.

## Where it runs

**Where it is written.** A directive already occupies its position in the file (#56), and the expander splices an assertion's `SELECT` exactly there. Two shapes fall out of that one rule:

- **Ahead of the migration's own SQL — a precondition.** `assert 'SELECT count(*) FROM legacy' = 0` before `DROP TABLE legacy`. This required relaxing one refusal: #56 refuses a directive migration whose own SQL carries H002/H003/H008, because the generated contract block is a suffix and a destructive statement before it would run in the expand phase. An assertion generates **no** contract statements, so a migration whose directives are all assertions has no suffix to protect and the refusal does not apply. The relaxation is exactly scoped: one non-assert directive in the file and the refusal is back.
- **After a `change-type` or a `backfill` — the gate on the swap.** The contract block is always a suffix, so an assertion is *always* in the expand phase, and an author who writes it under the `change-type` line gets the last statement before the hold. A failing assertion fails the run before it ever reaches `awaiting_contract`, so the confirm is not "stopped" — it is never offered.

*Rejected: a `phase=` or `when=` option.* Position in the file already says it, reads better in review, and cannot disagree with the order the statements actually run in.

## Statement or not: it is a statement, and a resume re-checks it

An assertion gets a plan index, a `sql_hash` and a `godwit.journal` row, which is what makes an edited assertion refuse to resume a run in flight, and what puts it in the same audit trail as everything else.

But `loadProgress` exists to **skip** what is done, and skipping an assertion is precisely the failure this feature is meant to prevent: the interesting crash is one *between* the expand phase and the confirm, and the interesting confirm is one a human makes hours later. So `Executor.apply` walks from statement 0 and skips a completed statement **unless it is an assertion**:

```go
if i <= prog.lastDone && p.Statements[i].Assert == nil {
    continue
}
```

Re-running is safe because it is a `SELECT`. The gain is the whole point of the feature: `ConfirmRollout` rebuilds the plan with `HoldFrom = 0` and resumes the same run, so the walk to the contract statements passes the assertion again and re-evaluates it against the data as it is **now**. A backfill that was consistent when the expand phase ended, and is not consistent at confirm time, does not get swapped.

The cost is real and is documented rather than engineered around: an assertion whose subject the same migration changes must be written to stay true after the change, or placed after it. A gate that only holds the first time is not a gate.

*Rejected: keeping it beside the plan* (a field on the migration, a separate check phase). It would need its own ordering rules, its own rendering, its own journal, and it could not express the precondition shape at all.

## Validation: offline, then on the scratch, then read-only at run time

Three layers, each catching what the one before it cannot.

1. **Offline, libpg_query, at load time.** One statement, a `SelectStmt`, no `INTO`, no locking clause, no data-modifying CTE, exactly one target column, no bare `*` — recursing through set operations and CTEs. A failure is `E004`, the same code every malformed directive gets, so `godwit lint` catches it with no database at all.
2. **On the scratch database, at plan time — a probe, not a check.** The scratch carries the target's replayed history and none of its rows, so `count(*)` there means nothing and enforcing the comparison would be a lie. The query is still executed: a table or column the schema does not have fails the validation, and the column's type OID is checked against the comparison. That is what makes a typo in an assertion a **pull-request** failure. The row count and the value are the target's business and are checked only there. `engine.WithAssertProbe()` is the one injection point, used by `Validator.Validate` and `Validator.Replay`.
3. **A read-only transaction, at run time.** `SET LOCAL transaction_read_only = on` before the query. Volatility lives in the catalog, so the offline check cannot know that `SELECT sneak()` writes; PostgreSQL can, and refuses it. This is the guard, not the libpg_query pass — the libpg_query pass is what gives the author the error in CI instead of in production.

*Rejected: checking `pg_proc.provolatile` for every function in the query at plan time.* The expander already does exactly that for `change-type`'s `using=`, so it was the obvious move. It is also strictly weaker than the read-only transaction: a `STABLE`-declared function can still write, and a function created between plan and run is not in the catalog the plan read. The transaction closes both.

## Failure

`failed`, never `needs_attention`, and never retried: the assertion is a deterministic function of the data, so a second attempt fails identically. It falls out of `classify` without a change — an assertion failure is a plain Go error, not a `PgError` and not a network error, so it is not transient. The message names the query, the value it got and the value it wanted, and rides on the run row, so the pull-request comment, `godwit run get`, the UI and the Slack/webhook notification carry it with no new plumbing.

## What falls out, and what does not

| Interaction | Outcome | Why |
|---|---|---|
| Plan key (#39) | an edited comparison is a new plan | the key is a pure function of the files, and the comparison is in the file; `expansionHash` carries the condition too, so `SameStatements` refuses a re-plan whose condition moved |
| `already_applied` (#42) | never marked | a `SELECT` is DML, so `Plan.Opaque()` already stops the prefix walk — marking it applied would skip the check, which is the one thing that must not happen |
| Revert (#68) | no inverse, and a refusal when it is the only directive | an assertion has nothing to undo; a migration whose directives are all assertions gets `-- godwit: revert` refused rather than an empty down body that would later fail as "never expanded" |
| Repeatables (#50) | refused, as every directive is | `E004`. Relaxing it for assertions means teaching the expander and the ledger about repeatables, which is a change to the directive machinery, not to assertions. Not done. |
| `--to` (#76) | a withheld migration does not check its assertion | it does not run |
| Ledger (#75) | unchanged | an assertion applies nothing, so there is nothing to record or revert |
| `rollout: direct` | allowed | the refusal in #56 is for a two-phase expansion; an assertion produces one phase |
| A destructive statement under `expand-contract` | the whole migration, assertion included, moves to the contract phase | `contractFrom` falls back to the hazard test at index 0. The order is still assertion-then-`DROP`, so the precondition is checked when the human confirms — which is where it belongs |

## What is not built

- **An assertion in a `.down.sql`.** `E004` refuses every directive but the `revert` sentinel there, and a down side is already either hand-written SQL or that sentinel.
- **An assertion that warns instead of failing.** A condition you would not stop on is a query, not an assertion. Liquibase's `onFail` policies are the alternative shape; they exist because a precondition there gates a changeset in a changelog that may be shared across environments. godwit's unit is one file for one target.
- **A tolerance (`= 0 ± 5`).** `SELECT abs(a - b) <= 5` is the same thing in SQL the reviewer can read.
