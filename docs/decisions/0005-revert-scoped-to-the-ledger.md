# 0005 — `revert` undoes what a run applied, read from the ledger, never the directory

Shipped in #68. Completed by #71, which moved the scratch-validation replay onto the same ledger.

## The incident

`godwit revert <run-id>` reverted the down side of **every file in the directory the run submitted**, not the migrations that run actually applied. `godwit migrate --dir db/migrations` sends the whole directory on every run, so a later run's file set contains every earlier migration.

Reproduced while reading the documentation as a first user: two migrations applied by two separate runs (run A creates table `a`, run B creates table `b`), then `godwit revert <run B> --ack H002` dropped **both** tables and left `godwit.migrations` empty. The `--ack H002` was itself the tell — the `DROP TABLE` it demanded belonged to migration A, which run B never applied.

The command looked like "undo that one thing" and behaved like "undo the directory". That mismatch is the bug, not the width itself.

## What the field does

A survey of thirteen tools, sourced below. Three families emerge:

1. **"One step back" defaults** — Flyway, Atlas, Rails, pgroll. The default unit is the last thing that happened.
2. **"Name a target or nothing happens"** — Alembic, Django, Liquibase's tag and count commands. No default at all.
3. **"Everything" defaults** — golang-migrate `down`, Sqitch `revert`. Both compensate with an **interactive confirmation prompt**, which is telling: no tool defaults to a wide blast radius *without* asking first.

godwit was in group 3 by blast radius with group-1 ergonomics, in a pipeline that cannot prompt.

**Scope from the ledger is near-universal.** Flyway walks the schema history table in reverse applied order. Liquibase's deployment-scoped `rollback-one-update` reads DATABASECHANGELOG rows. Atlas sidesteps the question by trusting neither and computing the plan from the live database. pgroll is scoped to the single *active* migration and is a no-op on anything completed.

**The one tool that resolves down files from the source directory is golang-migrate — and it is the one tool that has to stop and ask**, `Are you sure you want to apply all down migrations? [y/N]`. Its version table holds a current version plus a `dirty` flag, so it has nothing else to read.

**Liquibase documents this exact axis as a footgun**, on a different command:

> The `rollback-count` command increments based on **changesets in the changelog, not records in the DATABASECHANGELOG table**.

> Using `rollback-count` comes with the risk of removing all changes depending on the number you specify.

Its answer to "revert what that deployment did" is a different command entirely, `rollback-one-update --deployment-id=…`, scoped to "all changesets **related by a specific deployment ID**". That is `revert <run-id>` done correctly.

## Do teams run down migrations in production?

Almost never, and the vendors say so themselves. This matters because it sets how much the escape hatches are worth.

**Atlas**, from customer interviews:

> In all of these interviews, we have only met with *a single team* that routinely applied down files in production (and even they were not happy with how it worked).

Their three reasons: a *partially failed* up migration leaves a state the pre-written down never anticipated; data loss (reverting an added column deletes the column's data, and re-applying does not bring it back); and CI/CD incompatibility, since rolling back the deployment artifact gives you the *old* commit, which does not contain the down files — they were written in a later one.

**Redgate — who sell the undo feature — recommend against using it in production.** This is the most credible stance in the corpus precisely because it is against their own commercial interest:

> As a general rule, when reverting database schema changes made to any live production database, it is not only simpler to roll forward, it also maintains the deployment audit trail which is all important for compliance purposes.

> Rollback scripts (aka undo scripts) are no substitute for having a robust backup/restore strategy… certain changes, such as table or column drops cannot be recovered directly, and will require a backup to be restored to recover dropped data.

**GitLab is the best "large team, written policy" data point, and it is nuanced.** Their style guide *requires* reversibility — "Your migration **must be** reversible" — and yet:

> On GitLab production environments, if a problem occurs, a roll-forward strategy is used instead of rolling back migrations using `db:rollback`.

So down methods are mandatory *as a development and review artifact* and are not the production recovery mechanism. They accept a `down` that does nothing, as long as it explains why. They also have the sharpest operational concept in the corpus: **post-deployment migrations are a rollback barrier**, "the point (package version) that can't be crossed for rollbacks", validated by a ChatOps command before a rollback proceeds.

**Liquibase's own caveats** confirm that "write a down for everything" is not achievable mechanically: it "cannot automatically generate rollback SQL for Change Types like `dropTable` and `insert`", and "You must write custom rollback statements for all formatted SQL changelogs… regardless of the Change Type", because "there are multiple states the database could be in right before a `dropTable` statement". **godwit is a raw-SQL tool, so it lives permanently in that category**: every down is hand-written, and therefore only as correct as its author's imagination of the future state.

**After the contract phase, revert is essentially not a thing.** pgroll makes it structural — both schema versions exist side by side during a migration, so rollback is just removing the new one, and "Migrations cannot be rolled back once completed." PlanetScale buys a window with replication instead: the old table is retained and kept in sync by VReplication for **30 minutes**, after which it is gone and you are back to restoring from backup. Both make post-contract revert look cheap by *not actually contracting yet* — a storage trade, not a semantics one.

## The decision

Approved as recommended, 2026-09-03, and shipped in #68.

| | before | after |
|---|---|---|
| Scope | the down side of **every file in the directory the run submitted** | the down side of **the migrations that run applied**, from the ledger, in reverse order of application |
| Default target | `run_id` required | the newest un-reverted run of `--target`; nothing wider, ever |
| Older run | refused, no way through | refused (`run y is newer and still stands`), released by `--force` |
| Preview | none | the plan is always printed before anything runs; `--dry-run` prints it and queues nothing |
| Data loss | silent | **refused** when the plan drops a table or column that still holds rows; released by `--allow-data-loss` |
| History | the original's versions vanished from the applied set | the revert is a new run; the original stays, turns `reverted`, and its ledger rows record `reverted_by` |

The reasoning, item by item, is the survey above: scope from the ledger because every tool that gets this right does; default to one step back because groups 1 and 2 both do and group 3 pays with a prompt godwit cannot use; print the plan because Liquibase's docs call the preview the best practice and Atlas ships `--dry-run`; **refuse rather than warn** on data loss because Atlas is the only tool that blocks and it is the right call — the failure mode the whole corpus agrees on is data loss, and a warning in CI logs is not read; keep the revert in history because Redgate's compliance argument for roll-forward is precisely that a revert which erases its predecessor destroys the evidence the incident review needs.

**No time window.** PlanetScale's 30 minutes is genuinely good and is affordable only because VReplication keeps the old table live and in sync. Without that machinery a window restricts *when* you lose data, not *whether*. The data-loss gate is the real version of that guarantee.

The trade-off in item 4 is stated plainly rather than smoothed over: **this makes `revert` fail in exactly the situation where someone is panicking during an incident.** That is intentional. The correct action at that moment is usually roll-forward or restore-from-backup, and the vendor selling the undo feature says so in their own docs. The honest alternative to this trade-off is not "warn instead of refuse" — it is "make the flag easy to find in the error message".

## What it cost to build

- **A per-migration ledger.** `cp_run_applied (run_id, migration, seq, held, expansion, applied_at, reverted_by)`. The scheduler writes a row per plan as it completes, so a run that fails half way leaves an honest ledger and a revert picks up exactly what stands. A skipped migration writes nothing. A revert sets `reverted_by` per migration as each down plan completes, so a partly-failed revert is recorded correctly and a second `revert` continues.
- **`Store.Applied` had to move onto the ledger too — mandatory, not incidental.** With the narrower scope, marking the original run `reverted` must not withdraw the migrations it merely *carried*.
- **It fixed a second bug on the way.** A directive expanded by an *earlier* run made every *later* run non-revertable: `RevertRun` called `ExpandDown` with only the current run's expansions, so the earlier migration's directive had nothing to substitute. Expansions are now recorded **per migration**, on the ledger row. `cp_runs.expansions` stays as the source for the *up* direction and for resuming a contract phase — that is what a run is *going to* apply; the ledger row is what a run *did* apply, and it is what the inverse is read from. *Rejected: a target-wide `cp_expansions` table* — the ledger row already has exactly the right lifetime and cardinality, and a separate table would need its own reconciliation with reverts.
- **#71 then had to move the replay too.** `Store.HistoryFiles` still returned every file of every succeeded run, which after #68 became a contradiction: the ledger could say a migration was reverted while a later run's file set still carried it. In practice a reverted migration came back on the scratch database, so `godwit plan` reported that a pending migration would do nothing — and worse under the expand-once rule, a reverted *directive* replayed as history with no statements, so `godwit migrate` would have recorded a `change-type` as applied and executed nothing. `Store.History` now reads the ledger with the same `standingRow` predicate `Store.Applied` uses, so the two halves of that rule are literally the same SQL clause instead of two queries that happen to agree.

## Consequences to live with

- **Generated inverses are exempt from the data-loss gate.** The gate reads the down files *you* wrote. A `-- godwit: revert` inverse is generated, and godwit already refuses to generate one wherever it would not be lossless (`drop-column`, `drop-index`, `backfill`, `keep-old=false`), so `ExpandPlan` clears `Drops` on them for the same reason it already clears hazards. *Alternative rejected:* gating generated inverses too, which would make the flagship `change-type` revert — dropping a shadow column holding a copy of data that survives in the original — always require `--allow-data-loss`, a false positive that would train people to pass the flag. This is a refinement the research did not anticipate.
- **`ErrNotRevertable` is one sentinel with a stated reason** (`its state is "queued"`, `run y is newer and still stands`, `target app has a queued or running run`, `it applied no migration that still stands`, `it is itself a revert`), instead of one message covering three causes.
- **The backfill of existing data is an approximation, and only for data that predates the ledger.** Each `(name, body)` is attributed to the earliest succeeded run that carried it, which is when it would have been applied. A repeatable that flips back to an earlier body across three runs is attributed to the first one.

## Explicitly not done

- **Generating downs by diffing the live schema (the Atlas model).** It is the best design in the survey and it is a different product: it needs a schema-introspection and planning engine, and godwit is a versioned-SQL-file runner. Adopting Atlas's *guardrails* — dry run, destructive-change refusal — without its *planner* is the achievable subset, and is what shipped.
- **Making down files mandatory in a stronger sense than they already are.** A hand-written down for a raw-SQL tool is, by Liquibase's own reasoning, unverifiable. Requiring one produces ceremonial files nobody has tested, which is worse than an honest "this run is not revertible". Flyway already behaves this way — undo stops at the first migration with no undo script.

## Where the evidence is thin — read this before quoting any of it

- **Nobody has measured production down-migration usage.** Atlas's "one team out of hundreds of interviews" is the only quasi-quantitative claim in the corpus, its methodology is unpublished, and it was written to launch a competing feature. Direction: well-supported. Magnitude: unquantified. Do not cite it as a statistic.
- **The Flyway paywall cuts both ways.** `undo` requires Flyway Teams. That is consistent with both "low usage, safe to gate" and "high willingness to pay", and no source settles it. The first reading is mildly favoured only because Redgate's own rollback-strategy document argues against using undo in production — unusual enough behaviour for a vendor to weight heavily — but that is inference, not evidence.
- **GitLab is one data point and an atypical one.** Their scale, self-managed distribution and upgrade obligations are why `down` is mandatory there. Do not read "GitLab requires downs" as "serious teams require downs"; read the second half of their policy, which is that production rolls forward regardless.
- **The expand/contract practitioner literature is weak.** Beyond pgroll's and PlanetScale's own docs, most of it is derivative blog content restating the pattern. The claim that each phase must be reversible without data loss is widely repeated and no primary source measures whether teams achieve it.
- **golang-migrate's version table shape was not verified from primary docs.** The README and FAQ document the `dirty` flag but not the absence of full history; the statement that it holds only a current version is an inference from the CLI resolving down files from the source directory.
- **Supabase's stance is unsettled, not a stance.** The thread is open and a maintainer signals intent to generate downs from declarative schema. Cite it as evidence of demand in dev workflows, not of a considered position.

## Sources

- https://documentation.red-gate.com/flyway/reference/commands/undo
- https://github.com/flyway/flyway/blob/main/documentation/Reference/Commands/Undo.md
- https://documentation.red-gate.com/fd/implementing-a-roll-back-strategy-138347142.html
- https://docs.liquibase.com/commands/rollback/home.html
- https://docs.liquibase.com/commands/rollback/rollback-count.html
- https://docs.liquibase.com/commands/rollback/rollback-one-update.html
- https://docs.liquibase.com/pro/user-guide-4-33/what-automatic-rollbacks-does-liquibase-support
- https://atlasgo.io/versioned/down
- https://atlasgo.io/versioned/checks
- https://atlasgo.io/blog/2024/04/01/migrate-down
- https://atlasgo.io/blog/2024/11/14/the-hard-truth-about-gitops-and-db-rollbacks
- https://pgroll.com/docs/latest/cli/rollback
- https://github.com/xataio/pgroll
- https://github.com/golang-migrate/migrate/blob/master/cmd/migrate/README.md
- https://github.com/golang-migrate/migrate/blob/master/internal/cli/main.go
- https://github.com/golang-migrate/migrate/blob/master/FAQ.md
- https://guides.rubyonrails.org/active_record_migrations.html
- https://docs.djangoproject.com/en/5.2/topics/migrations/
- https://alembic.sqlalchemy.org/en/latest/tutorial.html
- https://alembic.sqlalchemy.org/en/latest/api/commands.html
- https://sqitch.org/docs/manual/sqitch-revert/
- https://www.prisma.io/docs/orm/prisma-migrate/workflows/generating-down-migrations
- https://docs.gitlab.com/development/migration_style_guide/
- https://docs.gitlab.com/development/database/post_deployment_migrations
- https://planetscale.com/docs/vitess/schema-changes/safe-migrations
- https://planetscale.com/blog/revert-a-migration-without-losing-data
- https://github.com/orgs/supabase/discussions/11263
