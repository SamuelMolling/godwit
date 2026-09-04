# Renaming columns and tables — **proposed, not decided**

> **Status: open.** Not a decision record — the argument that has to be had before one exists.
> Nothing here binds the code. It lives in `docs/decisions/open/` and carries **no number** on
> purpose: the numbered records above all name the pull requests that shipped them, and a number
> here would claim a standing this text does not have. It takes one when it is decided, either way.

## The question

[0002](../0002-directives-godwit-executes.md) deferred `rename-column` and `rename-table` in one
table row: *a safe rename needs the application to read either name during the transition; pgroll
buys that with versioned views; godwit's unit is a versioned SQL file and it will not start owning
the application's view of the schema.* One sentence, behind a decision that shapes what godwit is
for. This record re-opens it so the deferral is **confirmed on purpose** or **overturned on
purpose** rather than inherited.

**Three things this review found, before the argument starts.**

1. **#69 does not transfer.** A hand-written rename is *not* the silent 17-dependent catastrophe
   `change-type`'s swap was. Every dependent follows a single rename correctly. Measured, below.
2. **`rename-table` is the easy case, and the docs have it backwards.** `recipeRenameTable`
   prescribes a full physical table copy for something two catalog writes do, because PostgreSQL's
   auto-updatable views hand you a read *and write* compatible old name for free.
3. **0002's premise about pgroll is incomplete.** pgroll does not avoid the physical rename — its
   `Complete` phase runs `ALTER TABLE ... RENAME COLUMN`, the same statement godwit gates behind
   H008. What the views buy is the *window*, not the rename. And pgroll pointedly does **not**
   duplicate the column for a rename, which is the shape option (c) proposes.

---

## What breaks today

Someone hand-writes `ALTER TABLE users RENAME COLUMN age TO age_at_signup`. Four layers see it.

**1. The hazard gate catches it, and holds it.** `classifyRename`
(`internal/engine/plan.go:305`) raises **H008** for both table and column renames; `PlanRun`
refuses with `failed_precondition: unacknowledged hazards` until `--ack H008`. H008 is a
*contract* hazard, so under `rollout: expand-contract` the plan is held, the run parks at
`awaiting_contract`, and nothing renames until a human calls `ConfirmRollout`. **godwit already
makes a rename a deliberate, acknowledged, human-confirmed act.** What it cannot hold is the
*application*: expand/contract buys a confirmation point, not a compatibility window.

**2. The #69 dependents refusal does not apply — and does not need to.** `undepended` has exactly
two callers, both inside the expander (`expand.go:364` for `change-type`, `:764` for
`drop-column`). A hand-written rename never reaches it, so nothing checks its dependents. It is
tempting to read #69's "a rename moves 17 kinds of object silently" as *so a hand-written rename
is a silent catastrophe too*. **It is not.** #69's corruption is a property of the **swap** — two
renames that hand a *different physical column* the name a dependent was bound to. A single rename
has no second column. Verified on PostgreSQL 17 (throwaway container, removed):

| Dependent | After one `RENAME COLUMN age TO age_at_signup` | Correct? |
|---|---|---|
| view `SELECT id, age FROM users` | `SELECT id, age_at_signup AS age FROM users` | **yes** — and it keeps its own output name `age`, so the view's readers are untouched |
| index (plain / expression) | `btree (age_at_signup)` / `btree (((age_at_signup * 2)))` | yes |
| check constraint | `CHECK ((age_at_signup > 0))` | yes |
| the column's `DEFAULT`, its `COMMENT` | both moved | yes |
| FK from another table (across a *table* rename) | `REFERENCES purchases(id)` | yes |

Seven of seven follow the rename and keep working. PostgreSQL binds dependents to the physical
attribute, which is exactly why a *rename* is the one operation attribute-binding gets right. The
expander's refusal is right for `change-type` and would be **noise** here.

**3. What actually breaks: names resolved at run time.** Same container:

```
=> CREATE FUNCTION f_age() RETURNS integer LANGUAGE plpgsql AS $$
     DECLARE n integer; BEGIN SELECT age INTO n FROM users LIMIT 1; RETURN n; END $$;
=> SELECT f_age();                                            -- 42
=> ALTER TABLE users RENAME COLUMN age TO age_at_signup;
=> SELECT f_age();
ERROR:  column "age" does not exist
```

A `plpgsql` body records no dependency, survives untouched, and fails at its next call. So does the
application's SQL, dynamic SQL, and an ORM's cached column list. **All of it fails loudly.**

**So the production failure is:** a catalog-only DDL that completes in milliseconds, is instantly
correct in the database, and instantly wrong for every process not yet redeployed. Duration = the
deploy window. Blast radius = every query naming the old identifier. Severity = errors, not
corruption. Reversibility = total (rename back; #68's data-loss guard never fires because
`Statement.Drops` is filled for `DROP TABLE`/`DROP COLUMN` only).

`ALTER TABLE ... RENAME` does take an `ACCESS EXCLUSIVE` lock — it is `ALTER TABLE`'s default —
but *"there is no effect on the stored data"*
([PostgreSQL docs](https://www.postgresql.org/docs/current/sql-altertable.html)): it is a
catalog write, held for microseconds, and godwit's `lock_timeout` already covers the acquisition.
There is no scan and no rewrite.

That profile is the opposite of what the directives exist for. Every shipped directive removes a
lock or a rewrite *the database* does to you. **A rename is not a database hazard; it is a
deployment-coordination hazard** — and [comparison.md](../../comparison.md) already states that the
two-version window is the deployment's problem. The external corpus agrees on exactly this
framing: Atlas's own analyzers classify it not as a lock risk but as backward incompatibility
(BC101 table, BC102 column — *"can cause errors during deployment if applications running the
previous version of the schema refer to the old name"*).

**4. Two things are genuinely wrong, and neither is the missing directive.**

- **`recipeRenameTable` (`internal/engine/recipe.go:454`) prescribes a full table copy** —
  `CREATE TABLE ... (LIKE ... INCLUDING ALL)`, dual-write, copy every row — for an operation
  PostgreSQL performs with two catalog writes (see below). Not wrong about safety; wrong about
  price, by orders of magnitude, and it is the advice a reader following the docs will take.
- **`godwit diff` emits a silent, lossy drop+add.** pg-schema-diff (`diff.go:316`) has no rename
  detection; `concepts.md:718` already records the consequence. A Prisma team renaming a field
  gets a pull request containing `ALTER TABLE users DROP COLUMN age;` + `ADD COLUMN
  age_at_signup integer;` — **every value destroyed**. H003 fires and its recipe is correct, so a
  reader who reads it is saved; but this is the one path where the author never wrote the SQL and
  is least likely to read it closely. **This is the strongest argument in the record for doing
  something** — and it argues for a `diff`-side answer, not a directive.

---

## Where a rename touches the rest of godwit

| Surface | Hand-written rename today | A directive would… |
|---|---|---|
| **`godwit diff`** (0003) | drop + add, silently lossy | **not help.** pg-schema-diff generates that SQL; a builder in `expand.go` cannot reach it. Fixing `diff` is separate work either way |
| **`already_applied`** (#42) | **works** — `visibleRename` (`opacity.go:87`) treats both rename kinds as inspectable, so a rename applied by hand in an incident is detected and recorded with a `stmt_count 0` run | **lose it.** A `change-type`-shaped expansion carries DML and non-inspectable objects, so `Plan.Opaque()` stops the prefix walk (`concepts.md:361`). It trades a working capability for a refusal |
| **Plan key / `shape()`** | an ordinary statement | free — the expansion hash already joins `shape()` (#56), so a stale re-plan is `PlanStale{history}` |
| **Ledger / `revert`** (0005, #68) | revertable by its own `.down.sql`, lossless, no `--allow-data-loss` | needs the **two-body** inverse `change-type` solved in #56 (pre-swap and post-swap, picked by the run's state). Reusable, not free |
| **Repeatables** (0006) | an `R__` body naming the old column **fails loudly on the scratch during validation**, at plan time. godwit catches this one properly | same — but a **compatibility view** is an object no ORM schema declares, so `godwit diff` proposes to drop it on every pull request. That is the 0006 problem, and closing it needs a `cp_retired_relations` analogue of `cp_retired_columns` (`schema.go:276`, `store.go:298`) |
| **The two-version window** | invisible to godwit; `awaiting_contract` is a human's confirmation, not evidence the new version is live | **unchanged by every option below except (d)** |

---

## `rename-table` is not the same problem — it is the easier one

A column cannot have two names on one table. A **table** can: rename it, then put a view where it
stood. PostgreSQL makes a simple single-table view automatically updatable, so the old name keeps
working *for writes too*. Verified, PostgreSQL 17:

```sql
ALTER TABLE orders RENAME TO purchases;
CREATE VIEW orders AS SELECT * FROM purchases;
```

`SELECT`, `INSERT ... RETURNING id` (identity column and all), `UPDATE ... RETURNING`, `DELETE`,
`INSERT ... ON CONFLICT`, and `SELECT ... FOR UPDATE` through the old name all work;
`is_insertable_into` is `YES`. Only `TRUNCATE` fails (`"orders" is not a table`), along with DDL —
neither of which an old application version does.

Two catalog-only statements. No copy, no trigger, no backfill, no double storage, no lock beyond
the instant rename — and because there is only ever **one copy of the data**, the two names cannot
diverge. Contrast (c) below, where two physical columns must be kept in sync by a trigger and the
sync is a thing that can be wrong.

---

## The options

Cost is in godwit's own currency, which `AGENTS.md` fixes: store migration, proto change, handler,
tests at store/scheduler/API level, README row, `concepts.md` rows, demo step — at 100% coverage.

| | Option | For | Against | Cost |
|---|---|---|---|---|
| **a** | **Do nothing** — H008 and the prose recipe | The gate already holds the plan and demands an ack. Zero new surface, zero new refusals to maintain | Leaves the table recipe prescribing a full copy; leaves `diff`'s lossy drop+add. Answers the question by never asking it | **zero** |
| **b** | **Teach, precisely** — rewrite `recipeRenameTable` around the view shim; make `recipeRenameColumn` name the `add-column`/`backfill`/`drop-column` sequence explicitly, as `recipeDropColumn` already does with `withDirective`; add a runbook section | Fixes the two things actually wrong, without godwit taking responsibility for the application's deploy. Consistent with 0002's posture: *a precise refusal the author can act on beats half a dependency-rewriting engine* | Gives the ORM team no one-liner. Someone will still ask for the directive | **small** — two recipe functions, a `concepts.md` row, a runbook section, tests. No store migration, no proto change |
| **c** | **`rename-column` = add + dual-write trigger + backfill + contract drop** (the `change-type` machinery with the type held constant) | ~90% of the machinery exists (#51 hold, #52 batched cursor, #56 trigger/freeze/two-body revert). Both names work at once — the actual requirement. Reviewable SQL; the plan stays the contract | **The trigger must be bidirectional**, which `change-type`'s is not — it is one-way (`SELECT age::bigint INTO new.age_new`); here an old-version writer sets `age` while a new one sets `age_at_signup`, each needing to fill the other. **And the result is not equivalent to a rename.** Its contract phase is a `DROP COLUMN`, so it goes through `droppable` (`expand.go:1109`): a column with a view, FK, trigger, rule or policy is **refused outright**, and one with a single-column index, check, comment or owned sequence is **allowed and silently loses all of them** — where a real `RENAME COLUMN` carries every one across (verified above). Making it faithful means the dependency-rewriting engine #69 refused. Doubles storage; costs `already_applied`. **And pgroll — built for exactly this — avoids the shape**, branching around its own duplicate-and-trigger machinery for renames (`if !o.IsRenameOnly()`) | **medium-large** — new builder, bidirectional trigger and its refusals, dependents handling, two revert bodies, `cp_retired_columns` interaction, ~20 refusal subtests, demo step. Realistically the size of #56 |
| **d** | **pgroll-style versioned views** — a schema per migration version, the application picking one via `search_path` | The only option that *solves* the two-version window instead of asking the team to honour it. pgroll and reshape both prove the model works, and in both, **rename becomes the cheapest operation the design supports** — it rides free on machinery built for backfills and type changes | Requires godwit to **own the application's view of the schema** — the exact thing 0002 refused, and what 0001/0004/0006 all assume it does not. Every connection string must set `search_path`, and pgroll documents the failure mode when one does not. `diff`, drift, `already_applied` fingerprints, baselining and the scratch replay all compare *one* schema today. And the free-ness is only true once the framework exists: pgroll spent **13 months and a dozen pull requests** (issue #239) teaching its other thirteen operation types to be aware of a preceding rename | **very large** — not a feature, a different product |
| **e** | **`rename-table` via a compatibility view** *(found in this review)* — rename, add the shim view, drop the view in a later contract migration | Two catalog writes; ~1% of (c)'s cost; both names work at once, reads and writes, with one copy of the data | Tables only. `TRUNCATE` through the shim fails. Creates an object the ORM does not declare → needs the `cp_retired_relations` work, or must live in a migration the team owns | **small-medium** as a directive; **near-zero** as a recipe, i.e. inside (b) |
| **f** | **Declare the rename to `godwit diff`** *(the other finding)* — an author annotation on the desired schema, plus a warning when an unannotated drop+add pair shares a type on one table | Attacks the only path where godwit's own output is worse than hand-writing. Independent of any directive. **The corpus has already run this experiment**: Atlas shipped an interactive prompt in v0.22, found `--auto-approve` could not answer it (issue #2956), and then shipped a *declarative* `renamed_from` / `-- atlas:renamed_from` annotation because "an autonomous agent running plan/apply non-interactively has no way to answer the prompt". Django has the same hole — `makemigrations --noinput` defaults `ask_rename` to false and silently emits drop+add | Detection alone is a heuristic and heuristics guess wrong, which is why Atlas, Django and Drizzle all **ask**. godwit's CI path has nobody to ask, so the annotation is the load-bearing half and the warning is only a prompt to write one. Never rewrite silently | **medium**, and **orthogonal**. Could ship under (a) |

---

## The evidence

**From this repository.** `internal/engine/plan.go:305` (`classifyRename`, H008);
`internal/engine/recipe.go:454` (both recipes, prose, no directive lead);
`internal/engine/opacity.go:87` (`visibleRename` — a rename **is** inspectable);
`internal/controlplane/expand.go:364`, `:764` (the only callers of `undepended`, both directives);
`internal/controlplane/diff.go:316` + `concepts.md:718` (pg-schema-diff, drop+add);
`internal/controlplane/store.go:298`, `schema.go:276` (`cp_retired_columns`, the precedent a shim
view would need an analogue of). [#56](https://github.com/SamuelMolling/godwit/pull/56),
[#57](https://github.com/SamuelMolling/godwit/pull/57),
[#68](https://github.com/SamuelMolling/godwit/pull/68),
[#69](https://github.com/SamuelMolling/godwit/pull/69).

**Measured for this record.** PostgreSQL 17 (`postgres:17-alpine`), one throwaway container,
removed. Both tables above are read back from the catalog, not reasoned about.

**From the corpus.** Two claims are worth taking on board even if nothing here is built.

*A rename is indistinguishable from drop + add at the schema level, so every declarative tool
either asks a human or guesses wrong.* Atlas: *"it's impossible to completely disambiguate between
`RENAME` and `DROP` and `ADD` operations"*, so it prompts `? Did you rename "users" column from
"first_name" to "name":`
([v0.22 release](https://atlasgo.io/blog/2024/05/01/atlas-v-0-22)). Alembic documents it as a
non-capability — *"Changes of column name … are detected as a column add/drop pair, which is not
at all the same as a name change"*
([autogenerate](https://alembic.sqlalchemy.org/en/latest/autogenerate.html#what-does-autogenerate-detect-and-what-does-it-not-detect)).
Django asks `Was %s.%s renamed to %s.%s (a %s)? [y/N]`
([questioner.py](https://github.com/django/django/blob/main/django/db/migrations/questioner.py))
and warns that otherwise the migration *"will lose any data in the old table"*
([docs](https://docs.djangoproject.com/en/6.1/ref/migration-operations/#renamemodel)). godwit's
`diff` is in exactly this family, and — like `makemigrations --noinput`, which defaults
`ask_rename` to false — it is on the silent side of it.

*Asking does not survive CI, which is why Atlas ended up with an annotation.* `--auto-approve` did
not bypass the prompt ([issue #2956](https://github.com/ariga/atlas/issues/2956)); Atlas's own
conclusion is that *"an autonomous agent running plan/apply non-interactively has no way to answer
the prompt, so a renamed table or column falls back to DROP + CREATE and the data is lost"*, and
the fix shipped as declarative `renamed_from` / `-- atlas:renamed_from`
([changelog](https://atlasgo.io/changelog/declarative-schema-renames)). That is option (f)'s
design, already validated elsewhere.

**On pgroll, which 0002 cites as the reason to defer.** Its rename is *not* a view-only trick: the
`Start` phase emits **no DDL at all** and only changes the name in the version's view, while
`Complete` runs the physical `ALTER TABLE ... RENAME COLUMN`
([op_rename_column.go](https://github.com/xataio/pgroll/blob/main/pkg/migrations/op_rename_column.go),
[concepts](https://pgroll.com/docs/latest/concepts)) — the same statement godwit gates behind
H008, wrapped in a compatibility window. Two details matter here:

- **pgroll does not duplicate the column for a rename.** Every other operation duplicates,
  backfills and installs up/down triggers; rename is branched around all of it
  (`if !o.IsRenameOnly()`,
  [v0.7.0 op_alter_column.go](https://github.com/xataio/pgroll/blob/v0.7.0/pkg/migrations/op_alter_column.go)).
  Option (c) is the shape the reference implementation deliberately avoids.
- **Composition is where the cost actually lands.** Teaching pgroll's other thirteen operation
  types to be aware of a preceding rename took **13 months and about a dozen pull requests**
  ([issue #239](https://github.com/xataio/pgroll/issues/239)), and pgroll later made a breaking
  format change to split rename out of `alter_column` precisely to *"remove the confusion around
  whether to use the old or the new column names"*
  ([issue #601](https://github.com/xataio/pgroll/issues/601)). Any godwit rename directive inherits
  this: it changes the name every later directive in the same migration must use.

**On the hand-written path (b) sends people down.** strong_migrations blocks both renames and
prints a six-step recipe — *"Renaming a column that's in use will cause errors in your
application"*
([error_messages.rb](https://github.com/ankane/strong_migrations/blob/master/lib/strong_migrations/error_messages.rb)).
PlanetScale states the cost plainly: *"Safely performing a rename requires downtime for your
application"*, and *"at least two deploy requests are needed"*
([docs](https://planetscale.com/docs/vitess/schema-changes/handling-table-and-column-renames));
strong_migrations [#160](https://github.com/ankane/strong_migrations/issues/160) puts it at three
deployments; Stripe's is a four-step dual-write
([Online migrations at scale](https://stripe.com/blog/online-migrations)). The three-phase framing
is Danilo Sato's
[ParallelChange](https://martinfowler.com/bliki/ParallelChange.html) (2014) — cited for the
expand/migrate/contract vocabulary; its worked example is an API refactor, not a database rename.
**This is (b)'s honest price: two to three deploys, by hand.**

## Who actually asks for this

**Thin, and one finding cuts against building anything.** This was surveyed rather than assumed,
and the honest summary is that renames are a recognised, repeatedly reinvented pain point which
most teams **avoid rather than solve**.

- **pgroll**: 21 rename-related issues, almost all authored by its own maintainers as backlog; no
  external "please support renames" request exists, and the maximum reaction count is 3.
- **Atlas** is the only project with real external pull — and it is about **automation**, not
  renames: [#2956](https://github.com/ariga/atlas/issues/2956) is a CI pipeline blocked by the
  interactive prompt. On [#1535](https://github.com/ariga/atlas/issues/1535) the maintainer notes
  *"I actually asked @elitan to open this issue"*. Reaction counts are 2–4.
- **Alembic**: 40 rename issues, all dialect mechanics. **Nobody asks autogenerate to detect
  renames** — it is settled as out of scope, not contested. Django's one relevant ticket cuts the
  other way (the prompt breaking `--noinput`). strong_migrations: 7, all closed, #160 at zero
  reactions. Skeema: two closed, zero reactions. Bytebase: effectively none. sqldef has issues
  disabled, so its demand is unmeasurable.
- **The counter-argument, met head on.** Two independent versioned-view implementations — pgroll
  and [reshape](https://github.com/fabianlindfors/reshape) — both treat rename as the *cheapest*
  operation they support, riding free on machinery built for backfills and type changes. reshape's
  author calls it *"a very basic change"*
  ([post](https://fabianlindfors.se/blog/schema-migrations-in-postgres-using-reshape/)). That is
  evidence **against** rename-specific tooling and **for** rename being a byproduct of a general
  expand/contract framework — i.e. it argues for (d) or for nothing, and specifically not for (c).
- **The best practitioner quote is double-edged.** Brandur Leach: *"the easiest way to administer a
  production database is to never rename anything, and live with the fact that some names are
  less-than-optimal … in practice it's more time, risk, and effort than it's worth"*
  ([fragment](https://brandur.org/fragments/postgres-table-rename)). Latent demand, and evidence
  that teams suppress the need rather than shop for a tool. The strongest organisational data
  point is GitLab's
  [#121607](https://gitlab.com/gitlab-org/gitlab/-/issues/121607) (5 upvotes, 24 comments), which
  produced [internal documented tooling](https://docs.gitlab.com/development/database/rename_database_tables)
  — self-driven, not customer pull.

**Inside this repository: nobody.** `gh issue list --state all` returns nothing; godwit has no
issue traffic at all. The deferral in 0002 was written by the person who would implement the
directive, and is being read by the same person. That is itself an argument for (a) or (b): **a
capability nobody has asked for, that costs `already_applied`, refuses a column with a view or a
foreign key on it and is quietly lossy on the rest, is speculative work.**

---

# Recommendation

*Everything above is analysis. This is opinion, separated on purpose. Reasonable people land on (c).*

**Take (b), plus (f) when there is appetite. Do not build `rename-column`. Fix the `rename-table`
recipe now.**

1. **A rename is not the kind of hazard godwit exists to absorb.** Every shipped directive removes
   something the database does to you. A rename holds `ACCESS EXCLUSIVE` for a catalog write and
   nothing else — no scan, no rewrite, no data effect — loses nothing, is reversible by a second
   catalog write, and fails loudly. What it breaks is the deployment, and godwit has already said
   that window is the deployment's problem. Atlas reaches the same classification independently:
   renames are BC101/BC102, backward-incompatibility analyzers, not lock analyzers. Building
   `rename-column` means quietly starting to own the deploy window, without the mechanism (d) that
   would let it own it honestly.

2. **(c) is not a rename, and cannot be made into one cheaply.** Add, sync, backfill, drop is
   `add-column` + `backfill` + `drop-column`, which godwit already has as three reviewable
   migrations behind the expand/contract gate. Collapsing them buys a shorter spelling and pays
   with a bidirectional trigger that does not exist and the loss of `already_applied`. Worse, the
   result is not *faithful*: a real `RENAME COLUMN` carries the column's index, check constraint,
   default and comment across, while (c)'s contract `DROP COLUMN` refuses outright where a view or
   FK is involved and **silently drops** the index and check where it is not. Closing that gap is
   the dependency-rewriting engine #69 already examined and refused. **A directive that refuses
   half its cases and is quietly lossy on the rest is worse than no directive.**

3. **The real defect is on a different surface.** `godwit diff` turning an ORM rename into a
   silent, lossy `DROP COLUMN` is the one place godwit is worse than hand-writing. That is (f), it
   is independent of any directive, and it is where effort should go if effort goes anywhere.

### The main counter-argument

**Expand/contract is only real if the tool enforces it, and (b) enforces nothing.** Under (b) the
safe column rename is a three-migration, three-deploy sequence carried out by hand and by
discipline, over days, remembering to come back for the contract migration. godwit exists largely
because that discipline does not survive contact with a Friday afternoon. Each of the three
existing directives is something a careful engineer could hand-write correctly; godwit builds them
anyway, because *"the author must remember"* is the failure mode it was written to remove. That
argument applies here with full force, and (c) is the version of it that reuses machinery that
already exists.

The counter-counter, and why I still land on (b): the argument justifies a directive **that
works**. This one refuses a column with a view or a foreign key on it, and is silently lossy —
dropping the index and the check — on the columns it accepts. An automation that declines half its
cases and is unfaithful on the rest is not the discipline godwit enforces; it is a footnote in the
refusal table.

### What would change my mind

- A user — any user — asking for it, on a column whose shape (c) can actually handle.
- A cheap, general way to carry a column's dependents — index, check, default, comment — onto the
  new name, making (c) faithful instead of lossy. #69 examined the recreate-other-people's-objects
  path and refused it; that would have to be reopened first.
- A decision to adopt (d) for other reasons. If godwit ever owns the application's `search_path`,
  `rename-column` is nearly free and this record is moot.
