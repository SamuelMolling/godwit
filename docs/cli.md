# Command reference

What each `godwit` command is *for*, in plain words, with an example you can paste. The exact flag list for each one is in [configuration](configuration.md#cli-reference); the mechanism behind it is in [concepts](concepts.md). This page is the middle: which command you want, and why.

| Command | What it does |
|---|---|
| **Writing a migration** | |
| [`godwit new`](#godwit-new) | Writes an empty migration pair with the name and timestamp the loader expects |
| [`godwit diff`](#godwit-diff) | Writes the next migration for you, from a schema you already have |
| [`godwit lint`](#godwit-lint) | Reads the SQL files and tells you what will hurt in production |
| [`godwit plan`](#godwit-plan) | Shows the statements that would run, one by one, before anything runs |
| [`godwit checkpoint`](#godwit-checkpoint) | Squashes years of old migrations into one file that produces the same schema |
| **Applying it** | |
| [`godwit migrate`](#godwit-migrate) | Sends a migration directory to the service and applies it to a database |
| [`godwit run confirm`](#godwit-run-confirm) | Releases the second half of a two-phase migration, once you are ready for it |
| [`godwit revert`](#godwit-revert) | Undoes exactly what one previous `migrate` applied |
| [`godwit run resume`](#godwit-run-resume) | Retries a migration that stopped, from where it stopped |
| [`godwit up`](#godwit-up) | Applies a migration directory straight to a database, with no service involved |
| [`godwit down`](#godwit-down) | Undoes one migration on your own machine |
| **Looking at what happened** | |
| [`godwit status`](#godwit-status) | Which migrations a database has, asked of the database directly |
| [`godwit target status`](#godwit-target-status) | The same, for a database the service manages, plus its last migration and its drift |
| [`godwit targets`](#godwit-targets) | One line per database the service manages |
| [`godwit migrations`](#godwit-migrations) | A grid: which of your databases has which migration |
| [`godwit runs`](#godwit-runs) | Every migration attempt, newest first |
| [`godwit run get`](#godwit-run-get) | Everything about one attempt: what it applied, why it stopped |
| [`godwit run watch`](#godwit-run-watch) | Follows an attempt that is still going, and exits when it ends |
| [`godwit plans`](#godwit-plans) | The reviewed migrations waiting to be applied to one database |
| [`godwit plan show`](#godwit-plan-show) | One of those in full, including the state of the database when it was reviewed |
| [`godwit audit`](#godwit-audit) | Who asked godwit to do what, and when |
| **Managing the databases godwit migrates** | |
| [`godwit target add`](#godwit-target-add) | Tells the service about a database and where to get its password |
| [`godwit target adopt`](#godwit-target-adopt) | Puts the migrations a database already has on the books, without running them |
| [`godwit drift check`](#godwit-drift-check) | Shows what changed in a database that godwit did not change |
| [`godwit drift accept`](#godwit-drift-accept) | Makes the live schema the new reference, and stops the alert |
| **Operating the service** | |
| [`godwit serve`](#godwit-serve) | Runs the service itself |
| [`godwit version`](#godwit-version) | Prints the version and the commit it was built from |
| [`godwit completion`](#godwit-completion) | Prints a shell completion script |

## The five words this page uses

The same binary is the service and the client, so almost every command means one of two things depending on the flags you give it. Five words carry that difference.

**Target.** A database godwit migrates, registered with the service under a short name (`app`, `staging`, `production`). You name it with `--target app`; the service knows the connection string, so you never type it. Without a service there are no targets — you point at a database yourself with `--dsn`.

**Journal.** A bookkeeping schema called `godwit` that lives *inside* each database godwit migrates, holding a row per statement it ran. It is written in the same transaction as your DDL, which is why a migration killed halfway can be picked up and continued rather than repaired. [Concepts: the journal protocol](concepts.md#the-journal-protocol).

**Run.** One attempt to apply (or undo) a set of migrations on one target. It has an id, a state (`queued`, `running`, `succeeded`, `failed`, `awaiting_contract`, `needs_attention`, `reverted`) and a list of what it applied. [Concepts: runs and states](concepts.md#runs-and-states).

**Plan.** The list of statements godwit worked out it would run, stored on the service together with a snapshot of what the target looked like at the time. A plan is what you review in a pull request; when the run finally happens it binds to that plan and refuses if the database has moved since. [Concepts: plans](concepts.md#plans).

**Scratch.** A throwaway database the service creates, replays your migrations on, and drops — so a migration that would fail is refused before it touches anything real. You never name it; you configure where it lives with `serve --scratch-dsn`. [Security: the scratch database](security.md#the-scratch-database).

## How the commands are told where to go

Three commands — `up`, `status`, `down` — talk to a database directly and take `--dsn` (or `GODWIT_DSN`). They need no service at all, and neither do `lint`, `new` and `plan` when you give them only a directory.

`up`/`down` is the local pair and `migrate`/`revert` the service pair, which is the split the vocabulary announces: nothing that takes `--dsn` reaches the service, and nothing that takes `--target` opens a connection of its own.

Every command that reaches the service takes `--server` and `--token` (or `GODWIT_SERVER` and `GODWIT_TOKEN`), plus `--json` to get the raw response instead of the table.

```bash
export GODWIT_SERVER=http://localhost:8474 GODWIT_TOKEN=s3cret-admin
```

To stop repeating `--target` and `--dir`, put them in a `godwit.yaml` next to your migrations; every command reads the nearest one up to the repo root, or the file you pass to `--config`. The keys are in [configuration](configuration.md#godwityaml).

Exit codes are the same everywhere: `0` succeeded, `1` anything went wrong — a refusal, a failed run, an unreachable server. `lint` also exits `1` when it found something blocking, and `migrate` exits `3` when the service refuses the run because the reviewed plan no longer matches the database.

### Colour

A plan lists what would change, one line per migration, marked `+` for a side the run would apply and `-` for one it would revert; `diff` and the drift block mark schema objects the same way. On a terminal those lines are green and red. `GODWIT_COLOR` is `auto` (colour when stdout is a terminal), `always` or `never`, and any non-empty `NO_COLOR` turns colour off unless `GODWIT_COLOR=always` says otherwise. The marks are the meaning and the colour only repeats it, so a pipe, a log with the escapes stripped and a colour-blind reader all get the same report. Markdown carries no escapes: the change list goes in a ` ```diff ` fence and the forge paints it.

The examples below all run against one directory:

```
db/migrations/
├── 20260901120000_create_orders.up.sql / .down.sql
├── 20260901120500_orders_customer_idx.up.sql / .down.sql
└── R__order_stats.up.sql / .down.sql
```

It grows as the page goes on, because some of these commands write migrations into it. The outputs below are real: they were captured in one session against PostgreSQL in Docker, in the order they appear, so the run and plan ids are the ones that session produced.

---

## Writing a migration

### `godwit new`

**Writes the empty pair for you, named the way the loader expects.** The loader accepts `<14 digits>_<snake_name>.up.sql` and a matching `.down.sql`, both non-empty, and nothing else; `new` produces exactly that with the current UTC second as the version.

Reach for it whenever you are writing the SQL by hand — it is cheaper than composing a timestamp and finding out at load time that you got it wrong. Do not reach for it when a schema you maintain elsewhere already describes the change; that is [`godwit diff`](#godwit-diff), which fills the files in as well.

```console
$ godwit new add_status --dir db/migrations
wrote db/migrations/20260908161500_add_status.up.sql
wrote db/migrations/20260908161500_add_status.down.sql
```

Both files hold one placeholder comment. That is deliberate: a migration with no statements is `E002` at [`lint`](#godwit-lint) and an error at [`plan`](#godwit-plan), so a scaffold you forgot to fill in blocks CI instead of shipping as a silent no-op. A name that is not `[a-z0-9_]+` is refused before anything is written, and a version already taken in the directory is stepped over rather than overwritten.

`--repeatable` writes `R__<name>.up.sql` / `.down.sql` instead — no version, re-run whenever the body changes. A repeatable's name is its identity rather than an ordering token, so a name already in the directory is refused instead of stepped over. [Concepts: repeatable migrations](concepts.md#repeatable-migrations).

### `godwit diff`

**Writes the next migration for you.** You describe the schema you want — a `.sql` file, a Prisma schema, a Django project, a GORM package, an Alembic history, a Rails `db/structure.sql`, a Drizzle config, or any command that prints DDL — and godwit compares it against the live database and writes the `.up.sql` / `.down.sql` pair that gets you there.

Reach for it when the schema you maintain lives somewhere else (an ORM, a hand-written DDL file) and you do not want to translate it into SQL by hand. Do not reach for it to invent a migration from nothing: it only ever describes the difference between a real database and a schema you already wrote down.

```console
$ godwit diff --target app --dir db/migrations --schema desired.sql --name add_order_status --dry-run
declared by repeatable migrations, so the desired schema keeps them: public.order_stats
app -> desired.sql: 1 statement(s)
  [0] tx    ALTER TABLE "public"."orders" ADD COLUMN "status" text COLLATE "pg_catalog"."default" DEFAULT 'new'::text NOT NULL

-- up
ALTER TABLE "public"."orders" ADD COLUMN "status" text COLLATE "pg_catalog"."default" DEFAULT 'new'::text NOT NULL;
-- down
ALTER TABLE "public"."orders" DROP COLUMN "status";
```

Drop `--dry-run` and it writes the two files:

```console
$ godwit diff --target app --dir db/migrations --schema desired.sql --name add_order_status
...
wrote db/migrations/20260908131937_add_order_status.up.sql
wrote db/migrations/20260908131937_add_order_status.down.sql
```

`--dir` is not only where the files land: godwit reads the repeatable migrations in it, so objects your `R__` files declare (here the `order_stats` view) are part of the desired schema and never come back as a proposed drop. If you have declared a `schema_source` in `godwit.yaml`, you can leave the source flag off entirely. [Concepts: generating migrations from a schema](concepts.md#generating-migrations-from-a-schema).

### `godwit lint`

**Reads your SQL files and reports what will hurt in production.** It parses each migration with a real PostgreSQL parser and flags statements that take a heavy lock, destroy data, or are otherwise a bad idea on a live table — each with the safe rewrite as copy-ready SQL.

Reach for it as the first step of a pull request check; it needs no database and no service. Do not expect it to tell you whether a migration will *work* — that is [`godwit plan`](#godwit-plan) against a real target, which replays it on a scratch database.

```console
$ godwit lint --dir db/migrations
0 finding(s), 0 blocking
```

Here is the same command on a directory holding one migration that builds an index the blocking way:

```console
$ godwit lint --dir hz
20260908160000_orders_email_unique.up.sql: error H001 CREATE INDEX without CONCURRENTLY blocks writes on orders
    -- or let godwit run it: -- godwit: add-index orders (email) name=orders_email_key unique
    CREATE UNIQUE INDEX CONCURRENTLY orders_email_key ON orders USING btree (email);
1 finding(s), 1 blocking
godwit: 1 blocking finding(s)
```

The indented lines are the *recipe*: the same change written safely. `--format markdown` produces the same thing as a table with the recipes in `<details>` blocks, which is what the GitHub Action posts on the pull request. A finding you have decided to live with is acknowledged with `--ack H001`, and the run has to carry the same acknowledgement. Codes: `H001`–`H010` for hazards, `E001`–`E005` for errors, `W001`–`W002` for warnings — all listed in [configuration](configuration.md#local-no-service). [Concepts: hazards](concepts.md#hazards).

### `godwit plan`

**Shows you what would happen to the database.** One block per object the migrations create, change or destroy, in terraform's shape: `+` creates, `-` destroys, `~` changes, at the object and at the attribute, and a hazard is written on the attribute line that causes it. `--plan-format statements` gives the other view — every statement in order, with the transaction mode and the recipes — which is what to reach for when auditing the SQL or reading a failed run. `plan.format` in `godwit.yaml` sets the default for a repository ([configuration](configuration.md#plan)).

It has three forms, in order of what they touch. `--help` lists them in the same order:

| You type | Reaches | Leaves behind |
|---|---|---|
| `godwit plan --dir db/migrations` | nothing | nothing |
| `godwit plan --target app` | the service, the target, a scratch database | nothing |
| `godwit plan --target app --save` | the same | a stored plan a later `migrate` binds to |

`--target` is a flag here and only a flag: unlike `migrate`, `plan` never reads it from `godwit.yaml`, so a bare `godwit plan` is the offline form even in a repository whose config names a target.

Without `--target` it is entirely offline — no database, no service — and prints both the up and the down side of every file:

An offline plan has no database, so there is no schema delta to describe and it prints the statements whatever `plan.format` says:

```console
$ godwit plan --dir db/migrations
Offline plan. Both sides of every migration in the directory, as written; no database was consulted, so nothing here says what is pending.

+ 20260901120000_create_orders  1 statement
  statement 0, runs inside a transaction
      CREATE TABLE orders (id bigserial PRIMARY KEY, customer_id bigint NOT NULL, total numeric NOT NULL);

- 20260901120000_create_orders (down)  1 statement
  statement 0, runs inside a transaction
      DROP TABLE orders;
      hazard H002: DROP TABLE is destructive
        -- expand then contract: ship the application version that no longer uses orders, then run this DROP TABLE as a contract migration (rollout: expand-contract)

+ 20260901120500_orders_customer_idx  1 statement
  statement 0, runs outside a transaction
      CREATE INDEX CONCURRENTLY orders_customer_idx ON orders (customer_id);
...

1 hazard must be acknowledged before this runs; use --ack H002.
Plan: 2 to apply, 2 to revert, 1 hazard(s) to acknowledge
```

`runs inside a transaction` means the statement commits with its own journal row; `runs outside a transaction` means it cannot, so it gets a write-ahead intent and a check afterwards.

With `--target` it becomes a different, more useful thing: godwit connects to that database through the service, works out which migrations are actually pending there, and replays them on a scratch database to prove they apply. It prints what it found and stores nothing — the same work `migrate --dry-run` does, and the two are interchangeable.

Add `--save` and it also **stores the result as a plan**, with a snapshot of the target. That stored plan is the durable half: a later `migrate` binds to it and refuses if the database has moved since, which is what makes the plan a reviewer approved the plan the deploy applies.

```console
$ godwit plan --target app --dir db/migrations --save
3 migrations will be applied to app.

godwit will perform the following actions:

  # 20260901120000_create_orders
  + table "public"."orders" {
      + customer_id = bigint NOT NULL
      + id          = bigint NOT NULL DEFAULT nextval('orders_id_seq'::regclass)
      + total       = numeric NOT NULL
      + orders_pkey = PRIMARY KEY (id)
    }

  # 20260901120500_orders_customer_idx
  + index "public"."orders_customer_idx" {
      + definition = CREATE INDEX orders_customer_idx ON public.orders USING btree (customer_id)
    }

  # R__order_stats
  + view "public"."order_stats"

plan details:
  target: app
  rollout: direct
  plan: f27eecc1-d479-4255-ae90-b53247e9230f

Plan: 3 to add, 0 to change, 0 to destroy.
```

A hazard is not a footnote here: it goes on the attribute that causes it, the way terraform writes `# forces replacement`, with its recipe under the block.

```console
  # 20260910120000_widen_age
  ~ table "public"."widgets" {
      ~ age     = integer -> character varying(20) # ALTER COLUMN TYPE rewrites the table under an exclusive lock (H004)
      + age_old = integer NULL
        # (4 unchanged attributes hidden)
    }
    recipe for H004:
      -- or let godwit run it: -- godwit: change-type public.widgets.age varchar(20)
      ...
```

The type carries its modifier — `character varying(20)`, `numeric(10,2)`, `timestamp(3) with time zone` — under PostgreSQL's own canonical name for it, the one `\d` prints. On a terminal `+` is green, `-` red and `~` yellow (see [colour](#colour)). The plan's key, the history and schema fingerprints and the raw observation are machine identity and stay in `--format json`, which also carries the delta under `changes`. What the target already has is not listed at all: with nothing pending the first line reads `Nothing to apply. app is at <version> (N migrations).`

A migration whose effect a schema snapshot cannot see — a seed, a `GRANT`, a function body — has no block; it says so on its own line and falls back to its statements.

Reach for the offline form to eyeball a migration you just wrote; for `--target` when you want to know whether it applies against the real thing; for `--target --save` on a pull request, so the plan a reviewer reads is the plan the deploy is bound to. Do not use any of them to check *whether anything is pending* — that is [`godwit target status`](#godwit-target-status), which is much cheaper because it replays nothing. [Concepts: plans](concepts.md#plans).

Note what `--target` costs even without `--save`: the scratch replay executes your DDL on the scratch server with the service's credentials, so it is a live operation, not a read of a file. `--save` is what adds the durable artifact and the `plan.create` audit row.

### `godwit checkpoint`

**Squashes the migrations you have accumulated into one file.** godwit replays the directory on a scratch database, dumps the schema that came out, and writes it as a single checkpoint migration. A brand-new database runs that one file instead of the hundreds below it; a database that already has them just records it and carries on.

Reach for it when a fresh developer database or a CI job spends minutes replaying history. Do not reach for it if you still want to revert any of the collapsed migrations — the checkpoint has no down side, and the files it collapses can no longer be undone. `--dry-run` prints it without writing anything.

```console
$ godwit checkpoint --dir db/migrations --name orders_baseline --dry-run
checkpoint 20260908150001_orders_baseline collapses 5 migration(s), 20260901120000_create_orders through 20260908150000_create_notes
they stay in the directory, they are never replayed again, and they can no longer be reverted

-- godwit: checkpoint through=20260908150000
-- 5 migrations, 20260901120000_create_orders through 20260908150000_create_notes.
-- A target that has applied any of them records this file; one with no history runs it instead of them.

CREATE SEQUENCE "public"."orders_id_seq"
	AS bigint
	INCREMENT BY 1
	MINVALUE 1 MAXVALUE 9223372036854775807
	START WITH 1 CACHE 1 NO CYCLE;
CREATE TABLE "public"."orders" (
	"id" bigint DEFAULT nextval('orders_id_seq'::regclass) NOT NULL,
	"customer_id" bigint NOT NULL,
	"status" text COLLATE "pg_catalog"."default" DEFAULT 'new'::text NOT NULL,
	CONSTRAINT "orders_pkey" PRIMARY KEY ("id")
);
CREATE INDEX orders_customer_idx ON public.orders USING btree (customer_id);
ALTER SEQUENCE "public"."orders_id_seq" OWNED BY "public"."orders"."id";
```

[Concepts: checkpoints](concepts.md#checkpoints), and the operational side in [operations](operations.md#checkpoints).

---

## Applying it

### `godwit migrate`

**The one that actually changes a production database.** It sends the migration directory to the service, which checks it, queues a run, and executes it on the named target; your terminal streams the run's state and exits when it settles.

Reach for it from CI, or from your shell for a database the service manages. Do not reach for it for your own laptop database — that is [`godwit up`](#godwit-up), which needs no service.

```console
$ godwit migrate --target app --dir db/migrations
plan f27eecc1-d479-4255-ae90-b53247e9230f: bound
run 7071c5ac-93e5-43de-9322-960a47d47f00: queued
run 7071c5ac-93e5-43de-9322-960a47d47f00: succeeded (attempt 1)
```

The first line tells you which reviewed plan this run is bound to. When no plan was stored for these files it says `no stored plan for this set: implicit plan` and plans on the spot; when the same CI job runs twice it says `re-attached to run <id>` and follows the existing run instead of queueing a second one.

`--dry-run` does everything except queue the run — including the scratch replay — and prints what would happen:

```console
$ godwit migrate --target app --dir db/migrations --dry-run --plan-format statements
3 migrations will be applied to app.

+ 20260901120000_create_orders  1 statement
  statement 0, runs inside a transaction
      CREATE TABLE orders (id bigserial PRIMARY KEY, customer_id bigint NOT NULL, total numeric NOT NULL);

+ 20260901120500_orders_customer_idx  1 statement
  statement 0, runs outside a transaction
      CREATE INDEX CONCURRENTLY orders_customer_idx ON orders (customer_id);

+ R__order_stats  1 statement
  statement 0, runs inside a transaction
      CREATE OR REPLACE VIEW order_stats AS SELECT customer_id, count(*) AS orders FROM orders GROUP BY customer_id;

plan details:
  target: app
  rollout: direct

Plan: 3 to apply, 0 to revert, 0 hazard(s) to acknowledge
```

`--skip-validation`, or a service started with `--skip-validation`, adds a `Not validated.` line above the plan: nothing replayed the SQL anywhere, so nothing has proved it applies — and nothing read the schema it leaves behind either, so the report falls back to the statements above whatever `plan.format` says.

When a statement fails, the run stops and so does the command, with a non-zero exit:

```console
$ godwit migrate --target app --dir db/migrations
no stored plan for this set: implicit plan
run 9c60b73c-ac42-42ec-9194-498a2c66cbc3: queued
run 9c60b73c-ac42-42ec-9194-498a2c66cbc3: failed (attempt 1): sql: statement 0 of 20260908150000_create_notes (up): exec: ERROR: relation "notes" already exists (SQLSTATE 42P07)
godwit: run 9c60b73c-ac42-42ec-9194-498a2c66cbc3 failed: sql: statement 0 of 20260908150000_create_notes (up): exec: ERROR: relation "notes" already exists (SQLSTATE 42P07)
```

Nothing is left half-applied: the statements that succeeded are journalled as done, and [`godwit run resume`](#godwit-run-resume) continues from there once you have fixed the cause.

Three flags change the shape of the run rather than a detail of it: `--rollout expand-contract` splits it in two ([below](#godwit-run-confirm)), `--to <version>` stops it at a version and reports the rest as *withheld* ([concepts](concepts.md#version-targets)), and `--plan <id>` binds a specific reviewed plan, which is how the GitHub Action applies exactly what the reviewer saw ([CI/CD](ci-cd.md)).

### `godwit run confirm`

**Releases the second half of a two-phase migration.** With `--rollout expand-contract`, godwit runs only the additive statements and then parks the run in `awaiting_contract`; the destructive ones — the drops, the renames — wait until a human says go. That human runs `confirm`.

```console
$ godwit migrate --target app --dir db/migrations --rollout expand-contract
no stored plan for this set: implicit plan
run dc21ac88-b8d9-4d42-b63b-5bdab184bb19: queued
run dc21ac88-b8d9-4d42-b63b-5bdab184bb19: awaiting_contract (attempt 1)

$ godwit run confirm --latest --target app
run dc21ac88-b8d9-4d42-b63b-5bdab184bb19: contract confirmed
run dc21ac88-b8d9-4d42-b63b-5bdab184bb19: queued
run dc21ac88-b8d9-4d42-b63b-5bdab184bb19: succeeded (attempt 1)
```

Reach for it once the application version that no longer needs the old column is deployed everywhere. Do not reach for it right after the expand phase to "finish the job" — the wait is the entire point. With a run id it confirms that run; with `--latest --target app` it finds the one waiting, which is what a deploy pipeline uses (add `--allow-none` so a pipeline with nothing to confirm exits 0). [Concepts: rollout policies](concepts.md#rollout-policies).

### `godwit revert`

**Undoes exactly what one earlier run applied** — the down side of those migrations, newest first — and nothing else. Not the rest of the directory, not migrations someone else applied.

Reach for it when a migration went in and turned out to be wrong. Do not reach for it as a routine rollback step: godwit's production path is roll forward, and a revert whose plan would drop a table or column that still holds rows is refused outright unless you pass `--allow-data-loss`.

It prints the plan before it runs anything, and refuses while any hazard in that plan is unacknowledged:

```console
$ godwit revert --target app --dry-run
godwit: unacknowledged hazards (pass acknowledge_hazards to accept):
H002: DROP TABLE is destructive

$ godwit revert --target app --ack H002
revert of run 9c60b73c-ac42-42ec-9194-498a2c66cbc3 on app: 1 migration(s), reverse order of application
  20260908150000_create_notes (down): 1 statement(s)
    statement 0, runs inside a transaction
      DROP TABLE notes
run 635b0cda-d61f-4beb-9550-1adb6da14e3b: queued
run 635b0cda-d61f-4beb-9550-1adb6da14e3b: succeeded (attempt 1)
```

With no run id it takes the newest un-reverted run on `--target`; an older one needs `--force`. `--dry-run` prints the plan and queues nothing. [Concepts: revert](concepts.md#revert), [runbook: reverting a run](runbook.md#reverting-a-run).

### `godwit run resume`

**Puts a stopped run back in the queue.** A run that failed, or that gave up after too many attempts and parked itself as `needs_attention`, keeps everything it already journalled; resuming it continues from the first statement that never committed.

Reach for it after you have fixed the cause — the lock is gone, the disk has space, the conflicting object has been dropped. Do not reach for it as a way of retrying blindly: if the cause is still there the run will fail again in the same place.

```console
$ godwit run resume 9c60b73c-ac42-42ec-9194-498a2c66cbc3
run 9c60b73c-ac42-42ec-9194-498a2c66cbc3: resumed

$ godwit run watch 9c60b73c-ac42-42ec-9194-498a2c66cbc3
run 9c60b73c-ac42-42ec-9194-498a2c66cbc3: queued
run 9c60b73c-ac42-42ec-9194-498a2c66cbc3: running (attempt 1)
run 9c60b73c-ac42-42ec-9194-498a2c66cbc3: succeeded (attempt 1)
```

`resume` returns as soon as the run is queued; the service picks it up on its next tick, which is why the example follows it with [`run watch`](#godwit-run-watch). [Runbook: run in `needs_attention`](runbook.md#run-in-needs_attention).

### `godwit up`

**Applies a migration directory straight to a database.** Same executor, same journal, same crash safety as the service — just no service, no target registration and no plan. You give it a connection string.

Reach for it on your own machine and in tests. Do not reach for it for a database the service manages: what `up` writes goes into that database's own journal but not into the service's ledger, and the two will disagree until you run [`target adopt --from-journal`](#godwit-target-adopt).

```console
$ godwit up --dsn postgres://app:app@localhost/app_dev --dir db/migrations
20260901120000_create_orders: applied (1 statement(s))
20260901120500_orders_customer_idx: applied (1 statement(s))
R__order_stats: applied (1 statement(s))
```

Keep the password out of your shell history and your process list with `GODWIT_DSN` instead of `--dsn`.

### `godwit down`

**Undoes one migration, on the database you point it at.** You give it a version; it runs that migration's down side and removes it from the journal.

Reach for it while you are iterating on a migration locally. Do not reach for it against anything shared — it is deliberately unguarded compared to [`revert`](#godwit-revert), which is the production path, and it refuses to run at all without `--yes`.

```console
$ godwit down --dsn postgres://app:app@localhost/app_dev --dir db/migrations --version 20260901120500 --yes
20260901120500_orders_customer_idx: reverted (1 statement(s))
```

A repeatable migration has no version and cannot be named here; a checkpoint has no inverse and is refused by name.

---

## Looking at what happened

### `godwit status`

**Asks a database directly what it has applied.** No service; it reads the journal inside the database itself and compares it against the directory.

```console
$ godwit status --dsn postgres://app:app@localhost/app_dev --dir db/migrations
20260901120000_create_orders: applied 2026-09-08T13:19:08Z
20260901120500_orders_customer_idx: applied 2026-09-08T13:19:08Z
R__order_stats: unchanged since 2026-09-08T13:19:08Z
```

`applied <ts> (checksum drift!)` means the file on disk was edited after it was applied. For a repeatable, `unchanged since` means its content still matches what the database recorded, so a run would skip it.

### `godwit target status`

**The same question, for a database the service manages** — plus the things only the service knows: the last run, the drift baseline, and the target's own timeout settings.

Reach for it before a deploy to see what is pending. It is the cheapest way to answer "is there anything to apply?" — much cheaper than `plan`, because it does not replay anything.

```console
$ godwit target status app --dir db/migrations
target app: provider static, lock timeout none, statement timeout none, search path none
applied (3):
  20260901120000_create_orders        2026-09-08T13:19:11Z
  20260901120500_orders_customer_idx  2026-09-08T13:19:11Z
  R__order_stats                      2026-09-08T13:19:11Z  unchanged
last run: 7071c5ac-93e5-43de-9322-960a47d47f00 migrate succeeded finished 2026-09-08T13:19:11Z
ready plans: 0
drift baseline: taken 2026-09-08T13:19:11Z by run 7071c5ac-93e5-43de-9322-960a47d47f00
```

Before that first run it would have listed three under `pending (3):` instead. `none` for a timeout means nothing is registered, not that there is no limit — see [configuration](configuration.md#target-settings). [Concepts: target status](concepts.md#target-status).

### `godwit targets`

**One line per database the service manages,** without connecting to any of them. Provider, how many migrations it has, plans waiting, runs needing a human, whether its schema has drifted, and its settings.

```console
$ godwit targets
NAME    PROVIDER  APPLIED  READY PLANS  NEEDS YOU  DRIFT  SEARCH PATH  LOCK  STATEMENT  REQUIRE PLAN  LAST RUN
app     static    5        0            0          clean  none         none  none       false         9c60b73c-ac42-42ec-9194-498a2c66cbc3 failed
dev     static    2        0            0          clean  none         none  none       false         3984dd6f-fce0-4a32-9af2-60c746dc92cd succeeded
legacy  static    2        0            0          clean  none         none  none       false         c6f96172-c7d5-4d19-9f5e-cd9f2e1bf380 succeeded
```

`APPLIED` counts versioned migrations only, so it is a smaller number than the `applied (N)` in `target status`, which also counts repeatables.

### `godwit migrations`

**A grid of every migration against every database.** Rows are migrations, columns are targets, cells say when that target got it. It answers "is staging ahead of production, and by what?" in one screen, from the service's ledger alone — nothing is connected to.

The row key is the version **and** the checksum, so one version that means two different things on two databases shows up as two rows rather than quietly agreeing.

```console
$ godwit migrations
MIGRATION                           CHECKSUM  APP         DEV         LEGACY
20260901120000_create_orders        1b551cd1  2026-09-08  2026-09-08  2026-09-08
20260901120500_orders_customer_idx  2a7b2b53  2026-09-08  -           -
20260908131937_add_order_status     0a99ed8f  2026-09-08  -           -
20260908140000_drop_orders_total    1b39142c  2026-09-08  -           -
R__order_stats                      38a877e0  2026-09-08  2026-09-08  2026-09-08

5 migrations, 3 targets: 3 not on every target, 0 under more than one checksum
key: - not there yet · missing the target is past it · differs another checksum · * recorded by a checkpoint
```

`--in app --not-in legacy` narrows it to what one database has and another does not — the release-notes question:

```console
$ godwit migrations --in app --not-in legacy
MIGRATION                           CHECKSUM  APP         DEV  LEGACY
20260901120500_orders_customer_idx  2a7b2b53  2026-09-08  -    -
20260908131937_add_order_status     0a99ed8f  2026-09-08  -    -
20260908140000_drop_orders_total    1b39142c  2026-09-08  -    -
```

[Concepts: the fleet view](concepts.md#the-fleet-view).

### `godwit runs`

**Every attempt the service has made, newest first.** Add `--target` to narrow it to one database.

```console
$ godwit runs --target app
ID                                    TARGET  KIND     STATE      ROLLOUT          PHASE     BY     SOURCE  CREATED
635b0cda-d61f-4beb-9550-1adb6da14e3b  app     migrate  succeeded  direct           expand    admin          2026-09-08T13:20:02Z
9c60b73c-ac42-42ec-9194-498a2c66cbc3  app     migrate  reverted   direct           expand    admin          2026-09-08T13:19:42Z
dc21ac88-b8d9-4d42-b63b-5bdab184bb19  app     migrate  succeeded  expand-contract  contract  admin          2026-09-08T13:19:38Z
7071c5ac-93e5-43de-9322-960a47d47f00  app     migrate  succeeded  direct           expand    admin          2026-09-08T13:19:09Z
```

`reverted` on the second row means a later run undid it; `contract` in the `PHASE` column means that expand-contract run got all the way through its second half.

### `godwit run get`

**Everything about one attempt.** What it applied and when, which plan it was bound to, who started it, and — if it stopped — the exact statement and the PostgreSQL error.

Reach for it when something failed and you need the reason, before deciding between [`run resume`](#godwit-run-resume) and [`revert`](#godwit-revert).

```console
$ godwit run get 9c60b73c-ac42-42ec-9194-498a2c66cbc3
run 9c60b73c-ac42-42ec-9194-498a2c66cbc3: failed (attempt 1): sql: statement 0 of 20260908150000_create_notes (up): exec: ERROR: relation "notes" already exists (SQLSTATE 42P07)
  target: app
  kind: migrate
  rollout: direct
  phase: expand
  reverts:
  lock_timeout:
  statement_timeout:
  created_by: admin
  source:
  plan:
  created: 2026-09-08T13:19:42Z
  finished: 2026-09-08T13:19:43Z
  applied: none
```

### `godwit run watch`

**Follows a run that is still going** and prints each state change until it settles, then exits with the run's outcome — 0 for success, 1 for `failed` or `needs_attention`.

Reach for it when you started a run somewhere else — from the UI, from another pipeline, or with `run confirm --no-wait` — and want to block on it. You do not need it after a plain `migrate`, which already streams.

```console
$ godwit run watch 9c60b73c-ac42-42ec-9194-498a2c66cbc3
run 9c60b73c-ac42-42ec-9194-498a2c66cbc3: queued
run 9c60b73c-ac42-42ec-9194-498a2c66cbc3: running (attempt 1)
run 9c60b73c-ac42-42ec-9194-498a2c66cbc3: succeeded (attempt 1)
```

### `godwit plans`

**The reviewed plans stored for one database, newest first,** with what each one would apply and whether a run has already used it.

```console
$ godwit plans --target app
ID                                    STATE  ROLLOUT  PENDING  VALIDATED  BY     SOURCE  CREATED               RUN
f27eecc1-d479-4255-ae90-b53247e9230f  ready  direct   3        true       admin          2026-09-08T13:19:09Z
```

`ready` means nothing has bound it yet; once a run does, it becomes `bound` and names that run in the last column.

### `godwit plan show`

**One stored plan in full:** the statements, the hazards and their recipes, what the database looked like when the plan was made, whether it has drifted since, and which run applied it.

Reach for it to answer "what exactly was approved?" long after the pull request is closed.

```console
$ godwit plan show f27eecc1-d479-4255-ae90-b53247e9230f --plan-format statements
3 migrations will be applied to app.

+ 20260901120000_create_orders  1 statement
  statement 0, runs inside a transaction
      CREATE TABLE orders (id bigserial PRIMARY KEY, customer_id bigint NOT NULL, total numeric NOT NULL);
...

plan details:
  target: app
  rollout: direct
  plan: f27eecc1-d479-4255-ae90-b53247e9230f
  state: bound (run 7071c5ac-93e5-43de-9322-960a47d47f00)
  by: admin at 2026-09-08T13:19:09Z

Plan: 3 to apply, 0 to revert, 0 hazard(s) to acknowledge
```

The snapshot the plan was made against — how many migrations the target had, and the fingerprints of its history and its schema — is what a stale `migrate` disagreed with. It is machine identity, so `--format json` is where to read it: `plan_key` and `observed`.

### `godwit audit`

**Who asked godwit to do what.** One row per mutating call — a target registered, a plan created, a run queued, a drift accepted — with the token name behind it.

```console
$ godwit audit --limit 6
AT                    ACTOR  ACTION           TARGET  RUN                                   DETAIL
2026-09-08T13:19:09Z  admin  run.create       app     7071c5ac-93e5-43de-9322-960a47d47f00  rollout=direct migrations=3 acked= source= plan=f27eecc1-d479-4255-ae90-b53247e9230f
2026-09-08T13:19:09Z  admin  plan.create      app                                           plan=f27eecc1-d479-4255-ae90-b53247e9230f key=4ad4fbb546cb1d6910cacd27e4a8831180649d476ade99808c5e114870708f63 rollout=direct pending=3 acked= source=
2026-09-08T13:19:08Z  admin  target.register  app                                           provider=static lock_timeout= statement_timeout= require_plan=false search_path=
```

`--target` and `--run` narrow it. [Concepts: actors and provenance](concepts.md#actors-and-provenance), [security: audit](security.md#audit).

---

## Managing the databases godwit migrates

### `godwit target add`

**Registers a database with the service under a name.** From then on every other command says `--target app` instead of a connection string, and nobody handling the CLI needs the password.

You also choose here *where the password comes from*: `static` stores the DSN encrypted in the service's own database, `kubernetes` reads it from a mounted secret file at connect time, `vault` fetches it from Vault — including short-lived credentials Vault generates per connection.

Reach for it once per database. Note that it is a full replace, not a patch: running it again with fewer flags resets the settings you left out.

```console
$ godwit target add app --provider static --dsn postgres://app:app@localhost/app
target app: registered (static)
```

Per-target settings live on this command too — `--lock-timeout`, `--statement-timeout`, `--search-path`, `--require-plan` — and are listed in [configuration](configuration.md#target-settings). Use `GODWIT_TARGET_DSN` rather than `--dsn` to keep the password out of the process list. [Deployment: registering a target](deployment.md#registering-a-target), [security: credential providers](security.md#credential-providers).

### `godwit target adopt`

**Puts the migrations a database already has on the books, without running them.** godwit records a succeeded run holding those migrations and takes a drift snapshot; not one statement is executed, and future runs start from the next migration.

There is one thing to decide, and the flags name it: **where the truth comes from.** Exactly one of the two is required, and godwit refuses rather than picks.

| Flag | Truth comes from | Use it when |
|---|---|---|
| `--version <N>` | you | the database was migrated by something else — Flyway, a shell script, hand-typed DDL — so there is no godwit journal to read |
| `--from-journal` | the target's own `godwit` journal | godwit migrated this database before, just not through this service |

```console
$ godwit target adopt legacy --dir db/migrations --version 20260901120000
target legacy: adopted through version 20260901120000, nothing executed (run c6f96172-c7d5-4d19-9f5e-cd9f2e1bf380)
```

```console
$ godwit target adopt dev --dir db/migrations --from-journal
target dev: adopted 2 migration(s) from its journal (run 3984dd6f-fce0-4a32-9af2-60c746dc92cd): 20260901120000_create_orders, R__order_stats
```

The two failure modes are not symmetric, which is the reason to prefer `--from-journal` when you can have it. Getting `--version` wrong hurts in either direction: too low and godwit will try to re-run migrations the database already has, too high and it will silently skip ones it does not. `--from-journal` has no version for you to get wrong, and it refuses on a named disagreement between the journal and the directory rather than guessing.

Reach for `--from-journal` after using [`godwit up`](#godwit-up) on a database you later want the service to manage, or when the service's ledger was lost and the databases were not — `migrate` and `plan --target` refuse such a target by name until you have. [Deployment: adopting an existing database](deployment.md#adopting-an-existing-database), [runbook: the ledger is behind a target](runbook.md#the-ledger-is-behind-a-target).

### `godwit drift check`

**Shows what changed in a database that godwit did not change.** After each successful run godwit fingerprints the schema; `drift check` takes a fresh fingerprint and diffs it against the stored one. A hotfix someone applied by hand at 3am shows up here.

The service also does this on its own every few minutes and raises an event, so you rarely need to run it — reach for it when you want the diff right now, or want to know whether a database is clean before migrating it.

```console
$ godwit drift check app
target app: drifted
+ column public.orders.hotfix_note text null=YES default=<none>
```

```console
$ godwit drift check app
target app: no drift
```

It reports, it does not fix: the diff tells you what is there, and it is up to you to write a migration for it or [accept it](#godwit-drift-accept). [Concepts: drift](concepts.md#drift), [runbook: drift detected](runbook.md#drift-detected).

### `godwit drift accept`

**Records the current schema as the new reference for the target.** It does not change the database; future checks compare against it. The same sentence is on the UI's **Accept as baseline** button, and the command now prints it.

Reach for it when you have looked at the diff and decided the change is legitimate — an emergency index you are keeping, an extension someone installed. Do not reach for it to make an alert go away: an accepted drift is a change no migration file describes, so the next database you build from those files will not have it. When you want the change to travel, write the migration instead.

```console
$ godwit drift accept app
target app: baseline accepted
recorded the current schema as the new reference for app; it does not change the database, and future checks compare against it
no migration file describes this change, so the next database built from those files will not have it
```

The word *baseline* on that first line and in [`target status`](#godwit-target-status) is the drift reference, and only that. Putting migration history on the books is [`target adopt`](#godwit-target-adopt).

---

## Operating the service

### `godwit serve`

**Runs the service.** One process holding the state store, the scheduler that executes runs, the drift monitor and the API. Run several and they share the work: each run is claimed under a lease, and a replica that dies has its runs taken over by another.

It needs a PostgreSQL database of its own — the *store*, which is not any of the databases it migrates — and a set of bearer tokens.

```console
$ export GODWIT_MASTER_KEY=$(openssl rand -hex 32)
$ export GODWIT_TOKENS='admin:admin:s3cret-admin,ci:pipeline:s3cret-ci,oncall:operator:s3cret-ops'
$ godwit serve --store-dsn postgres://godwit:godwit@localhost/godwit_store --log-format text
time=2026-09-08T10:14:06.793-03:00 level=INFO msg="store migrated" replica=host/b2111bb933db47e3 build=dev applied=17
time=2026-09-08T10:14:06.796-03:00 level=WARN msg="validation and diff execute submitted DDL on the store server with the store credentials; set --scratch-dsn to a throwaway PostgreSQL that holds nothing" replica=host/b2111bb933db47e3 build=dev
time=2026-09-08T10:14:06.797-03:00 level=INFO msg=listening replica=host/b2111bb933db47e3 build=dev addr=[::]:8474 validation=true
```

That warning is worth acting on: without `--scratch-dsn`, the throwaway databases used to validate submitted SQL are built on the store server with the store's own credentials, which means submitted DDL runs there. Point it at a PostgreSQL that holds nothing. `--ui` adds an operator web UI at `/ui`. Every flag and environment variable is in [configuration](configuration.md#godwit-serve); running it for real is [deployment](deployment.md) and [operations](operations.md).

### `godwit version`

**Prints the version and the commit it was built from.** `dev (none)` from a plain `go build`, `main (<commit>)` from the published image.

```console
$ godwit version
dev (none)
```

### `godwit completion`

**Prints a shell completion script** for bash, zsh, fish or PowerShell — the standard Cobra command. `godwit completion zsh > "${fpath[1]}/_godwit"`.

---

## Where to go next

- The exact flags, environment variables and required token scope for each command: [configuration](configuration.md#cli-reference).
- What the commands do underneath — the journal, hazards, directives, plans, drift: [concepts](concepts.md).
- The same commands wired into a pull request: [CI/CD](ci-cd.md).
- The same operations as HTTP calls: [API](api.md).
- Something is wrong right now: [runbook](runbook.md).
