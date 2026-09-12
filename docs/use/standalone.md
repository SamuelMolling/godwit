# Run migrations from the command line

No App, no workflow, no webhook. One binary, which is both the service and the client, and two ways to use
it: straight against a database, or against a godwit service over its API. Start here if you are evaluating
godwit — everything the other routes do, they do by calling these commands.

```bash
go install github.com/SamuelMolling/godwit/cmd/godwit@main   # needs gcc (libpg_query) and Go 1.26
docker pull ghcr.io/samuelmolling/godwit:main               # or the image: amd64 + arm64, distroless
```

## Without a service at all

`up`, `status` and `down` talk to one database and nothing else. There is no target to register, no plan to
bind and no ledger — what they apply is recorded in that database's own journal. Same executor, same
statement-level journal and the same crash safety as a service run.

```bash
export GODWIT_DSN='postgres://app:app@localhost/app'   # or --dsn; the env form keeps the password out of ps
```

**Write the pair.** `godwit new` names the files the way the loader expects; the name must be `[a-z0-9_]+`.

```console
$ godwit new add_users
wrote migrations/20260912233924_add_users.up.sql
wrote migrations/20260912233924_add_users.down.sql
```

Both files start with a comment and nothing else — godwit refuses an empty migration, and the down side is
not optional.

**Check it before it runs.** `godwit lint` parses with PostgreSQL's own parser and exits 1 on any blocking
finding, which makes it the one command worth putting in a pre-commit hook or a build step:

```console
$ godwit lint --dir migrations
20260912233924_add_users.up.sql: error H001 CREATE INDEX without CONCURRENTLY blocks writes on users
    -- or let godwit run it: -- godwit: add-index users (email) name=users_email_idx
    CREATE INDEX CONCURRENTLY users_email_idx ON users USING btree (email);
1 finding(s), 1 blocking
godwit: 1 blocking finding(s)
```

`--base origin/main` narrows it to migrations added since that ref, so a repository full of history does not
re-report itself on every run. `--format json` and `--format markdown` are there for a pipeline to consume.
`--ack H001` accepts a finding and drops the exit code back to 0.

**See what would run.** With no `--target`, `godwit plan` never touches a database — it parses the directory
and prints both sides of every migration in it:

```console
$ godwit plan --dir migrations
Offline plan. Both sides of every migration in the directory, as written; no database was consulted, so nothing here says what is pending.

+ 20260912233924_add_users  2 statements
  statement 0, runs inside a transaction
      CREATE TABLE users (
          id bigint PRIMARY KEY,
          email text NOT NULL
      );
  statement 1, runs inside a transaction
      CREATE INDEX users_email_idx ON users (email);
      hazard H001: CREATE INDEX without CONCURRENTLY blocks writes on users
        -- or let godwit run it: -- godwit: add-index users (email) name=users_email_idx
        CREATE INDEX CONCURRENTLY users_email_idx ON users USING btree (email);

- 20260912233924_add_users (down)  1 statement
  statement 0, runs inside a transaction
      DROP TABLE users;
      hazard H002: DROP TABLE is destructive
        -- expand then contract: ship the application version that no longer uses users, then run this DROP TABLE as a contract migration (rollout: expand-contract)

2 hazards on what this run would execute: take the recipe printed with it, or accept the risk with --ack H001,H002 (godwit apply --ack H001,H002 on a pull request).
Plan: 1 to apply, 1 to revert, 2 hazard(s) to acknowledge
```

Statements are numbered from zero, and the down side is planned too — which is how `W001` catches a down
migration that is a no-op.

**Run the loop.**

```console
$ godwit status --dir migrations
20260912233924_add_users: pending

$ godwit up --dir migrations
20260912233924_add_users: applied (2 statement(s))

$ godwit status --dir migrations
20260912233924_add_users: applied 2026-09-12T23:40:29Z
```

`status` asks the database, not a ledger: a migration whose file changed after it was applied shows
`(checksum drift!)`.

**Undoing, in development.** `down` runs one migration's down side and removes it from that database's
journal. It names a version and insists you mean it:

```console
$ godwit down --dir migrations --version 20260912233924
godwit: down is destructive; re-run with --yes to confirm

$ godwit down --dir migrations --version 20260912233924 --yes
20260912233924_add_users: reverted (1 statement(s))
```

This is the dev pair. In production the pair is `migrate` and `revert`, and `revert` undoes one whole *run*
read from the ledger rather than one version you name — which is the entire reason it is the production path.

**What this mode does not do.** `up` has no `--ack`: it applies what you wrote, hazards and all. The hazard
gate, the out-of-order guard and the replay of the target's history on a throwaway database all live in the
service's admission step. Locally, `godwit lint` is the gate, and it is one you have to remember to run.

## Against a service

Everything below needs an endpoint and a token. The CLI authenticates with a **static bearer token only** —
there is no OIDC, no workload identity, no mTLS in the client's path.

```bash
export GODWIT_SERVER=http://localhost:8474    # or --server
export GODWIT_TOKEN=s3cret                    # or --token
```

An `https://` endpoint gets TLS and system roots; anything else is dialled as cleartext h2c, so do not point
the plain form at a public host.

**Your own service, for a trial:**

```bash
psql -U postgres \
  -c "CREATE ROLE godwit LOGIN PASSWORD 'godwit' CREATEDB" \
  -c "CREATE DATABASE godwit_store OWNER godwit"

export GODWIT_MASTER_KEY=$(openssl rand -hex 32) GODWIT_TOKENS='admin:admin:s3cret'
godwit serve --store-dsn postgres://godwit:godwit@localhost/godwit_store
```

`GODWIT_TOKENS` is a comma-separated list of `name:scope:secret`. The scopes are ordered — `read` <
`pipeline` < `operator` < `admin` — and each includes everything below it: `read` plans, diffs and lists;
`pipeline` also creates, reverts and confirms runs; `operator` resumes and parks runs and accepts drift;
`admin` registers targets and credential stores. Give a pipeline a `pipeline` token, not an admin one.

That single-server form runs submitted DDL on the store server with the store credentials, which is why the
role needs `CREATEDB` and why `serve` warns about it on every start. Point `--scratch-dsn` at a PostgreSQL
that holds nothing before anyone else gets a token.

**Register the target.** The DSN is sealed in the store under `GODWIT_MASTER_KEY`; the CLI never sees it
again.

```console
$ godwit target add app --provider static --dsn postgres://app:app@localhost/app
target app: registered
```

**Plan against the live database.** With `--target`, the plan is admitted: the files are replayed against the
target's recorded history on a throwaway database, the hazards are gated, and the result is a plan you can
store.

```console
$ godwit plan --target app --dir migrations
godwit: unacknowledged hazards (pass acknowledge_hazards to accept):
H001: CREATE INDEX without CONCURRENTLY blocks writes on users
```

That is the difference from the offline plan, which prints hazards and exits 0. Name the code, and add
`--save` to store the plan so a later `migrate` can bind to it:

```console
$ godwit plan --target app --dir migrations --ack H001 --save
1 migration will be applied to app.

godwit will perform the following actions:

  # 20260912233924_add_users
  + table "public"."users" {
      + email      = text NOT NULL
      + id         = bigint NOT NULL
      + users_pkey = PRIMARY KEY (id)
    }
  # CREATE INDEX without CONCURRENTLY blocks writes on users (H001)
  + index "public"."users_email_idx" {
      + definition = CREATE INDEX users_email_idx ON public.users USING btree (email)
    }
    recipe for H001:
      -- or let godwit run it: -- godwit: add-index users (email) name=users_email_idx
      CREATE INDEX CONCURRENTLY users_email_idx ON users USING btree (email);

how this will run:
  2 statements run one at a time, in the order this report lists them, each committing with its own journal row on the target. A run that fails or is killed part way is resumed at the first statement that never committed, rather than replaying the migration from its first line.
  Every statement here runs inside a transaction, so one that fails leaves nothing of itself behind.
  1 statement holds a lock the rest of the application queues behind while it runs: users_email_idx: CREATE INDEX without CONCURRENTLY blocks writes on users (H001). ...

plan details:
  plan: 7f5b42e7-f694-483e-8f26-9f441f7f8c92

1 hazard on what this run would execute: take the recipe printed with it, or accept the risk with --ack H001 (godwit apply --ack H001 on a pull request).
Plan: 2 to add, 0 to change, 0 to destroy.
```

`--plan-format statements` prints the SQL instead of the schema change. `--format markdown` and
`--format json` render the same report for a pipeline; `--json`, which most service commands carry, is a
different thing — it dumps the raw protobuf response and ignores `--format`.

**Run it.** `migrate` submits the directory, binds to the stored plan and streams the run until it settles:

```console
$ godwit migrate --target app --dir migrations --ack H001
plan 7f5b42e7-f694-483e-8f26-9f441f7f8c92: bound
run 23923fa0-bee5-43ae-826d-b2670f371c37: queued
run 23923fa0-bee5-43ae-826d-b2670f371c37: succeeded (attempt 1)
```

`--dry-run` prints the plan and queues nothing. `--plan <id>` binds one specific stored plan, which supplies
the target, the rollout and the files unless you pass them yourself. If the target moved since the plan was
stored, `migrate` refuses with the diff and **exits 3** — plan again on the current state.

**Where it stands, and what it did:**

```bash
godwit target status app --dir migrations   # applied, pending, last run, drift baseline
godwit run get <run-id>                     # one run in detail
godwit run watch <run-id>                   # follow a run someone else started
godwit run report <run-id> --format markdown
godwit runs --target app
godwit migrations                           # which target has which migration, across the fleet
```

**Expand-contract from a shell.** A run under `rollout: expand-contract` stops at `awaiting_contract`. Deploy
the application, then release the rest:

```bash
godwit run confirm --latest --target app --allow-none
```

`--latest` picks the parked run without your having to find its id, and `--allow-none` exits 0 when there is
nothing parked — which is what makes the line safe to leave unconditionally in a deploy script.

**Undoing.** `godwit revert` undoes what one run applied, newest migration first; with no run id it takes the
newest un-reverted run on `--target`. `--dry-run` shows the plan first. A revert whose plan would drop a table
or column still holding rows is refused with the row counts and needs `--allow-data-loss`; one that is not the
newest un-reverted run on its target needs `--force`.

## Stop repeating the flags

Put a `godwit.yaml` at the root of the repository — the CLI looks for the nearest one from the working
directory upwards, stopping at the directory holding `.git`:

```yaml
dir: db/migrations
target: app
server: https://godwit.internal
rollout: expand-contract
lock_timeout: 5s
statement_timeout: 0s
allow_out_of_order: false
plan:
  format: schema
```

An explicit flag wins over `GODWIT_*` in the environment, which wins over the file. Unknown keys are an
error, not a warning. Two things to know about how far the file reaches: `target` deliberately does **not**
reach `godwit plan`, so a bare `godwit plan` is always the offline one; and `lock_timeout` and
`statement_timeout` reach `up`, `status` and `down` but not `migrate` or `revert`, which take them as
per-run string flags (`--lock-timeout 5s`) or from the target's own registered settings.

## Exit codes

Anything driving godwit from a script only needs these four.

| Code | Meaning |
|---|---|
| 0 | success — including `plan` with unacknowledged hazards, and `run confirm --latest --allow-none` with nothing parked |
| 1 | any error: blocking lint findings, a refusal at admission, a run that ended `failed` or `needs_attention`, a connection or usage error |
| 2 | reserved for the GitHub Action's own configuration and grammar refusals; no ordinary CLI command returns it |
| 3 | the stored plan is stale, or a target requires a plan and none matched — plan again and retry |

Errors go to stderr, always as `godwit: <message>`. Reports go to stdout in full even when the command then
exits non-zero, so a pipeline can capture the report and the status separately.
