# 0006 — What a repeatable declares is part of the desired schema

Shipped in #73; extended to `/ui/diff` in #77.

## The open question

A repeatable migration (`R__order_totals.up.sql`, a `CREATE OR REPLACE VIEW`, a function, a trigger body) declares objects an ORM schema knows nothing about. `Diff`'s desired side was the ORM's DDL alone, so those objects looked like objects nothing wants, and **both directions proposed dropping them, every time**.

For a Prisma or GORM team that also keeps a couple of `R__` files, `godwit diff` wrote a migration that deletes them, and `lint`'s `E005` — whose files base *does* rebuild them on the scratch — reported the same drop as permanent drift. The two checks disagreed about the same directory.

The question was whether the objects a repeatable builds belong to the desired schema, or whether the drop is real and should be filtered downstream.

## The decision

**They belong to the desired schema.** `DiffRequest.files` is now the migration directory under **both** bases, and its `R__` pairs are applied on the desired scratch database right after the DDL, in the order a run applies them. The object is then on both sides of the comparison and pg-schema-diff emits nothing about it — under `base = live` and under `base = files` alike, which is what makes the two coherent. `DiffResponse.repeatable_objects` names what appeared.

**Where the object set comes from: the scratch database's own catalog**, read before and after the repeatables are applied (relations, routines, non-internal triggers).

- *Not the planner's AST* — it sees only what the statements name, and a body can create more than it names.
- *Not `pg_depend` on the target* — it records dependencies, not which *file* made an object, so it cannot attribute anything on a live database.

On a scratch where nothing else ran, whatever appears is exactly what those files build.

## The four situations, and why the decision does not over-suppress

| Situation | Behaviour |
|---|---|
| A repeatable edited so it builds a **different** object | the new object is in the desired schema; the old one is in no file any more, so the `up` drops it — that drop is the point |
| A repeatable **deleted** from the directory | nothing declares its object, and the `up` drops it; deleting the file is how you retire what it built |
| An object a **versioned** migration created and a repeatable later **took over** | the `R__` file declares it, so it is in the desired schema and the diff leaves it alone |
| **No migration directory at all** | `failed_precondition`, naming the recorded repeatables |

On the last one: the RPC takes DDL, not files, and the honest answer when it is handed nothing is that **nothing is possible**. A request with no `files` while `godwit.repeatables` on the target has rows can see those objects but not what declares them, so its only options are to propose dropping all of them or to refuse. It refuses, naming them:

```
failed_precondition: target records repeatable migrations and the request carried no
migration files: R__order_totals
```

`godwit diff` sends `--dir` for exactly this reason. A target recording no repeatables is completely unaffected — nothing to attribute, nothing to refuse.

`/ui/diff` has no directory to send, and first shipped showing the refusal on exactly the targets most likely to have views and functions. Since #77 it supplies the `R__` pairs from a snapshot the control plane already holds — the file bodies stored with the target's newest plan (`cp_plan_files`), or with the run that last succeeded on it (`cp_run_files`) — or from boxes on the page. This is not the rejected "record what each repeatable created": nothing new is written, the store's own copy of the **files** is read back, and the attribution is still done on the scratch database at diff time from whatever those files build. Because a stored snapshot can be older than the repository, the page names the plan or run it read, the timestamp, and every repeatable whose checksum disagrees with what the target recorded. The refusal is untouched: the page may supply files, it never asks the service to skip the check, and with nothing to supply it still refuses.

One further error falls out of the mechanism and is worth having: a repeatable that no longer builds on the desired schema (the ORM dropped the column its view reads) is `invalid_argument` naming the file and PostgreSQL's error. The migration the diff was about to write would have broken it.

## Refused alternatives

| Thing | Reason |
|---|---|
| Filtering the generated statements by object name | This is the retired-column precedent, and it is wrong here. pg-schema-diff also emits `DROP VIEW … ALTER TABLE … CREATE VIEW` when a column a view depends on changes, even where the view is identical on both sides; removing those two statements by name would leave an `ALTER` that fails on the target. Building the object on the desired side instead lets the library do its own dependency ordering, and needs no text matching against generated DDL — which is multi-line. |
| Recording in the store what each repeatable created when it ran | It survives an edit or a delete as a stale claim, so the store would go on suppressing the drop of an object no file declares any more — which is exactly the case that must **not** be suppressed. The directory is the declaration; the store is the past. |
| Classifying the drop and letting the caller filter | `godwit diff` writes a file. A destructive statement that is only correct if someone downstream removes it is a footgun with extra steps. |
| A `godwit.yaml` key declaring the objects by hand | A second source of truth next to the `R__` file that made them, kept in sync by nobody. |

## Consequences

- **No store migration.** Nothing new is persisted, which is the point of the decision.
- **Only `R__` pairs are loaded from the request**, so a directory whose *versioned* files a run would refuse still diffs.
- **Two CLI test fixtures had to become loadable directories** rather than be worked around — one kept `schema.prisma` inside the migration directory, another used a placeholder the loader did not ignore. Both are now what `godwit plan` has always required a migration directory to be.
