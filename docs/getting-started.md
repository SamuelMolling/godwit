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
20260901120500_orders_customer_idx (up): 1 statement(s)
  [0] no-tx CREATE INDEX CONCURRENTLY orders_customer_idx ON orders (customer_id)
...
```

`tx` statements run inside a transaction with the journal write; `no-tx` statements (`CREATE INDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`, `VACUUM`, `REFRESH MATERIALIZED VIEW CONCURRENTLY`, `REINDEX CONCURRENTLY`) get a write-ahead intent and a verifier instead. Hazards are the codes a run must acknowledge ([concepts: hazards](concepts.md#hazards)).

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

Pull request gate and merge step with the composite Action in this repository:

```yaml
# on pull_request
permissions: { contents: read, pull-requests: write }
steps:
  - uses: actions/checkout@v4
  - uses: SamuelMolling/godwit@main
    with: { command: lint }
  - uses: SamuelMolling/godwit@main
    with: { command: plan }

# on push to main
steps:
  - uses: actions/checkout@v4
  - uses: SamuelMolling/godwit@main
    with:
      command: migrate
      server: https://godwit.internal
      token: ${{ secrets.GODWIT_TOKEN }}
```

`lint` and `plan` keep one sticky comment on the pull request; `migrate` streams the run and fails the job with the run. Inputs, outputs, the dry-run comment and the ArgoCD variant are in [CI/CD](ci-cd.md).

## Next

[Concepts](concepts.md) explains what just happened in the target database and what happens when a replica dies with a run in flight. [Operations](operations.md) is the checklist before the service takes production traffic.
