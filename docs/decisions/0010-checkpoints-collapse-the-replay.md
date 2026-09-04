# 0010 — A checkpoint is a file in the directory, and what it collapses can never be reverted

Shipped in #85.

## The open question

Every plan replays the target's whole recorded history on a scratch database before it is admitted. That replay is the load-bearing part of five other features — already-applied detection (#42), the plan contract's observation (#39), directive expansion frozen once (#56), `godwit diff --base files` and the ORM drift gate (#61) — and its cost grows with every migration ever merged. `docs/comparison.md` listed checkpoints as a gap and named baselining as the escape hatch, which it is not: a baseline is per target, does not travel with the repository, and cannot be reviewed.

Atlas has the feature and its shape is settled: a file in the migration directory carrying `-- atlas:checkpoint` and the whole schema, used so that "if you have a checkpoint, you can replay only the migrations that were added since the latest checkpoint". The owner's instruction was to do what Atlas does. Three things Atlas's documentation does not settle had to be decided anyway, because godwit's replay is not Atlas's.

## What is different about godwit's replay, and why it changes the answers

Atlas replays **the migration directory**. godwit replays **the target's ledger** (#68, #71, #75): what each run applied and no revert undid, with each migration's body read back from the run that carried it and each directive's expansion frozen on its own row. So:

1. A checkpoint has to be something the *ledger* can contain, not only something the directory can.
2. The migrations it collapses have to end up in the target's `godwit.migrations`, or the next plan finds them pending again.
3. Whatever is collapsed must still count as *replayed*, or the expand-once rule for directives breaks and a `change-type` under the checkpoint gets expanded a second time.

## The decision

### A file in the directory, marked by a directive — not a row in the store

```sql
-- godwit: checkpoint through=20260430120000
CREATE TABLE public.users (...);
```

Atlas's shape, spelled in godwit's own directive grammar, plus one field Atlas does not need: `through=`, the newest version the body accounts for.

*Why a file.* It travels with the repository, so a target godwit has never seen can be built from the directory alone; it is reviewed in the pull request that adds it, by the same `lint` and `plan` as any migration; it is in the plan key, so a plan taken before it does not bind after it; and `godwit diff --base files` and the ORM drift gate see it, because they build their base from the same files. *A row in the control plane was rejected* on all four counts: it would make the repository and the service disagree about what the directory produces, and a fresh clone would have nothing to apply.

*Why `through=` rather than Atlas's "everything above it in the directory".* godwit records a row per applied migration and has to mark the collapsed ones truthfully, and it has to be able to tell a target that is genuinely caught up from one that stopped half way. Reading coverage off the directory makes both undecidable the moment someone deletes a collapsed file. `through=` is the checkpoint's own claim about what its body contains, and it is what the mid-history refusal is checked against.

*Why no `.down.sql`.* The loader requires one for every other migration. Requiring one here would produce a file that drops a hundred migrations' worth of schema and that nobody has run — exactly the ceremonial down file 0005 refused to make mandatory. The loader now allows a checkpoint with no down side and refuses one that has it.

### Generated from a scratch replay of the files, never from a target — and verified

`godwit checkpoint --name squash` replays the versioned migrations at or below `--at` on a scratch database, expanding any directive among them against the catalog the ones before it left, renders that schema as DDL, then **applies the generated DDL alone on a second scratch database and refuses unless the fingerprint comes out identical**.

*Why the files and not a live target.* Dumping a target bakes that target's drift into the repository — the hand-made `ALTER`, the column added during an incident — and then tells every other target it is missing it. The scratch replay is exactly what the validation replay would have produced, which is the property the checkpoint has to preserve.

*Why the verification is not optional.* The renderer is pg-schema-diff, already trusted for `godwit diff`, and its output has known holes (domains, composite types, exclusion constraints, comments, roles). Without the check those holes become silent data loss at apply time on a fresh database. With it they become a refusal at generation time, on the author's machine, naming the difference. **Rejected: writing a DDL dumper inside godwit.** 0005 already refused to build a schema-introspection and planning engine on the grounds that godwit is a versioned-SQL-file runner; a checkpoint is not the place to reverse that. **Rejected: shelling out to `pg_dump`.** It would put a client binary of a matching major version in the service image and make the output unverifiable by anything but the same check we are doing anyway.

### Which database does what: a pure function, recomputed everywhere

| The target has applied | The checkpoint | What it collapses |
|---|---|---|
| nothing | runs | recorded without running, same run |
| everything it collapses | recorded without running | already applied |
| some of them | recorded, after the rest have run | the missing ones run from their own files |

The first row is Atlas's rule. The second and third are the ones Atlas's documentation leaves open, and both fall out of the same decision: **a checkpoint is never run on a database that already holds part of what it carries.**

The decision is a pure function of `(files, newest applied version)`, so it is recomputed at plan time, at apply time in the scheduler, and inside the scratch replay, rather than persisted on the plan. That is deliberate: it is the same choice #39 made for the plan key, and it means the decision can never go stale between planning and applying while the target moves.

*Recording rather than running is not a new mechanism.* It is `Plan.MarkOnly`, the path a baseline and already-applied detection already use.

*The mid-history case is not special-cased.* The migrations between where a target stopped and `through=` are simply pending; they run from their own files, in order, and the checkpoint is recorded once the target arrives. It only breaks when those files are gone, and then godwit refuses by name — `ErrCheckpointGap`, naming the version it cannot get to and the two ways out (restore the files, or baseline at the checkpoint). **Rejected: applying the checkpoint anyway on a partially-migrated database**, which is the only other option and is silent corruption.

### The replay starts at the newest checkpoint the target holds

The replay flattens the ledger, finds the newest row that is a checkpoint, executes that row first, drops every versioned row at or below its `through=`, and writes those into the scratch `godwit.migrations` in one statement. Everything else keeps its order.

Every consumer of the replay was checked against this:

- **Already-applied detection (#42)** walks the fingerprints of the pending set on top of the replayed base; the base is the same schema either way, so the walk is unchanged.
- **Expansion frozen once (#56)** reads the `replayed` set, which now includes the collapsed ids, so a directive under the checkpoint is never expanded again — and its expansion is baked into the checkpoint's body rather than surviving as a directive.
- **`godwit diff --base files` and the ORM drift gate (#61)** go through the same `Replay`, so they get the short replay and the same base.
- **The ledger (#68/#75)** is untouched: the checkpoint is a row like any other and the collapsed rows stay where they are, which is what the revert gate reads.
- **Repeatables (#50)** are never collapsed. Their identity is their body, they have no version, and the checkpoint's body is generated from versioned migrations alone — so every repeatable in the history still replays on top of it.

## What is lost, on purpose

- **Nothing at or below a checkpoint can be reverted.** `PlanRevert` refuses a run whose standing ledger holds the checkpoint (`it is a checkpoint, and a checkpoint has no inverse`) or a migration the checkpoint collapsed (`checkpoint <id> collapsed it: ...`), and `godwit down` refuses a checkpoint offline. This is the honest reading of what a checkpoint is: on a target that started from it those migrations never ran, their down files were written against states that target never passed through, and the replay would rebuild them from the checkpoint's body regardless — a revert would report success and leave permanent drift. Refusing is the same call 0005 made about the data-loss gate: fail where a warning would be read as permission.
- **Data a collapsed migration inserted is gone from the checkpoint.** The body is schema only, as in Atlas, whose documentation states the same limitation. A history that seeds rows keeps them above the checkpoint with `--at`, or writes the `INSERT`s into the file by hand.
- **The collapsed files are still needed** and godwit does not delete them. They are what carries a target that stopped below the checkpoint.

## Where godwit diverges from Atlas, and why

| | Atlas | godwit |
|---|---|---|
| Marker | `-- atlas:checkpoint` | `-- godwit: checkpoint through=<version>` — the same idea in the existing directive grammar, plus an explicit claim about coverage |
| Coverage | position in the directory | `through=`, recorded in the file |
| Down side | none | none, and a `.down.sql` next to a checkpoint is a load error |
| What the replay is | the migration directory | the target's ledger, so the checkpoint is a ledger row too and an existing target's replay gets shorter as well, not only a fresh database's setup |
| Marking the collapsed set | not applicable | recorded in `godwit.migrations` as the checkpoint runs, so old and new targets have the same history |
| Mid-history database | not documented | stated: run the remaining files, then record the checkpoint; refuse by name when they are gone |
| Verification | not documented | the generated body is replayed on a second scratch database and the fingerprint must match, or the checkpoint is refused |
| Revert below it | not documented | refused explicitly, with the reason |
| Availability | Atlas Pro | in the binary |

## Refused or deferred

| Thing | Verdict | Reason |
|---|---|---|
| A checkpoint as a store row | refused | Does not travel with the repository; a fresh clone would have nothing to apply, and `diff`/`lint` could not see it. |
| Deleting the collapsed files as part of `godwit checkpoint` | refused | They are what carries a target that has not reached the checkpoint. Deleting them is a separate, later decision the team takes once `godwit targets` says every target is past it. |
| Collapsing repeatables into the checkpoint | refused | A repeatable re-applies when its body changes; freezing a body into a checkpoint would either pin it or make it re-apply forever. They replay on top, which is also the argument for keeping views, functions and triggers in `R__` files. |
| A `checkpoint` key in `godwit.yaml` | refused | Same reasoning as `to_version` in 0001: a standing setting that reshapes every run in the repository, invisibly. |
| Generating the body with `pg_dump` | refused | A client binary of a matching major version in the service image, output godwit cannot verify any better than it verifies its own, and a new failure mode on every PostgreSQL release. |
| Writing a DDL dumper inside godwit | refused | 0005 refused to build a schema engine; pg-schema-diff is already the dependency that renders schema, and the fingerprint check is what makes trusting it safe. |
| More than one checkpoint in a directory | allowed, untested at scale | The newest one wins everywhere (the replay, the shaping, the revert bar). Generating a second one over the first is refused — a checkpoint is generated over the migrations *above* the newest one — so the only way to have two is to keep an old one deliberately. |

## Consequences to live with

- **The plan surface gained two fields** (`PlannedMigration.checkpoint`, `.collapses_through`) and a note that says which of the three behaviours the target will get. Without it a reviewer cannot tell a checkpoint that is about to build the whole schema from one that is about to be recorded and do nothing.
- **`Store.History` had to stop inner-joining the down file.** A checkpoint has no `.down.sql` row in `cp_run_files`, and the join silently dropped it from the history — which would have made the replay skip the very file it was supposed to start from. It is a `LEFT JOIN` with `coalesce` now.
- **A checkpoint's own body goes through the hazard planner.** pg-schema-diff emits `CREATE INDEX CONCURRENTLY`, so the file plans as non-transactional statements and is applied by the executor with the journal, the same as any migration — which is also how the verification runs it, so a body godwit could not plan is refused at generation rather than at apply.
- **`--at` cuts by version, not by count.** There is no `--keep-last 50`: a count is a moving target that means something different on every branch, and the version is what the file says and what the refusal quotes.

## Sources

- https://atlasgo.io/versioned/checkpoint
- https://atlasgo.io/blog/2023/08/31/atlas-v-0-14
- https://atlasgo.io/versioned/apply
- https://atlasgo.io/versioned/down
