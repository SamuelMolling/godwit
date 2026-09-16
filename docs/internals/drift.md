# Drift and comparison

What godwit compares, and what it does when two descriptions of a schema disagree: the live target against its baseline, against a desired schema, and against what the control plane recorded.

## Drift

After every successful run (and after a baseline) the scheduler stores a **snapshot** of the target schema in `cp_snapshots`: tables, their columns, constraints, indexes and triggers, sequences, enum types, functions and procedures, and an `md5` of each view and materialized view definition, sorted under a format marker, with a `sha256` fingerprint.

A trigger belongs to the table it fires on, the way a constraint does, and is recorded as `pg_get_triggerdef` writes it; the enforcement triggers behind a foreign key are internal and left out, because the constraint line already carries them. A function or procedure is keyed by its argument list, so two overloads are two objects, and carries its language, return type, volatility, security, strictness and parallel safety in full — but its **body as an `md5`**, the way a view definition is. A body is a page of PL/pgSQL and a snapshot line holds one object per line; the digest says the body moved without pasting it into a diff, and the migration that moved it is the diff. What a body's digest cannot say is *how* it changed, and a per-function `SET` (`proconfig`) is not recorded at all, which is why a `CREATE FUNCTION ... SET search_path` is `effect not inspectable` rather than a described change.

A column carries the type as PostgreSQL declares it — `format_type`, so `character varying(20)`, `numeric(10,2)`, `timestamp(3) with time zone`, `text[]` and the name of an enum, rather than the one word `information_schema` reduces each of them to. Widening a `varchar`, changing an enum column from one type to another and turning a column into an array are all schema changes, and a snapshot that cannot tell them apart cannot report them. The default is the expression `pg_get_expr` renders, which is also where a generated column's expression lands.

Left out, in one place rather than per query: `pg_catalog` and `information_schema`, godwit's own journal, everything an extension owns, and under `ignore_adopted_tables` the bookkeeping tables of the migration tool the database was adopted from. Two more are left out because something else already reports them, and reporting them twice turns one change into two diff lines: **the index behind a constraint** (the constraint line carries it) and **a sequence a serial or identity column owns** (the column's default carries it).

Not described at all: row-level security policies, grants, comments, and which extensions are installed. A change to any of those is invisible to drift; keep them in migrations and review them there.

**The format marker.** The first line of every definition names the snapshot format (`godwit-schema-v4`). A stored baseline carrying an older marker was taken by a godwit that looked at a different set of objects, or described them differently, so it cannot be compared with a fresh one: `CheckDrift` and the monitor refuse with `drift baseline predates the schema format` and name the fix (`godwit drift accept <target>`), rather than either reporting the upgrade as fleet-wide drift or silently re-baselining over drift the target really had. A plan's own drift falls back to empty for the same reason, and a stored plan taken before the change is `PlanStale{schema}` — re-plan.

**The tool godwit replaced is not drift.** A database adopted from golang-migrate keeps its `schema_migrations`; from Flyway, `flyway_schema_history`; also recognised are Liquibase's `databasechangelog` and `databasechangeloglock`, Alembic's `alembic_version`, Rails' `schema_migrations` and `ar_internal_metadata`, Prisma's `_prisma_migrations` and Atlas's `atlas_schema_revisions`. No migration creates them, so every plan and every drift check would report them for the rest of the target's life, and a signal that is always on is one people stop reading. The exemption is earned by shape, not by name: the table must carry the columns that tool's table must have and none it never creates, so a `schema_migrations` of your own stays visible. Every plan report names what it left out and how to turn the exemption off (`ignore_adopted_tables`), and the same exemption applies to the scratch replay, so the two sides of a comparison always agree about what is in the schema.

The monitor fingerprints every snapshotted target every `--drift-interval` (5m). A different fingerprint opens a `cp_drift_events` row with the diff (`- expected` / `+ live` lines), notifies, and logs `schema drift detected`; a matching one resolves any open event and notifies `resolved`. A partial unique index keeps one open event per target and diff, so replicas ticking together record once. `CheckDrift` runs the same comparison on demand; `AcceptBaseline` snapshots the live schema as the new baseline and resolves the open events.

## Adopting an existing database

One command, `godwit target adopt`, over two RPCs. Which one it calls depends on where the truth comes from, which is what its two flags name: `--version <N>` you supply, or `--from-journal` read off the target. Both write a `succeeded` run holding the files they put on the books, so scratch validation of later runs replays them, and both take a drift snapshot.

**`BaselineTarget{target, files, version}` (`godwit target adopt --version N`) — the schema is there, godwit never journalled it.** Every migration with `version <= N` is inserted into the target's `godwit.migrations` with its checksum, without running it, in one transaction under the advisory lock; then a `baseline` run records them in the ledger. The usual first file is a schema dump named like `20260101000000_baseline.up.sql` with an empty-effect down side. `version` is required and explicit: defaulting to the whole directory would silently swallow pending migrations. Both halves are idempotent — a version the target already records is left alone, and one the ledger already stands on is not recorded twice — so adopting again over a partly-adopted target completes the adoption. Refused with `failed_precondition` when there is nothing left to record on either side, and when a file's checksum differs from the one the target recorded for that version (`recorded on the target under different content`).

**`ReconcileTarget{target, files}` (`godwit target adopt --from-journal`) — the target has a `godwit` journal this service did not write.** Another instance migrated it, the store was rebuilt, or the store was restored from a backup older than the target. It reads `godwit.migrations` and `godwit.repeatables`, compares them with the ledger, and writes a `reconcile` run holding the ledger rows the journal has and the store does not. **It never writes to the target.** No `version`: the target says what it holds.

It refuses, naming what it means, on the three disagreements it will not decide alone:

| What it found | Why it refuses |
|---|---|
| recorded under different content than the directory carries | drift between the repository and the target; one of the two is wrong and only a person can say which |
| recorded on the target and absent from the directory | the replay could never rebuild a migration whose SQL the store does not hold |
| standing in the ledger and absent from the target | the target has lost history the control plane saw applied — a restore from backup, or a hand-emptied journal |

**The refusal that sends you here.** The ledger is what the order guard and the scratch replay read, so planning over one that cannot see an out-of-band apply plans against a history the target does not have. Before planning against a target with no stored plan to explain its history, godwit compares the observation it takes with the ledger:

```
target records migrations the ledger does not: app records 20260101000000_orders, 20260101000001_total;
run `godwit target adopt app --from-journal --dir <migrations>` to adopt what it already has
```

Reverts are not gated: a revert acts on the ledger's own rows and is well defined whatever else the target holds. A target with a stored plan is not gated either — a history change the plan cannot attribute to a run is already `PlanStale{history}`, which reports it better.

**A run repairs what it walks past.** When the executor skips a migration because the target's journal already records it under the same checksum, the scheduler writes an adopted ledger row, unless a standing row already accounts for it. So a `migrate` over a database whose journal is ahead brings the ledger level, and a second `migrate` over the same directory adopts nothing.

Neither a `baseline` nor a `reconcile` run can be reverted (`failed_precondition`): reverting would run down files against a schema godwit never built.

## Generating migrations from a schema

`Diff{target, schema}` (`godwit diff`) turns a description of the whole database you want into the next migration. The **before** side is the target as the plan machinery observes it (`Observe`: applied versions, schema definition, `search_path`), not its recorded history. The **after** side is `schema`, applied as-is on an empty scratch database on the scratch PostgreSQL (`--scratch-dsn`, [security](../run/security.md#the-scratch-database)) with the target's `search_path`, followed by the `R__` migrations of `files` ([objects a repeatable declares](#objects-a-repeatable-declares)). The two are compared with [pg-schema-diff](https://github.com/stripe/pg-schema-diff) in both directions: live → desired is the `up` SQL, desired → live the `down` SQL. Every statement is classified by the same planner a run uses, so the response carries hazards and recipes, and `godwit diff` writes `<timestamp>_<name>.up.sql` / `.down.sql` in the migration directory. Equal schemas produce empty SQL and nothing is written.

Because the starting point is the live schema, hand changes that are not in the history become part of the generated migration. When validation is on, the response also carries `drift`: the `+`/`-` lines between the history replayed on a scratch database and the live target, so you can tell which part of the `up` captures drift and which part is new ([drift](#drift) explains the format). With `--skip-validation` on the service, `drift` is empty.

### Objects a repeatable declares

Why they are part of the desired schema, and the alternatives refused: [decision 0006](../decisions/0006-repeatable-objects-are-desired.md).

An `R__` migration builds objects the ORM schema knows nothing about — a view, a function, a trigger. The desired side is therefore not `schema` alone: `DiffRequest.files` is the migration directory, and every `R__` pair in it is applied on the desired scratch database after the DDL, in the order a run applies them. The object is then on both sides of the comparison and neither direction proposes to touch it, under either base. `repeatable_objects` in the response names what appeared when they ran.

That set comes from the scratch database's own catalog, read before and after the repeatables are applied — not from parsing their bodies and not from `pg_depend` on the target. Parsing sees only what the statements name; `pg_depend` records dependencies, not which file made an object, so it cannot attribute anything on a live target. What appears on a database where nothing else ran is exactly what those files build.

What follows:

| Situation | What the diff does |
|---|---|
| A repeatable edited so it builds a different object | the new object is in the desired schema; the old one is in no file any more, and the `up` drops it |
| A repeatable deleted from the directory | nothing declares its object, and the `up` drops it — deleting the file is how you retire what it built |
| An object a versioned migration created and a repeatable later took over | the `R__` file declares it, so it is in the desired schema and the diff leaves it alone |
| A repeatable that no longer builds on the desired schema | `invalid_argument`, `repeatable migration does not build on the desired schema: <file>: <postgres error>` — the migration the diff was about to write would have broken it |

**Without the migration directory the diff refuses.** A request carrying no `files` while `godwit.repeatables` on the target has rows is `failed_precondition`, naming the recorded repeatables: the diff can see those objects but not what declares them, and would propose dropping every one. `godwit diff` sends `--dir` for exactly that reason. `/ui/diff` has no directory to send, so it supplies the `R__` pairs from a snapshot the control plane already stores — the target's newest stored plan, or the run that last succeeded on it — and says on the page which one it used, how old it is and where it disagrees with what the target recorded; the boxes on the page take the bodies by hand when neither snapshot has them. A target that records none is unaffected: there is nothing to attribute and nothing to refuse.

### Schema sources

Why every source runs client-side: [decision 0003](../decisions/0003-orm-schema-sources.md).

The `schema` the service receives is always DDL; where it comes from is the client's business. `godwit diff` has one **schema source** per flag, each an implementation of `schemasource.Source` (`Load(ctx) (ddl, error)`), all of them running next to the repository with the project's own toolchain — the service never sees a Prisma schema, a Go package, a Django project or an Alembic history, and never gains a Node, Go, Python or Ruby dependency:

| Flag | `kind` | What it runs | Refuses |
|---|---|---|---|
| `--schema <file>` | `file` | nothing; the file is plain DDL | — |
| `--prisma <schema.prisma>` | `prisma` | `prisma migrate diff --from-empty --to-schema[-datamodel] <file> --script`, the flag chosen by the CLI's major version | a datasource provider other than `postgresql`, before running anything |
| `--exec '<command line>'` | `command` | the argv as given, stdout is the DDL | empty stdout |
| `--gorm <package>` | `gorm` | `go run <package>` | an empty stdout, with the compiler's stderr surfaced on a build failure |
| `--django <manage.py>` | `django` | `python manage.py showmigrations --plan --no-color`, then `python manage.py sqlmigrate <app> <name> --no-color` for every migration in plan order | a `DATABASES` `ENGINE` that is not PostgreSQL, before running anything |
| `--alembic <alembic.ini>` | `alembic` | `alembic -c <alembic.ini> upgrade head --sql` — Alembic's offline mode, which renders every revision from base without a connection | a `sqlalchemy.url` whose dialect is not PostgreSQL, before running anything |
| `--rails <app root>` | `rails` | nothing; the application's committed `db/structure.sql` is already DDL | `db/schema.rb`, which is a Ruby DSL no offline tool renders |
| `--drizzle <drizzle.config.ts>` | `drizzle` | `drizzle-kit export --config=<file>`, which diffs the TypeScript schema against empty state and prints the DDL on stdout | a `dialect` other than `postgresql`, before running anything |

`--exec` is the escape hatch: any command that prints the whole desired database on stdout. `--gorm` is a thin wrapper over it, because GORM's dry-run migrator is a Go API over your model structs, not a CLI: the package is yours, godwit only runs it and reports a build failure with the package name instead of `exit status 1` ([examples/gorm/schema/main.go](../../examples/gorm/schema/main.go) is a copyable 20-line one). `--django` concatenates `sqlmigrate`'s output, dropping the `BEGIN;`/`COMMIT;` lines Django wraps an atomic migration in. **Django's constraint, documented rather than hidden:** `sqlmigrate` opens the configured connection to introspect, so `DATABASES` must point at a reachable PostgreSQL (`--django-database <alias>` picks which); teams for whom that does not hold use `--exec` with their own dump. `--go-bin`, `--python-bin`, `--prisma-bin`, `--alembic-bin` and `--drizzle-bin` (and their `GODWIT_*_BIN` variables) name the interpreter when it is not on `PATH`; a missing one is reported as a godwit message, not as a bare `exec` error.

`--alembic` runs the CLI's own offline mode, so nothing connects: `upgrade head --sql` replays every revision from base into a script. The `BEGIN;`/`COMMIT;` wrappers go, for the same reason Django's do; **`alembic_version` stays**, both the `CREATE TABLE` and the `INSERT`/`UPDATE` that carry the revision — it is a real table on the target, and a desired schema that omitted it would make the first diff propose dropping the project's own migration history. **Alembic's constraint, documented rather than hidden:** offline mode cannot render a revision that reads the database (`op.get_bind()`, reflection, a data migration without `literal_binds`); Alembic raises there and godwit surfaces it. A second one: a plain relative `script_location` in `alembic.ini` is resolved against the *working directory*, not against the file, so either write `script_location = %(here)s/alembic` or run `godwit diff` from the project root. The `sqlalchemy.url` check is deliberately lenient — a project that builds the URL in `env.py` declares no dialect in the file and is not refused; only a URL godwit can read whose dialect is not PostgreSQL is.

`--rails` runs nothing at all. Rails has two schema formats and only one of them is SQL: `db/schema.rb` is a Ruby DSL, and rendering it means booting ActiveRecord against a database — so it is refused, with the `config.active_record.schema_format = :sql` line and the `bin/rails db:schema:dump` that produce the other one. `db/structure.sql` is real `pg_dump` output, checked into the repository, and needs neither Ruby nor a database to read. What godwit strips from it is what would not survive being replayed: the `\restrict` / `\unrestrict` psql meta-commands newer `pg_dump` emits (not SQL at all, and a syntax error anywhere else), the session `SET` block (`SET transaction_timeout` alone fails on a server older than PostgreSQL 17) including the trailing `SET search_path` Rails appends, `SELECT pg_catalog.set_config('search_path', '', false)`, and the `INSERT INTO "schema_migrations"` rows, which are the ledger's contents rather than schema. Everything else survives untouched — extensions, comments, `pg_dump`'s own `-- Name: ...` headers — and the stripper tracks dollar quoting, so a `SET` inside a `$$ ... $$` function body is left alone. The argument takes the application root, or the dump itself when it is not at `db/structure.sql`.

`--drizzle` uses `drizzle-kit export`, which builds the snapshot in memory, diffs it against empty state and prints the DDL on stdout — no database, no `dbCredentials`, no files written, and none of the `--> statement-breakpoint` markers `drizzle-kit generate` puts in a migration file. **Drizzle's trap:** a `dialect` that does not match the schema files fails *silently* — `export` exits 0 with an empty script — so the `postgresql` check runs first and empty output is refused with that hint. As with `--alembic`, godwit starts the tool as a child of its own working directory and sets no other one, so a relative `schema` path inside `drizzle.config.ts` resolves from there.

The source is also a property of the directory, not only of the command line: a `schema_source` block in `godwit.yaml` says which ORM the migrations next to it follow, and `godwit diff` falls back to it when no source flag is given ([configuration](../run/configuration.md#godwityaml) has the keys). `schema_source.path` is resolved relative to the file that declares it, and `godwit.yaml` is looked up from the working directory upward, so a monorepo puts one next to each migration directory and every directory keeps its own source. The block is also what the lint check below compares the committed migrations against; the flags stay the override for a one-off diff.

### Keeping the generated SQL and the ORM schema together

A Prisma or GORM team edits the ORM schema, `godwit diff` writes the pair, both are committed. Nothing then stops someone from editing the ORM schema without regenerating, or hand-editing the generated `.sql`: the pull request looks fine and the two drift apart silently. `godwit lint` catches it.

The check cannot use the live target as the before side — the pending files are not applied there, so the diff would re-propose everything pending. `DiffRequest.base` therefore takes a second starting point:

| `base` | Before side | Reads |
|---|---|---|
| `DIFF_BASE_LIVE` (default) | the target as `Observe` sees it | the live database |
| `DIFF_BASE_FILES` | `DiffRequest.files` replayed on top of the target's recorded history, on a scratch database | the store and the files in the request |

With `base: files` the before side is `S_n`, the schema the committed files claim to produce, so `up_sql` is empty **exactly when** the committed migrations already express the ORM schema. Anything left is the residue, whatever caused it. The replay is the validator's own: the recorded history first (each run with the directive expansion *it* froze, never a fresh one), then the request's files, which the journal skips where the history already covers them.

`godwit lint --server <url> --target <t>` renders the declared source client-side, sends the whole directory as `files`, and reports the residue:

```
$ godwit lint --dir db/migrations --server https://godwit.internal --target orders
prisma/schema.prisma: error E005 the migration generated from prisma/schema.prisma is out of date
    ALTER TABLE "public"."users" ADD COLUMN "email" text;
1 finding(s), 1 blocking
```

`E005` blocks (exit 1) unless `schema_source.lint` is `false`, which makes it a warning. Without `--server` the check reports `W002` (`<path> not checked: no server configured`) and lint stays entirely offline — the local/CI parity the check is for is "same command, same config", not "same connectivity". `--no-schema-check` turns it off. The ORM itself always runs client-side, next to the repository: the service only ever receives DDL.

What the diff covers is what pg-schema-diff covers: schemas, extensions, enums, tables (columns with type, default, nullability, collation, identity and generated expression; check constraints; partitions; row-level security and policies; replica identity; table grants), primary and unique keys, foreign keys, indexes (`CREATE INDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`, online replacement of a changed index), sequences, functions, procedures, triggers, views and materialized views. Not covered: types other than enums (domains, composite types), exclusion constraints, comments, roles, grants on anything but tables; keep those in hand-written migrations. Index names in the output are unquoted; everything else is schema-qualified. The scratch database is empty apart from the target's `search_path`, so the schema must declare what it relies on (`CREATE SCHEMA`, `CREATE EXTENSION`) or qualify names; a schema that fails to apply is refused with `invalid_argument` and PostgreSQL's error. Data is never inferred: a column rename comes out as drop + add, a type change as `ALTER COLUMN ... TYPE`, both flagged by their hazards with the expand/contract recipe.

## Target status

`GetTargetStatus` reads `godwit.migrations` and `godwit.repeatables` on the live target without creating them (a never-migrated database reports nothing), compares against optional files (pending versions, `checksum_mismatch` when an applied migration's up file changed, repeatables listed as applied when their content matches and as pending when it does not), and adds the last run, the drift baseline (`taken_at`, the run that took it, whether drift is open), the provider and the registered timeouts. Repeatable rows carry `repeatable = true` and no version. A target whose credential does not resolve is answered from the control plane alone, with `unreachable` saying why its journal was not read and what to do about it, rather than refused: that is the state an operator is in *while fixing the registration*, so it is the last moment to withhold everything else godwit knows about the target. A credential that resolves onto a database godwit cannot reach is still an error — the answer would be wrong, not partial.

## The fleet view

`GetTargetStatus` answers *what does this database have*, one database at a time. `ListMigrations` — `godwit migrations`,
`/ui/fleet` — answers the question that spans them: **which of my targets has this migration**. It reads the
control plane's ledger and opens no connection to any target, so it answers while one is unreachable.

The key is the migration **and its content**: the id (`<version>_<name>` or `R__<name>`) with the sha256 of the up file
the target applied. So a version two targets applied from different files is two rows, both marked *divergent*, and each
target reads `differs` in the other's row. That is the case worth catching — the same version meaning two different
things in staging and production — and it is why the checksum is in the key and not a detail on the side. Repeatables
are keyed the same way, which is their normal identity anyway (name and content, no version).

A migration is on a target when its ledger row still **stands**: not `held`, not withdrawn by a revert. That is the same
predicate `Applied`, the replay and the out-of-order guard use, so a run that applied three migrations and then failed on
the fourth has those three here, exactly as the target's own journal has them.

A target that does not have a migration is reported with the reason:

| Reading | What it means |
|---|---|
| *not there yet* | the target's newest standing version is below this one — it simply has not got there |
| *missing* | the target is already past this version and does not have it: it was skipped, or applied and reverted |
| *differs* | the target has this migration under other content, with the checksum it has |

Two more readings come from elsewhere in godwit. A migration a checkpoint collapsed is marked with the checkpoint that
recorded it: on a database built from the checkpoint it never ran, and the view says so rather than implying it did. A
migration whose file bodies retention has swept keeps its row with the content `unknown` instead of vanishing, because
dropping it would say the target does not have it.

Filters: by target, by version range, *not everywhere*, and `--in staging --not-in production` — what is ahead in
staging. The view is read-only and takes no position on what should follow from it: it does not refuse a run in
production because staging never saw it.

