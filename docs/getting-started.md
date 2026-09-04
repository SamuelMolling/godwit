# Getting started

Four stops: a migration directory you can plan and apply locally, the service with one registered target, a first run through it, and the same commands in CI.

## Install

The binary needs cgo because the planner links [libpg_query](https://github.com/pganalyze/pg_query_go).

```bash
go install github.com/SamuelMolling/godwit/cmd/godwit@main   # needs gcc and Go 1.26
godwit version
```

Or pull the image, published from every merge to `main` for linux/amd64 and linux/arm64 (distroless, entrypoint `/godwit`, also tagged `sha-<short commit>`):

```bash
docker run --rm ghcr.io/samuelmolling/godwit:main version
```

`docker build -t godwit:dev .` at the repo root produces the same image locally. Tagged releases will also ship binaries for macOS and Linux via `brew install SamuelMolling/tap/godwit`; until the first `v*` tag, `go install` or the image are the options.

## 1. The dev loop (no service)

Migrations are plain SQL pairs in one directory:

```
db/migrations/
├── 20260901120000_create_orders.up.sql
├── 20260901120000_create_orders.down.sql
├── 20260901120500_orders_customer_idx.up.sql
├── 20260901120500_orders_customer_idx.down.sql
├── R__order_stats.up.sql
└── R__order_stats.down.sql
```

File names must match `<14-digit timestamp>_<snake_name>.{up,down}.sql`, or `R__<snake_name>.{up,down}.sql` for a **repeatable** migration. Both sides are required and must not be empty; a version with two different names, or an unexpected file, fails the load. Entries starting with `.` are ignored.

A repeatable has no version: it runs after the versioned files of the same run, in name order, whenever its content differs from what the target last recorded under that name, and is skipped when it does not. Use it for objects the file describes in full — a view, a function, a trigger body — and write both sides so they can run more than once:

```sql
-- R__order_stats.up.sql
CREATE OR REPLACE VIEW order_stats AS SELECT customer_id, count(*) AS orders FROM orders GROUP BY customer_id;
-- R__order_stats.down.sql
DROP VIEW IF EXISTS order_stats;
```

The down side runs only when the run that applied the repeatable is reverted; godwit does not keep previous bodies, so roll forward by editing the file. See [concepts: repeatable migrations](concepts.md#repeatable-migrations). `godwit diff` reads these files too, so the objects they declare are part of the desired schema and never come back as a proposed drop.

```sql
-- 20260901120000_create_orders.up.sql
CREATE TABLE orders (id bigserial PRIMARY KEY, customer_id bigint NOT NULL, total numeric NOT NULL);
-- 20260901120000_create_orders.down.sql
DROP TABLE orders;

-- 20260901120500_orders_customer_idx.up.sql
CREATE INDEX CONCURRENTLY orders_customer_idx ON orders (customer_id);
-- 20260901120500_orders_customer_idx.down.sql
DROP INDEX CONCURRENTLY orders_customer_idx;
```

See what godwit would do with them, offline:

```bash
godwit plan --dir db/migrations
```

```
20260901120000_create_orders (up): 1 statement(s)
  [0] tx    CREATE TABLE orders (id bigserial PRIMARY KEY, customer_id bigint NOT NULL, total numeric NOT NULL)
20260901120000_create_orders (down): 1 statement(s)
  [0] tx    DROP TABLE orders
        hazard H002: DROP TABLE is destructive
          -- expand then contract: ship the application version that no longer uses orders, then run this DROP TABLE as a contract migration (rollout: expand-contract)
20260901120500_orders_customer_idx (up): 1 statement(s)
  [0] no-tx CREATE INDEX CONCURRENTLY orders_customer_idx ON orders (customer_id)
...
```

`tx` statements run inside a transaction with the journal write; `no-tx` statements (`CREATE INDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`, `VACUUM`, `REFRESH MATERIALIZED VIEW CONCURRENTLY`, `REINDEX CONCURRENTLY`) get a write-ahead intent and a verifier instead. Hazards are the codes a run must acknowledge; the indented lines under each one are its recipe, the safe form as SQL built from the statement's own names ([concepts: hazards](concepts.md#hazards)).

Lint the directory the way a pull request gate does, and apply against a local database. Create two: `app_dev` for this loop, and `app` for the service to manage from section 2 onwards. Keep them apart — what `godwit apply` writes goes into the target's own journal and not into the service's ledger, so a database you have already migrated by hand is not a database to register as a target.

```bash
psql -U postgres -c "CREATE ROLE app LOGIN PASSWORD 'app'" \
                 -c "CREATE DATABASE app_dev OWNER app" -c "CREATE DATABASE app OWNER app"
```

```bash
godwit lint --dir db/migrations                       # exit 1 on unacknowledged hazards, parse errors, empty files
godwit apply --dsn postgres://app:app@localhost/app_dev --dir db/migrations
godwit status --dsn postgres://app:app@localhost/app_dev --dir db/migrations
godwit down --dsn postgres://app:app@localhost/app_dev --dir db/migrations --version 20260901120500 --yes
```

```
$ godwit lint --dir db/migrations
0 finding(s), 0 blocking
$ godwit apply --dsn postgres://app:app@localhost/app_dev --dir db/migrations
20260901120000_create_orders: applied (1 statement(s))
20260901120500_orders_customer_idx: applied (1 statement(s))
R__order_stats: applied (1 statement(s))
$ godwit status --dsn postgres://app:app@localhost/app_dev --dir db/migrations
20260901120000_create_orders: applied 2026-09-04T17:50:24Z
20260901120500_orders_customer_idx: applied 2026-09-04T17:50:24Z
R__order_stats: unchanged since 2026-09-04T17:50:24Z
$ godwit down --dsn postgres://app:app@localhost/app_dev --dir db/migrations --version 20260901120500 --yes
20260901120500_orders_customer_idx: reverted (1 statement(s))
```

`apply` is the same executor the service uses: it takes the advisory lock, creates the `godwit` schema in the target, and journals every statement. Kill it mid-way and run it again; it resumes from the last `done` row.

## 2. The service

The service needs a PostgreSQL database of its own (the *store*) and two secrets.

```bash
psql -U postgres -c "CREATE ROLE godwit LOGIN PASSWORD 'godwit' CREATEDB" \
                 -c "CREATE DATABASE godwit_store OWNER godwit"
export GODWIT_MASTER_KEY=$(openssl rand -hex 32)          # seals static target DSNs at rest; only static targets need it
export GODWIT_TOKENS='admin:admin:s3cret-admin,ci:pipeline:s3cret-ci,oncall:operator:s3cret-ops'
godwit serve --store-dsn postgres://godwit:godwit@localhost/godwit_store --listen :8474
```

```
{"time":"...","level":"INFO","msg":"store migrated","replica":"host","build":"dev","applied":16}
{"time":"...","level":"INFO","msg":"listening","replica":"host","build":"dev","addr":"[::]:8474","validation":true}
```

`serve` migrates the store schema, starts the leased scheduler, the drift monitor and the scratch-database validator, then listens for gRPC and JSON on one port (plus `/metrics`, `/healthz`, `/readyz`).

Between those two lines it also prints these three, and they mean what they say:

```
{"time":"...","level":"WARN","msg":"scratch database is not isolated","detail":"scratch role godwit owns the store database \"godwit_store\", so submitted DDL can DROP DATABASE ... WITH (FORCE)"}
{"time":"...","level":"WARN","msg":"scratch database is not isolated","detail":"scratch role godwit may CONNECT to the store database \"godwit_store\": REVOKE CONNECT ON DATABASE \"godwit_store\" FROM PUBLIC and from this role"}
{"time":"...","level":"WARN","msg":"validation and diff execute submitted DDL on the store server with the store credentials; set --scratch-dsn to a throwaway PostgreSQL that holds nothing"}
```

Validation and `godwit diff` execute the SQL a caller submits, to find out what it produces. Without `--scratch-dsn` that happens on the store server as the store role, which is fine on a laptop and wrong anywhere a token is shared. Point it at a second PostgreSQL with a role that owns nothing else, and the store role stops needing `CREATEDB` ([security: the scratch database](security.md#the-scratch-database)):

```bash
psql -U postgres -h scratch-host -c \
  "CREATE ROLE godwit_scratch LOGIN PASSWORD 'scratch' CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS"
godwit serve --store-dsn postgres://godwit:godwit@localhost/godwit_store \
             --scratch-dsn postgres://godwit_scratch:scratch@scratch-host/postgres --listen :8474
```

Register a target. The DSN is sealed with the master key and never printed again:

```bash
export GODWIT_SERVER=http://localhost:8474
export GODWIT_TOKEN=s3cret-admin
godwit target add app --provider static --dsn postgres://app:app@localhost/app --lock-timeout 5s
```

`target add` needs the `admin` scope; the `kubernetes` and `vault` providers avoid storing a DSN at all ([security: providers](security.md#credential-providers)).

## 3. First run

```bash
export GODWIT_TOKEN=s3cret-ci
godwit migrate --target app --dir db/migrations
```

```
no stored plan for this set: implicit plan
run 2cc06d64-7721-4ec3-8417-bcf51299b688: queued
run 2cc06d64-7721-4ec3-8417-bcf51299b688: running (attempt 1) [statement 0 of 20260901120000_create_orders]
run 2cc06d64-7721-4ec3-8417-bcf51299b688: succeeded (attempt 1)
```

The first line is the plan binding: this run had no stored plan to bind to, so it planned itself. [Section 3c](#3c-plan-on-the-pull-request-apply-from-it) is the other way round. The trailing bracket on a `running` line is how far the run had got when the state last changed, so a slow migration shows which statement it is on; a run this small may settle before any `running` line is streamed at all.

`migrate` sends every file in the directory, waits for admission (hazard gate, out-of-order guard, scratch replay of the target's whole history plus the new files), then streams the run until it settles. Exit code 0 on `succeeded` or `awaiting_contract`, 1 on `failed`, `needs_attention` or any refusal. A run with an unacknowledged hazard is refused before anything is queued:

```
unacknowledged hazards (pass acknowledge_hazards to accept):
H001: CREATE INDEX without CONCURRENTLY blocks writes on orders
```

Hazards are reported for the direction being planned; a normal `migrate` plans the up side only, so the `DROP TABLE` in the down file above is not in the way (it will be when that run is reverted). Acknowledge with `--ack H001` when you do mean it.

To land a branch one migration at a time, stop the run at a version instead of editing the directory. With two new files in the directory, `20260904181500_orders_source` and `20260904182000_orders_channel`:

```
$ godwit plan --target app --dir db/migrations --to 20260904181500
plan e6d08852-34ff-42a9-a327-b4e193c80640 on app (rollout direct, validated on a scratch database)
key: bc051b2c60b53c2d31ffa945f98c369c08da213b3c4a890ff8ccab7b8731ae53
observed: 5 applied, newest 20260904181000, history 369a12ef…, schema ec575f3f…, at 2026-09-04T18:03:08Z
withheld: 2 migration(s) in the directory this plan does not cover (20260904182000_orders_channel, R__order_stats)
...
20260904181500_orders_source (up): 1 statement(s) [expand, pending]
  [0] tx    ALTER TABLE orders ADD COLUMN source text
20260904182000_orders_channel (up): 0 statement(s) [withheld]

$ godwit migrate --target app --dir db/migrations --to 20260904181500
plan e6d08852-34ff-42a9-a327-b4e193c80640: bound
run b10197aa-8d27-4cdb-87fc-1d7adfd3d1c9: queued
run b10197aa-8d27-4cdb-87fc-1d7adfd3d1c9: succeeded (attempt 1)
```

The migrations above the version stay in the directory and in the plan, marked `withheld`, so the report cannot be read as the whole set; the next `migrate` without `--to` applies them. The withheld list is on the plan, not on the run: `migrate` streams the run and prints no plan, so read `plan --to` (or `migrate --to --dry-run`) when the set matters. A version below what the target has already applied is refused — `--to` stops a run short, it never reverts ([concepts](concepts.md#version-targets)).

Look around:

```bash
godwit targets                                   # every registered target, without connecting to any of them
godwit runs --target app
godwit run get <run-id>
godwit run watch <run-id>                        # streams until it settles; exits 1 on failed / needs_attention
godwit target status app --dir db/migrations     # the target compared against the files on disk
godwit audit --target app
```

```
$ godwit targets
NAME  PROVIDER  APPLIED  READY PLANS  NEEDS YOU  DRIFT  SEARCH PATH  LOCK  STATEMENT  REQUIRE PLAN  LAST RUN
app   static    3        0            0          clean  none         5s    none       false         2cc06d64-… succeeded

$ godwit target status app --dir db/migrations
target app: provider static, lock timeout 5s, statement timeout none, search path none
applied (3):
  20260901120000_create_orders        2026-09-04T17:50:27Z
  20260901120500_orders_customer_idx  2026-09-04T17:50:27Z
  R__order_stats                      2026-09-04T17:50:27Z  unchanged
last run: 2cc06d64-7721-4ec3-8417-bcf51299b688 migrate succeeded finished 2026-09-04T17:50:27Z
ready plans: 0
drift baseline: taken 2026-09-04T17:50:27Z by run 2cc06d64-7721-4ec3-8417-bcf51299b688
```

The two counts answer different questions and can differ: `targets` counts the migrations the **service's own runs** recorded, `target status` reads the **target's** journal. They agree here because the service applied everything `app` has.

With `serve --ui`, the same answers are pages: `/ui/` is the run list and the needs-you queue, `/ui/targets/app` is that status page, `/ui/plans` the stored plans. Sign in with any token's secret as the basic-auth password — the username is ignored — or run `serve` with `--ui-user` / `--ui-password` for one shared identity. What a page offers follows the scope behind the password; anything beyond it is a `403`.

## 3c. Plan on the pull request, apply from it

Two commands instead of one, so what a reviewer approved is what runs. `plan` stores the admitted plan on the service against an observation of the live target; `migrate` binds to it:

```bash
godwit plan --target app --dir db/migrations      # on the pull request
godwit migrate --target app --dir db/migrations   # after review; binds the stored plan
```

```
$ godwit plan --target app --dir db/migrations
plan 48779753-1d13-4637-a290-6639adaca3dc on app (rollout direct, validated on a scratch database)
key: 7fa0a893cf922112cc5365a626276bfdf770708ea117a7d854d70ab8df6c783b
observed: 2 applied, newest 20260901120500, history 79d14c57…, schema 814c9433…, at 2026-09-04T18:00:14Z
20260901120000_create_orders (up): 1 statement(s) [expand, applied]
  [0] tx    CREATE TABLE orders (id bigserial PRIMARY KEY, customer_id bigint NOT NULL, total numeric NOT NULL)
20260901120500_orders_customer_idx (up): 1 statement(s) [expand, applied]
  [0] no-tx CREATE INDEX CONCURRENTLY orders_customer_idx ON orders (customer_id)
20260904180008_orders_status (up): 1 statement(s) [expand, pending]
  [0] tx    ALTER TABLE "public"."orders" ADD COLUMN "status" text COLLATE "pg_catalog"."default" DEFAULT 'new'::text NOT NULL
R__order_stats (up): 1 statement(s) [expand, unchanged]
  [0] tx    CREATE OR REPLACE VIEW order_stats AS SELECT customer_id, count(*) AS orders FROM orders GROUP BY customer_id

$ godwit migrate --target app --dir db/migrations
plan 48779753-1d13-4637-a290-6639adaca3dc: bound
run 5bd4a4f6-2e7f-4014-af6b-ce5601604b8a: queued
run 5bd4a4f6-2e7f-4014-af6b-ce5601604b8a: succeeded (attempt 1)
```

The plan covers the whole directory, so it lists what the target already has as `applied` and `unchanged` beside the one migration that is `pending`; only the pending ones are run.

If the target moves between the two, `migrate` refuses with the diff and exits 3 instead of applying something nobody reviewed. `godwit target add --require-plan` (or `serve --require-plan`) makes the stored plan mandatory, and `godwit plans` / `godwit plan show <id>` read them back.

## 3d. Let godwit write the lock-safe SQL

A `-- godwit: <op>` comment line states the intent and godwit renders the lock-safe statements against the real catalog at plan time. A type change, whose safe form is a dozen statements nobody wants to hand-write:

```sql
-- 20260904180100_total_cents.up.sql
-- godwit: change-type orders.total bigint using='(total * 100)::bigint'
-- 20260904180100_total_cents.down.sql
-- godwit: revert
```

`godwit lint` parses it offline; `godwit plan --target` shows the expansion, split into the phases it will run in:

```
20260904180100_total_cents (up): 12 statement(s) [expand, pending]   directive, expand 6 / contract 6
  -- godwit: change-type orders.total bigint using='(total * 100)::bigint'
  -- godwit expanded: change-type orders.total bigint
  [0] tx    ALTER TABLE public.orders ADD COLUMN total_new bigint   [expand]
  [1] tx    CREATE FUNCTION public.orders_total_sync() RETURNS trigger ...   [expand]
  [3] batch WITH b AS (SELECT id AS godwit_key FROM public.orders WHERE id > $1::bigint ...)   [expand]
        batch over id (int), 5000 rows per transaction
  [8] tx    ALTER TABLE public.orders RENAME COLUMN total TO total_old   [contract]
  note: leaves public.orders.total_old for rollback; drop it with `-- godwit: drop-column public.orders.total_old`
```

The `-- godwit expanded:` line is the expander's own marker: everything under it up to the next marker was generated by that directive rather than written by hand.

The column has to be one nothing else depends on. `change-type` swaps the column by renaming it, and PostgreSQL moves indexes, views and constraints with the physical attribute — so they would silently keep reading the pre-swap column. The expander refuses instead of doing that, by name:

```
$ godwit plan --target app --dir db/migrations   # with change-type on orders.customer_id
godwit: godwit directive on line 1 (-- godwit: change-type orders.customer_id text using='customer_id::text'):
index public.orders_customer_idx, view public.order_stats depend on public.orders.customer_id; ...
Drop and recreate them around this migration, in their own migrations
```

Under `--rollout expand-contract` the run applies the expand half and stops; `run confirm` releases the contract phase and streams it to the end:

```
$ godwit migrate --target app --dir db/migrations --rollout expand-contract
plan a9492a64-d3c7-4dcf-bec6-6fcb2c5198d0: bound
run 2076f415-7188-46ef-9f34-1f495d0fb324: queued
run 2076f415-7188-46ef-9f34-1f495d0fb324: awaiting_contract (attempt 1)
$ godwit run confirm --latest --target app        # once the application reads both shapes
run 2076f415-7188-46ef-9f34-1f495d0fb324: contract confirmed
run 2076f415-7188-46ef-9f34-1f495d0fb324: queued
run 2076f415-7188-46ef-9f34-1f495d0fb324: succeeded (attempt 1)
```

`awaiting_contract` is exit code 0, not a failure: the expand half is on the database and the swap waits for a human. `run confirm` waits for the contract phase it released and exits on its outcome — 0 on `succeeded`, 1 on `failed` or `needs_attention` — so a deploy step or a Kubernetes hook cannot go green on a contract phase that then fails; `--no-wait` returns as soon as it is queued, for a caller that watches the run elsewhere. The full directive list, the expansion rules and everything the expander refuses are in [concepts: directives](concepts.md#directives).

One directive reads instead of writing. `-- godwit: assert` states a condition about the **data** and makes it part of the plan, so the swap above is gated on the backfill having actually worked:

```sql
-- 20260904180100_total_cents.up.sql
-- godwit: change-type orders.total bigint using='(total * 100)::bigint'
-- godwit: assert 'SELECT count(*) FROM orders WHERE total_new IS DISTINCT FROM (total * 100)::bigint' = 0
```

The assertion is a statement of the plan like any other — it shows up in `godwit plan` and in the pull-request comment with the condition beside its SQL — and it runs at the end of the expand phase. If it does not hold the run fails there, `awaiting_contract` is never reached, and no swap happens. `godwit run confirm` re-checks it against the data as it is at confirm time, not as it was when the backfill finished. See [concepts: assertions](concepts.md#assertions).

## 3b. Write the next migration from a schema

When you would rather describe the database you want than the step to get there, keep a `schema.sql` with the whole DDL and let the service write the file:

```bash
godwit diff --target app --schema db/schema.sql --name orders_status --dir db/migrations
```

```
declared by repeatable migrations, so the desired schema keeps them: public.order_stats
app -> db/schema.sql: 1 statement(s)
  [0] tx    ALTER TABLE "public"."orders" ADD COLUMN "status" text COLLATE "pg_catalog"."default" DEFAULT 'new'::text NOT NULL

-- up
ALTER TABLE "public"."orders" ADD COLUMN "status" text COLLATE "pg_catalog"."default" DEFAULT 'new'::text NOT NULL;
-- down
ALTER TABLE "public"."orders" DROP COLUMN "status";
wrote db/migrations/20260904180008_orders_status.up.sql
wrote db/migrations/20260904180008_orders_status.down.sql
```

The starting point is the live target as `plan` observes it, the end point is `schema.sql` applied on an empty scratch database on the service. Hazards and recipes are printed the way `plan` prints them, a `drift` block comes first when the live schema has hand changes the history does not know about (they end up in the generated `up`), `--dry-run` prints without writing, `--json` returns `up_sql`, `down_sql`, `statements`, `drift`, `files` and `repeatable_objects`, and `no changes` exits 0 with nothing written. Read the files before committing them: the generated SQL goes through the same `lint`, `plan` and hazard gate as a hand-written one ([concepts: generating migrations from a schema](concepts.md#generating-migrations-from-a-schema) lists what the diff does and does not cover).

`--schema` takes any file with plain PostgreSQL DDL, so an ORM's schema dump works as the desired state. Seven other flags skip the dump and render the model with the project's own toolchain, next to the repository; the service only ever receives DDL.

```bash
# Prisma: the datamodel itself; the datasource provider must be postgresql
godwit diff --target app --prisma prisma/schema.prisma --name sync_prisma

# GORM: a Go package that dry-runs the migrator and prints the SQL (examples/gorm/schema/main.go)
godwit diff --target app --gorm ./cmd/schema --name sync_gorm

# Django: showmigrations --plan, then sqlmigrate for every migration, in that order
godwit diff --target app --django manage.py --name sync_django

# SQLAlchemy/Alembic: offline mode replays every revision from base, nothing connects
godwit diff --target app --alembic alembic.ini --name sync_alembic

# Rails: the committed db/structure.sql, no Ruby and no database (schema.rb is refused)
godwit diff --target app --rails . --name sync_rails

# Drizzle: drizzle-kit export prints the schema's DDL on stdout
godwit diff --target app --drizzle drizzle.config.ts --name sync_drizzle

# Anything else: your own command, its stdout is the desired schema
godwit diff --target app --exec 'atlas schema inspect --url env://dev --format "{{ sql . }}"' --name sync_orm
```

`--prisma` runs `prisma migrate diff --from-empty --script` on the file (`npx prisma` by default, so the `prisma` devDependency the project pins is what renders it; `--prisma-bin` or `GODWIT_PRISMA_BIN` names another command line, such as `node_modules/.bin/prisma --config prisma.config.ts` on Prisma 7, whose `prisma.config.ts` must set a `datasource.url`, any value, nothing connects to it). Prisma 5, 6 and 7 are supported; the CLI's own errors are surfaced as-is.

`--gorm` runs `go run <package>` and takes its stdout: GORM's dry-run migrator is a Go API over your model structs, not a CLI, so the package stays yours — copy [examples/gorm/schema/main.go](../examples/gorm/schema/main.go), point it at your models, and godwit reports a build failure with the package name and the compiler's stderr. `--django` runs `python manage.py showmigrations --plan --no-color` and one `sqlmigrate` per migration, concatenated in plan order with Django's `BEGIN;`/`COMMIT;` wrappers dropped; a `DATABASES` `ENGINE` that is not PostgreSQL is refused before any process starts, and because `sqlmigrate` introspects over the configured connection, `DATABASES` must point at a reachable PostgreSQL (`--django-database` picks the alias). `--go-bin`, `--python-bin`, `--alembic-bin` and `--drizzle-bin` (or `GODWIT_GO_BIN` / `GODWIT_PYTHON_BIN` / `GODWIT_ALEMBIC_BIN` / `GODWIT_DRIZZLE_BIN`) name the interpreter when it is not on `PATH`.

`--alembic` runs `alembic -c <alembic.ini> upgrade head --sql`, Alembic's documented offline mode: every revision from base rendered into a script with no connection open. The `BEGIN;`/`COMMIT;` wrappers are dropped; the `alembic_version` table and the rows tracking the revision are **kept**, because they exist on your target and a desired schema without them would make the first diff propose dropping your own migration history. A `sqlalchemy.url` godwit can read whose dialect is not PostgreSQL is refused before anything runs — a project that builds the URL in `env.py` declares none in the file and is not refused. Two constraints worth knowing before you point it at a real project: offline mode cannot render a revision that queries the database (`op.get_bind()`, reflection), and a plain relative `script_location` is resolved against the working directory, so write `script_location = %(here)s/alembic` or run `godwit diff` from the project root. `uv run alembic` and `poetry run alembic` work as `--alembic-bin`.

`--rails` reads the `db/structure.sql` your application already commits — no Ruby, no `pg_dump`, no database. It takes the application root (or the dump itself when it lives elsewhere), and strips what could not be replayed: the `\restrict` / `\unrestrict` psql meta-commands recent `pg_dump` emits, the session `SET` preamble and the `SET search_path` Rails appends, and the trailing `INSERT INTO "schema_migrations"` rows. `db/schema.rb` is **refused**: it is a Ruby DSL, not SQL, and the only way to turn it into DDL is to boot ActiveRecord against a database — which is the thing this avoids. If that is what your repository has, set `config.active_record.schema_format = :sql` in `config/application.rb` and run `bin/rails db:schema:dump`.

`--drizzle` runs `drizzle-kit export --config=<file>`, which renders the TypeScript schema from empty state onto stdout without a database, without `dbCredentials` and without writing a migration directory. A `dialect` other than `postgresql` is refused first, because `export` answers a mismatched dialect with an empty script and exit 0 rather than an error.

The ORM keeps owning the model; godwit keeps owning what runs, when, under which lock and with which hazards acknowledged. The file must describe the whole database: anything the target has that the file does not is a `DROP` in the generated `up`, so a Django dump keeps its `django_*` tables and a Prisma project that also has `_prisma_migrations` on the target declares it too. The Alembic and Rails sources already do this for you: `alembic_version` and `schema_migrations` come out of the tools themselves.

The one exception is what a repeatable declares. `godwit diff` sends `--dir` to the service and applies its `R__` migrations on top of the desired schema, so an ORM team that also keeps a couple of views or functions in `R__` files gets them left alone instead of dropped — and `lint`'s `E005` stops firing on them for good. Delete the `R__` file when you want the object gone: then nothing declares it and the next diff proposes the drop. On a target that has run repeatables, a diff that cannot see the directory is refused rather than answered with those drops ([concepts: objects a repeatable declares](concepts.md#objects-a-repeatable-declares)).

## 4. Stop repeating flags

A `godwit.yaml` at the repo root (found from the working directory upward, stopping at `.git`):

```yaml
dir: db/migrations
target: app
rollout: expand-contract
server: http://localhost:8474
```

Then `godwit lint`, `godwit plan` and `godwit migrate` work bare. The token stays in `GODWIT_TOKEN`; the file never carries secrets. Every key, its env override and its precedence is in [configuration](configuration.md).

Two consequences of this particular file worth knowing before you write it:

- `target` reaches `plan` too, and `plan --target` is a service command. With `target: app` in the file, a bare `godwit plan` no longer parses the directory offline — it plans against the live target and stores a plan, like section 3c. `godwit plan --target ""` forces the offline form back.
- `rollout: expand-contract` applies to every run, including the ones CI makes, so a destructive migration will stop at `awaiting_contract` and wait for `godwit run confirm` (or `/godwit confirm` on the pull request, below).

## 5. CI

Plan on the pull request, apply from it, verify on the merge, with the composite Action in this repository:

```yaml
# on pull_request
permissions: { contents: read, pull-requests: write }
steps:
  - uses: actions/checkout@v4
  - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
    with: { command: lint }
  - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
    with:
      command: plan
      server: https://godwit.internal
      token: ${{ secrets.GODWIT_TOKEN_READ }}
      target: orders

# on issue_comment (created): a collaborator comments "/godwit apply" once the review is done
permissions: { contents: read, pull-requests: write, statuses: write }
steps:
  - uses: actions/checkout@v4
    with: { ref: "refs/pull/${{ github.event.issue.number }}/head" }
  - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
    with:
      command: apply
      server: https://godwit.internal
      token: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
      target: orders

# on push to main
steps:
  - uses: actions/checkout@v4
  - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
    with:
      command: verify
      server: https://godwit.internal
      token: ${{ secrets.GODWIT_TOKEN_READ }}
      target: orders
```

`lint` and `plan` keep one sticky comment on the pull request; `apply` runs the stored plan from the pull request head and sets the `godwit/applied` commit status the branch protection requires; `verify` on the merge proves `main` carries nothing unapplied.

`/godwit apply` is refused unless the commenter holds write or admin permission on the repository **and** an approving review by someone other than the pull request author stands on that exact commit — so a push after the approval means approving again before applying. Working alone, either approve from a second account or pass `require-approval: "false"` ([who may command an apply](ci-cd.md#who-may-command-an-apply)). The action is pinned to a commit rather than `@main` on purpose: the apply job holds a `pipeline` token.

The database changes **before** the merge, on purpose: by the time the pull request lands, `main` describes a schema the target already has. An `expand-contract` apply needs a second comment to finish: it stops at `awaiting_contract`, and until `/godwit confirm` runs, `godwit/applied` stays `pending` with *expand applied; comment `/godwit confirm` to run the contract phase*, so branch protection holds the pull request. Add the step beside the apply, in the same `issue_comment` job:

```yaml
  - name: Run the contract phase held by the apply
    if: contains(github.event.comment.body, '/godwit confirm')
    uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
    with:
      command: confirm
      server: https://godwit.internal
      token: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
      target: orders
```

Inputs, outputs, the revert command, the `apply-on-merge` mode and the ArgoCD variant are in [CI/CD](ci-cd.md); the pull request stuck at `awaiting_contract` is a [runbook](runbook.md#a-pull-request-stuck-in-awaiting_contract) entry.

## Next

[Concepts](concepts.md) explains what just happened in the target database and what happens when a replica dies with a run in flight. [Operations](operations.md) is the checklist before the service takes production traffic. Before you first need it: `godwit revert <run-id>` undoes **every** migration the run carried, not the last one — the [runbook](runbook.md#reverting-a-run) shows what that costs.
