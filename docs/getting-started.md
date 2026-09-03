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
└── 20260901120500_orders_customer_idx.down.sql
```

File names must match `<14-digit timestamp>_<snake_name>.{up,down}.sql`. Both sides are required and must not be empty; a version with two different names, or an unexpected file, fails the load. Entries starting with `.` are ignored.

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

Lint the directory the way a pull request gate does, and apply against a local database:

```bash
godwit lint --dir db/migrations                       # exit 1 on unacknowledged hazards, parse errors, empty files
godwit apply --dsn postgres://app:app@localhost/app --dir db/migrations
godwit status --dsn postgres://app:app@localhost/app --dir db/migrations
godwit down --dsn postgres://app:app@localhost/app --dir db/migrations --version 20260901120500 --yes
```

`apply` is the same executor the service uses: it takes the advisory lock, creates the `godwit` schema in the target, and journals every statement. Kill it mid-way and run it again; it resumes from the last `done` row.

## 2. The service

The service needs a PostgreSQL database of its own (the *store*) and two secrets.

```bash
createdb godwit_store
export GODWIT_MASTER_KEY=$(openssl rand -hex 32)          # encrypts static target DSNs at rest
export GODWIT_TOKENS='admin:admin:s3cret-admin,ci:pipeline:s3cret-ci,oncall:operator:s3cret-ops'
godwit serve --store-dsn postgres://godwit:godwit@localhost/godwit_store --listen :8474
```

`serve` migrates the store schema, starts the leased scheduler, the drift monitor and the scratch-database validator, then listens for gRPC and JSON on one port (plus `/metrics`, `/healthz`, `/readyz`). The store role needs `CREATEDB` because validation creates a throwaway `godwit_validate_<id>` database on the store server ([operations: store](operations.md#the-store)).

Register a target. The DSN is encrypted with the master key and never printed again:

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
run 0d3c6c6e-...: queued
run 0d3c6c6e-...: running (attempt 1)
run 0d3c6c6e-...: succeeded (attempt 1)
```

`migrate` sends every file in the directory, waits for admission (hazard gate, out-of-order guard, scratch replay of the target's whole history plus the new files), then streams the run until it settles. Exit code 0 on `succeeded` or `awaiting_contract`, 1 on `failed`, `needs_attention` or any refusal. A run with an unacknowledged hazard is refused before anything is queued:

```
unacknowledged hazards (pass acknowledge_hazards to accept):
H001: CREATE INDEX without CONCURRENTLY blocks writes on orders
```

Hazards are reported for the direction being planned; a normal `migrate` plans the up side only, so the `DROP TABLE` in the down file above is not in the way (it will be when that run is reverted). Acknowledge with `--ack H001` when you do mean it.

Look around:

```bash
godwit runs --target app
godwit run get <run-id>
godwit target status app --dir db/migrations
godwit audit --target app
```

## 3b. Write the next migration from a schema

When you would rather describe the database you want than the step to get there, keep a `schema.sql` with the whole DDL and let the service write the file:

```bash
godwit diff --target app --schema db/schema.sql --name orders_status --dir db/migrations
```

```
app -> db/schema.sql: 1 statement(s)
  [0] tx    ALTER TABLE "public"."orders" ADD COLUMN "status" text COLLATE "pg_catalog"."default" DEFAULT 'new'::text NOT NULL

-- up
ALTER TABLE "public"."orders" ADD COLUMN "status" text COLLATE "pg_catalog"."default" DEFAULT 'new'::text NOT NULL;
-- down
ALTER TABLE "public"."orders" DROP COLUMN "status";
wrote db/migrations/20260902103000_orders_status.up.sql
wrote db/migrations/20260902103000_orders_status.down.sql
```

The starting point is the live target as `plan` observes it, the end point is `schema.sql` applied on an empty scratch database on the service. Hazards and recipes are printed the way `plan` prints them, a `drift` block comes first when the live schema has hand changes the history does not know about (they end up in the generated `up`), `--dry-run` prints without writing, `--json` returns `up_sql`, `down_sql`, `statements`, `drift` and `files`, and `no changes` exits 0 with nothing written. Read the files before committing them: the generated SQL goes through the same `lint`, `plan` and hazard gate as a hand-written one ([concepts: generating migrations from a schema](concepts.md#generating-migrations-from-a-schema) lists what the diff does and does not cover).

`--schema` takes any file with plain PostgreSQL DDL, so an ORM's schema dump works as the desired state. A Prisma schema needs no dump: `--prisma` renders it with the project's Prisma CLI, no database involved.

```bash
# Prisma: the datamodel itself; the datasource provider must be postgresql
godwit diff --target app --prisma prisma/schema.prisma --name sync_prisma

# GORM: dry-run AutoMigrate against an empty database and capture the DDL
go run ./cmd/schema-dump > db/schema.sql   # db.Session(&gorm.Session{DryRun: true}).AutoMigrate(&Order{}) with a logger that records the SQL

# Django: the SQL of every migration of every app, in order
python manage.py showmigrations --plan | awk '{print $2}' | while IFS=. read app name; do python manage.py sqlmigrate "$app" "$name"; done > db/schema.sql

godwit diff --target app --schema db/schema.sql --name sync_orm
```

`--prisma` runs `prisma migrate diff --from-empty --script` on the file (`npx prisma` by default, so the `prisma` devDependency the project pins is what renders it; `--prisma-bin` or `GODWIT_PRISMA_BIN` names another command line, such as `node_modules/.bin/prisma --config prisma.config.ts` on Prisma 7, whose `prisma.config.ts` must set a `datasource.url`, any value, nothing connects to it). Prisma 5, 6 and 7 are supported; the CLI's own errors are surfaced as-is.

The ORM keeps owning the model; godwit keeps owning what runs, when, under which lock and with which hazards acknowledged. The file must describe the whole database: anything the target has that the file does not is a `DROP` in the generated `up`, so a Django dump keeps its `django_*` tables and a Prisma project that also has `_prisma_migrations` on the target declares it too.

## 4. Stop repeating flags

A `godwit.yaml` at the repo root (found from the working directory upward, stopping at `.git`):

```yaml
dir: db/migrations
target: app
rollout: expand-contract
server: http://localhost:8474
```

Then `godwit lint`, `godwit plan` and `godwit migrate` work bare. The token stays in `GODWIT_TOKEN`; the file never carries secrets. Every key, its env override and its precedence is in [configuration](configuration.md).

## 5. CI

Plan on the pull request, apply from it, verify on the merge, with the composite Action in this repository:

```yaml
# on pull_request
permissions: { contents: read, pull-requests: write }
steps:
  - uses: actions/checkout@v4
  - uses: SamuelMolling/godwit@main
    with: { command: lint }
  - uses: SamuelMolling/godwit@main
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
  - uses: SamuelMolling/godwit@main
    with:
      command: apply
      server: https://godwit.internal
      token: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
      target: orders

# on push to main
steps:
  - uses: actions/checkout@v4
  - uses: SamuelMolling/godwit@main
    with:
      command: verify
      server: https://godwit.internal
      token: ${{ secrets.GODWIT_TOKEN_READ }}
      target: orders
```

`lint` and `plan` keep one sticky comment on the pull request; `apply` runs the stored plan from the pull request head and sets the `godwit/applied` commit status the branch protection requires; `verify` on the merge proves `main` carries nothing unapplied. Inputs, outputs, the revert command, the `apply-on-merge` mode and the ArgoCD variant are in [CI/CD](ci-cd.md).

## Next

[Concepts](concepts.md) explains what just happened in the target database and what happens when a replica dies with a run in flight. [Operations](operations.md) is the checklist before the service takes production traffic.
