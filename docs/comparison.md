# Comparison

godwit against Flyway, Liquibase and Atlas, as of the code on `main`. Written by the godwit side; the other columns come from their public documentation (linked at the end), not from running them. Where godwit lacks something, it says so.

godwit is narrower on purpose: PostgreSQL only, plain SQL only, versioned only, and it is a service rather than a CLI. If you need another database, a DSL, or declarative schema-as-code, stop reading: it is not a fit.

## Feature by feature

| Capability | godwit | Flyway | Liquibase | Atlas |
|---|---|---|---|---|
| Databases | PostgreSQL | many | many | many |
| Migration format | plain SQL, up + down required | SQL, Java | XML/YAML/JSON/SQL changelogs | SQL, HCL, ORM-derived |
| Schema history | `godwit.migrations` in the target, plus per-statement `godwit.runs`/`godwit.journal`; run history in the service store | `flyway_schema_history` | `DATABASECHANGELOG` | `atlas_schema_revisions` |
| Checksum validation | yes: applied version with a different checksum refuses; resume refuses if a statement's hash changed; `lint --base` flags edited merged files (`E003`) | `validate` | checksum on changesets | `atlas.sum` file |
| Repair / realign checksums | no, by design; write a new migration | `repair` | `clearCheckSums` | `migrate hash` |
| Crash safety | statement-level journal with write-ahead intent for non-transactional statements; resume from the last done statement after any crash | per-migration transaction where the DB allows; `failed` row needs `repair` | per-changeset; manual cleanup | per-file; `migrate status` shows partial |
| Locking | session advisory lock per target database, plus a lease per target in the service | lock on the history table (advisory lock on PostgreSQL) | `DATABASECHANGELOGLOCK` | advisory lock |
| Undo / down | required for every migration; `RevertRun` goes through the same gate and executor | Undo migrations (paid tiers) | rollback (auto for some changes, manual for SQL) | down migrations (paid) |
| Dry run against a live database | `PlanRun` / `migrate --dry-run`: hazards, ordering, scratch validation, applied state, phases | `migrate -dryRunOutput` (paid) | `updateSQL` | `migrate apply --dry-run` |
| Pre-flight validation | replays the target's whole recorded history plus the new files on a scratch database before queueing | none built in | preconditions | `migrate lint` with dev database replay |
| Lint / safety analysis | H001–H010 (PostgreSQL DDL hazards), `lint` command, hazard acknowledgement is part of the run | none in Community | none | analyzers (destructive, data-dependent, concurrent index, ...) |
| Drift detection | fingerprint after every successful run, periodic monitor, events, accept | drift check (Community preview since 10.20) | `diff` command | `schema diff` / `schema inspect` |
| Expand/contract rollout | built in: `expand-contract` policy holds destructive migrations until `ConfirmRollout` | no | no | no |
| Out-of-order migrations | refused unless `--allow-out-of-order` | refused unless `outOfOrder=true` | allowed by default | refused by default (`--exec-order`) |
| Baseline an existing database | `BaselineTarget` (target's journal must be empty) | `baseline` | `changelogSync` | `migrate set` |
| Target version (`--to`) | no; send fewer files | `-target` | `update-to-tag`/count | `migrate apply N` |
| Repeatable migrations | no | `R__` migrations | `runOnChange` / `runAlways` | no |
| Placeholders / templating | no; template in the pipeline | placeholders | properties | template dirs |
| SQL hooks (before/after) | no; webhook and Slack notifications only | callbacks | not built in | no |
| `search_path` per target | no; migrations must qualify names, journal lives in `godwit` schema | `schemas` | `defaultSchemaName` | `--schema` |
| Contexts / labels | no; use targets and directories | no | yes | no |
| Tag and rollback-to-tag | no; reverts are per run, newest first | no | `tag`, `rollback <tag>` | no |
| Checkpoints | no; baseline is the escape hatch when history replay gets slow | no | no | yes |
| Declarative schema | no | no | no | yes (`schema apply`) |
| Migration from a desired schema | `Diff` / `godwit diff --schema`: up and down SQL from the live target to a DDL file, hazards and recipes on the result, drift against the history shown; tables, keys, foreign keys, indexes, sequences, enums, functions, triggers, views, policies, extensions (via pg-schema-diff); no domains, composite types, exclusion constraints, comments or roles | no | `diff-changelog` | `migrate diff` (schema in HCL, SQL or from an ORM; dev database) |
| Retention of history | no command; documented SQL | n/a | n/a | n/a |
| Multi-target ordering | no; one target per run, the pipeline orchestrates | n/a | n/a | n/a |
| Service / API | connect (gRPC + JSON) service with scoped tokens, leases, replicas | CLI (Enterprise has a hub) | CLI (Pro has a hub) | CLI (Atlas Cloud) |
| Audit trail | `cp_audit` with actor per mutation; `created_by` and `source` on runs | history table only | history table only | Atlas Cloud |
| Per-target / per-run timeouts | `lock_timeout` and `statement_timeout` on the target and per run | connection properties | connection properties | connection properties |
| CI/CD | composite GitHub Action (lint and stored plan on the pull request, migrate bound to it on the merge commit, outcome posted back on the pull request), ArgoCD hook Jobs, Helm chart | Docker image, various actions | Docker image, GitHub Action | `atlas-action` |
| Notifications | webhook JSON, Slack (thread or edit mode) | no | no | Atlas Cloud |
| Web UI | in progress (`feat/ui` branch, not merged) | Desktop (paid) | Hub (paid) | Cloud |
| Licence | Apache 2 | Apache 2 core, paid tiers | Apache 2 core, paid tiers | Apache 2 core, paid tiers |

## What godwit does not have, and why

- **Repeatable migrations.** A view or function file that re-applies whenever its content changes conflicts with "the journal is the truth" unless modelled as checksum-keyed re-runs. Planned for after v1; today, put the `CREATE OR REPLACE` in a versioned migration.
- **`search_path`.** Migrations must qualify schema names; the journal always lives in `godwit`. A per-target `search_path` is small and planned; not there.
- **`--to <version>`.** Send fewer files. A client-side filter is planned; the service does not need to change.
- **Retention command.** Growth is one journal row per statement and one file body per run; the SQL to prune is in [operations](operations.md#retention).
- **SQL hooks.** Notifications are the operational hook. Running SQL before or after a run needs a design decision (per run or per migration) that has not been taken.
- **A declarative apply.** `godwit diff` generates the migration from a desired schema, but what runs is always a versioned file that went through the gate, and the output has holes (domains, composite types, exclusion constraints, comments, roles) that Atlas does not have. It is a shortcut for writing the file, not a replacement for the history.
- **Checkpoints.** Validation replays the full recorded history; on a long history that becomes slow. Baselining a target resets the replay root and covers the same need until it does not.
- **Repair.** There is no dirty state to repair: a failed run resumes from its journal, and a changed file is refused until you write a new migration.
- **Multiple databases.** The planner is `libpg_query`; every hazard is a PostgreSQL lock or rewrite semantics. Nothing here ports.

## What godwit has that the others do not, in one line each

Statement-level crash recovery with verified resume of `CREATE INDEX CONCURRENTLY`; hazard gate with acknowledgement as part of the run request; scratch-database replay of the full history before admission; expand/contract as a run state instead of a convention; a leased multi-replica service with scoped tokens, audit and drift monitoring in the same binary.

## Sources

[Bytebase: Flyway vs Liquibase (2026)](https://www.bytebase.com/blog/flyway-vs-liquibase/) · [Flyway Community drift check](https://www.postgresql.org/about/news/flyway-community-drift-check-released-2970/) · [Flyway pricing](https://www.g2.com/products/redgate-flyway/pricing) · [Atlas versioned apply](https://atlasgo.io/versioned/apply) · [atlas-action](https://github.com/ariga/atlas-action)
