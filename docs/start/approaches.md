# Approaches to a migration

Every schema change on a live database is a choice between two costs: the lock you take now, or the extra
steps you take to avoid it. This page is about making that choice deliberately — and it is written against
what godwit will actually stop you on, because the ten hazard codes `H001`–`H010` are exactly the places where
the cheap form and the safe form differ.

godwit parses your SQL with PostgreSQL's own parser and classifies each statement before anything runs. A
hazard is not a warning you scroll past: the run is refused at admission until you name the code.

```
unacknowledged hazards (pass acknowledge_hazards to accept):
H003: DROP COLUMN is destructive
```

There is one level. A hazard blocks or it is acknowledged with `--ack H003` — there is no severity ladder, no
per-file pragma and no config key that turns one off.

## Direct or expand-contract

`rollout: direct` is the default: every statement of every migration runs in one pass, and the run ends when
the last one lands. `rollout: expand-contract` splits the same plan in two, applies the first half, parks the
run in `awaiting_contract`, and waits for a human: `godwit confirm` as a pull request comment, or
`godwit run confirm` from a pipeline.

godwit decides where to cut, not you. The contract phase begins at the first statement godwit marked
`contract` — which a directive does — or, failing that, at the first migration carrying one of **`H002`
(DROP TABLE)**, **`H003` (DROP COLUMN)** or **`H008` (RENAME)**. Those three are the hazards that break an
application still running the old code, and they are the reason the split exists. A plan with none of them and
no directive has nothing to hold back, and `expand-contract` runs it straight through.

| | Direct | Expand-contract |
|---|---|---|
| Deploys needed | one | two: the expand lands, the application ships, then the confirm |
| Time the old and new shapes coexist | none | from the expand until the confirm — hours or days, and that is the point |
| Rollback after the change is live | restore, or a revert that has to undo real DDL | the old column is still there holding the data; the way back is to not confirm |
| Cost | a window where the application is broken, or a lock readers queue behind | leftover columns, a longer calendar, and a run parked on the target |
| What godwit does to you | nothing extra | `godwit migrate` on that target is refused while a run is parked: *"target app has run r-… awaiting contract; confirm or revert it first"* |

Two consequences worth planning around. A migration whose SQL expands into both phases is refused under
`direct` — *"expands into expand and contract phases; use rollout: expand-contract"* — so the rollout is a
property of the change, not a preference. And because only one run per target may be parked, expand-contract
serialises that target: the migration that is waiting for a deploy is blocking the next one.

**Downtime is honestly cheaper when the table is small.** An `ALTER TABLE` that takes an exclusive lock on ten
thousand rows finishes before anyone notices; the same statement on a hundred million rows holds every reader
and writer behind it for the length of a full rewrite. godwit will not make that judgement for you — it does
not know your row counts at lint time — but it does bound the damage: `lock_timeout` defaults to **5s**, so a
statement that cannot get its lock gives up instead of queueing behind a long transaction and building a
convoy of waiters behind itself. `statement_timeout` is **0 by default**, which means disabled; set it per
target if you want a ceiling on the statement itself.

Separately, godwit takes one advisory lock per database so two runs never overlap on the same target. When
it cannot get that one it never terminates the holder — it names it and gives up:

```
acquire advisory lock on app (held by pid 4812, application_name "psql", idle in transaction for 930s)
```

## What each hazard is about

Three groups, and they want different things from you.

**Locks other sessions queue behind** — `H001`, `H004`, `H006`, `H007`, `H009`, `H010`. The change is
compatible with the running application; the problem is the seconds or minutes the table is unavailable. The
recipe is always the same shape: do the expensive part without the lock, then take the lock for something
instantaneous.

**Destruction** — `H002` (`DROP TABLE`), `H003` (`DROP COLUMN`). The rows go and nothing brings them back.

**Breaking the code that is running** — `H008` (`RENAME TABLE`, `RENAME COLUMN`). The statement itself is
instant; what it breaks is every application version still using the old name.

The second and third are what `expand-contract` holds back, because deploying around them is the only fix.

## `CREATE INDEX` and what `CONCURRENTLY` costs

**H001 — `CREATE INDEX without CONCURRENTLY blocks writes on <table>`.** A plain `CREATE INDEX` takes a lock
that lets readers through and stops every writer until the index is built. On a large table that is the
duration of a full scan and a sort.

`CONCURRENTLY` buys writes staying up, and charges for it:

- **Two passes over the table**, plus a wait for every transaction that was open when it started. It is
  slower in wall-clock terms than the blocking form, sometimes much slower.
- **It cannot run inside a transaction.** godwit marks the statement `no-tx` and takes it out of the plan's
  transaction — which also means a plan carrying one can never be applied atomically as a whole.
- **It can fail and leave an `INVALID` index behind** that indexes nothing and still costs writes.

That last one is the reason godwit refuses an unnamed concurrent index:

> `CREATE INDEX CONCURRENTLY must name the index so godwit can verify it after a crash`

With a name, godwit writes an intent to the journal before the statement and reconciles afterwards: if the
index is absent it runs again; if it is present but `indisvalid = false` it drops it and runs again; if it is
present, valid and the same shape, the statement is done. An index with that name and a *different* shape is
a hard stop rather than an adoption.

The directive form is the one to reach for, because godwit renders it against the live catalog:

```sql
-- godwit: add-index users (email) name=users_email_idx
```

It is always `CONCURRENTLY` — there is no option to make it otherwise — and if it finds a leftover `INVALID`
index of that name from an interrupted build, it drops it first and says so in the plan.

**H009** is the mirror image: `DROP INDEX` without `CONCURRENTLY` takes a lock that blocks reads *and* writes.
`-- godwit: drop-index <name>` always uses the concurrent form. It has no generated inverse — if you want the
index back on a revert, write the `CREATE INDEX` in the `.down.sql` yourself.

## `NOT NULL`

Two different hazards, and the distinction matters more than the folklore about it does.

**H005 — `ADD COLUMN NOT NULL without DEFAULT fails on non-empty tables`.** It does not rewrite the table; it
does not take a long lock. It *fails*, immediately, because the existing rows would violate the constraint.
Two ways out, both in the recipe godwit prints:

```sql
-- add it nullable, fill it, then constrain it
ALTER TABLE users ADD COLUMN country text;
-- ...backfill...
ALTER TABLE users ADD CONSTRAINT country_not_null CHECK (country IS NOT NULL) NOT VALID;
ALTER TABLE users VALIDATE CONSTRAINT country_not_null;
ALTER TABLE users ALTER COLUMN country SET NOT NULL;
ALTER TABLE users DROP CONSTRAINT country_not_null;
```

or, when one constant fits every row that already exists, a single metadata-only step — PostgreSQL 11 and
later store the default without touching the rows:

```sql
ALTER TABLE users ADD COLUMN country text NOT NULL DEFAULT 'unknown';
```

**H007 — `SET NOT NULL on <column> scans the table under an exclusive lock`.** This is the one that hurts:
turning an existing nullable column into a non-nullable one makes PostgreSQL read every row while holding a
lock nothing else can pass. The ladder above is why the recipe exists — `VALIDATE CONSTRAINT` does the scan
under a weak lock that lets reads and writes continue, and from PostgreSQL 12 onwards `SET NOT NULL` then
finds the proven `CHECK` and skips its own scan entirely. Four statements instead of one, and the exclusive
lock is held for microseconds.

`-- godwit: add-not-null users.country` renders exactly that ladder against the catalog, and is smarter than
the hand-written version in two ways: if an equivalent `CHECK` already exists it reuses it rather than adding
a second, and if that constraint is already validated it skips the scan.

## Changing a column's type

**H004 — `ALTER COLUMN TYPE rewrites the table under an exclusive lock`.** Every row is written out again,
every index on the table is rebuilt, and nothing reads or writes the table until it finishes. There is no
flag that makes this cheap; the only answer is to not do it in one statement.

`-- godwit: change-type orders.customer_id text using='customer_id::text'` is that answer, expanded against
the live catalog and frozen onto the plan:

*Expand* — add `customer_id_new`, carry the old column's default over to it, install a trigger that keeps the
two in step on every insert and update, backfill the existing rows in batches, and finally assert that no row
is left where the two disagree. Writes never stop. *Contract* — drop the trigger, rename `customer_id` to
`customer_id_old` and `customer_id_new` to `customer_id`.

Two things to decide before you use it. `keep-old` defaults to `true`, which leaves `customer_id_old` on the
table holding the data you started with — that column *is* the rollback, and the plan tells you how to remove
it later (`-- godwit: drop-column public.orders.customer_id_old`). Setting `keep-old=false` drops it in the
contract phase and makes the migration irreversible; godwit says so rather than generating a down it cannot
honour. And the directive refuses outright where it cannot be safe: an identity or generated column, a column
in a foreign key, one with a dependent view, index, trigger or policy, or a `using=` expression calling a
`VOLATILE` function — because the trigger and the backfill would then disagree with each other.

## Constraints

**H006 — `ADD CONSTRAINT FOREIGN KEY|CHECK scans the whole table under lock`.** Split it in two: add the
constraint `NOT VALID`, which only checks new rows and takes the lock for an instant, then `VALIDATE
CONSTRAINT` in a separate statement, which does the scan under a lock that lets reads and writes through.

```sql
ALTER TABLE orders ADD CONSTRAINT orders_customer_id_fkey
  FOREIGN KEY (customer_id) REFERENCES customers (id) NOT VALID;
ALTER TABLE orders VALIDATE CONSTRAINT orders_customer_id_fkey;
```

`-- godwit: add-fk orders.customer_id -> customers.id` and `-- godwit: add-check` always emit both steps;
there is no single-step form of either.

**H010 — `ADD PRIMARY KEY|UNIQUE builds its index under an exclusive lock`.** The constraint needs a unique
index, and by default it builds one while holding the table. Build the index first, concurrently, then adopt
it:

```sql
CREATE UNIQUE INDEX CONCURRENTLY users_email_key_idx ON users USING btree (email);
ALTER TABLE users ADD CONSTRAINT users_email_key UNIQUE USING INDEX users_email_key_idx;
```

`H010` fires only when the constraint names no index — writing `USING INDEX` is what clears it.

## Where a backfill belongs

A backfill belongs **in a migration** when it is part of the schema change and the change is not correct
without it: the new column filled before it becomes `NOT NULL`, the new typed column agreeing with the old one
before the swap. godwit generates exactly those itself, and what it generates is not a bare `UPDATE`:

- **Batched**, 5000 rows per transaction by default (`batch=`), walking a cursor over a key it has verified is
  non-null and backed by a single-column unique btree index — because a cursor over anything weaker can skip
  or repeat rows, and godwit refuses rather than risk it.
- **Resumable.** Each batch commits the rows and the cursor position in the same transaction, so a replica
  killed mid-backfill is taken over and continues from the last committed batch. On restart the cursor repeats
  at most one batch and never skips one.
- **Correct while the application writes**, because a trigger installed alongside keeps rows arriving during
  the backfill in step with it.
- **Checked.** A trailing assertion counts the rows still pending and fails the run if any remain, so a
  backfill that never converges cannot be reported as success — and cannot become an irreversible swap.
- `pause=` between batches, when you want to give replication room.

A backfill does **not** belong in a migration when it is data work wearing a schema-change costume: recomputing
a denormalised total, correcting records, anything that could run tomorrow instead of now and would keep a
deploy waiting on it. A hand-written `UPDATE orders SET ...` with no bound is the worst of both — one
transaction for the whole table, every row locked until it commits, no cursor and nothing to resume from if
the run is killed half way.

The middle ground is real and godwit's own H004 recipe names it: expand in a migration, backfill in batches
*outside* one, then swap in a later migration. Take it when the backfill runs for days.

## When a hazard is not reported

Three cases, so an empty hazard list is never a mystery:

- **A checkpoint body carries none.** Every hazard is about a table that already holds rows, readers or
  writers, and a checkpoint body only ever runs on a database with none of the three.
- **Statements godwit generated from a directive carry none** — godwit rendered the safe form itself, so there
  is nothing to warn you about. Hand-written SQL sitting in the same file around the directive keeps its own
  hazards.
- **The down side of an expansion carries none.**

## The one gate that is not a hazard

`--ack` covers hazards. It does not cover destroying rows on the way back: a `godwit revert` whose plan drops
a table or column still holding data is refused with the row counts, and takes `--allow-data-loss` to run
anyway. That gate exists on the revert path only — going forward, `DROP TABLE` is `H002` and an ack is all
godwit asks for.
