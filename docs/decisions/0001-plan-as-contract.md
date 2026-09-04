# 0001 — The plan a reviewer approved is the contract the deploy applies

Shipped in #39 (plans, binding, staleness), #40 (hazard recipes), #41 (Action), #42 (already-applied detection), #43 (inspection and explicit override), #44 (transient retry and re-attach). Later corrected by #45 (apply before merge), #49 (`search_path` on the observation), #50 (repeatables in the key).

## The open question

The plan shown on a pull request and the run applied afterwards were two independent admissions. Anything that changed the target in between — a hand-made `ALTER`, another pull request's migration merging first, an edited file — was applied without anyone noticing that the thing being applied was not the thing that had been read.

The question was not *whether* to persist the plan. It was **what the deploy should do when the world has moved**, given that with two migration pull requests open the second one's plan always predates the first one's apply.

## The decision

A plan is persisted with an **observation** of the target at plan time, and `CreateRun` binds to it or refuses.

- **Plan key** = `sha256` over target, rollout and the ordered pending set (each migration's id, up checksum, down checksum). A pure function of the files and of which versions the target has applied — not a git SHA (squash merges change it), not the plan id. Re-planning the same set refreshes the `ready` row under a new id; there is one `ready` plan per `(target, key)`.
- **Observation** = `history_hash` over the live `godwit.migrations`, the schema fingerprint and definition, and the effective `search_path`. Taken from the target itself, not from what godwit last wrote, so a hand-baselined or hand-edited history is part of what the plan saw.
- **Binding** has three outcomes:
  - *fresh* — history hash and fingerprint match: bind, run carries `plan_id`, plan becomes `bound`;
  - *explained* — every added version came from a succeeded godwit run after the plan, nothing was removed, and the live schema is exactly what the last run left: re-plan silently, and bind only if the statement set is byte-identical;
  - *stale* — anything else: `failed_precondition` with a `PlanStale` detail naming the reason, the history added and removed with the run that made each, and the `+`/`-` schema lines. No row is written and the target is not touched.

**Why the `explained` case exists, and why it is not a weakening.** Strict "any difference refuses" was rejected because with two migration pull requests open the second one *always* fails at merge, for a reason that has nothing to do with it. Re-planning inside the bind is the same computation the pull request would have run had it been pushed again, and the contract holds because the statement set must come out identical. The alternative — an Atlantis-style per-target plan lock, first pull request to plan blocks the others — serialises every migration pull request in the repository.

**Never auto-healed**, in any case: a hand schema change since the plan, a version removed from the history, an order-guard failure, a validation failure, or a pending migration now applied with different content. A human reads the diff.

## What follows from it

- **`read` scope may persist plans.** The pull-request pipeline holds a read token; a stored plan is inert. Rejected: a new `plan` scope between `read` and `pipeline`, which would force pipeline tokens onto review jobs for no gain in what can be done with them.
- **A version applied with different content is now a refusal, not a skip.** Before this, the executor silently skipped such a file. It is now `invalid_argument` on `PlanRun{persist}` and `PlanStale{content}` at bind, because the key would otherwise be computed from files that do not describe the target.
- **Detection of migrations already applied by hand** (#42) falls out of the same machinery: the validator replays the history on a scratch database and snapshots after each pending migration, and the largest prefix whose fingerprint equals the live one is recorded rather than executed. It is prefix-only and conservative — a migration with any DML, or whose effect a snapshot cannot see (function, trigger, type, grant), is never marked and stops the walk.
- **Re-attach** (#44): a pipeline re-run finds the bound plan by a hash of the submitted files — not the pending-set key, which moves as the earlier run makes progress — and follows or resumes the run it already created. Only persisted plans re-attach; an implicit run has no stored identity, so re-running it queues a new run as before.
- **A version target cuts the pending set, and says so on the plan.** `--to <version>` (#75) stops a run at a chosen migration. Because the key is a hash of the pending set rather than of the directory, a truncated set gets its own key for free and `plan --to N` / `migrate --to N` bind to each other with no change to the key function. What the key cannot do is *say* that a truncation happened: a plan covering three of five migrations is indistinguishable from a directory holding three. So the client sends the whole directory plus the version as a field, the service cuts the set, and the migrations above the target are stored on the plan marked `withheld` and printed in the pull-request comment. Rejected: filtering the files client-side, which produces the same run and a plan that silently covers less than the directory — the reviewer's only signal would be a version number they were not shown.
- **Retries are for transient failures only.** Lock timeouts, deadlocks, `statement_timeout`, connection-class SQLSTATEs and network errors requeue with exponential backoff and jitter; everything else fails on the first attempt. Run errors are prefixed `transient:` or `sql:` so a reader knows whether to page.

## Consequences to live with

- **`cp_plan_files` is the second-largest table in the store.** Every pull-request plan stores its file bodies. `--plan-retention` (default 90 days) sweeps `bound` and `superseded` plans on the drift ticker; `ready` plans and plans of unfinished runs are never swept. After a sweep, `run.plan_id` is `NULL` and the `run.create` audit entry is the durable answer to "which plan did this run apply".
- **Two `godwit plan` commands share a name.** With a target configured, `godwit plan` talks to the service and stores a plan; without one it parses files offline. `godwit.yaml`'s `target` key therefore silently switches the mode, and only `--target ""` restores the offline form. Documented in `configuration.md`; not fixed, because both forms are wanted.
- **Recipes are hints, never a reason to refuse.** A hazard carries ready-to-copy SQL built from the parsed AST (#40); a deparse failure renders as a comment inside the recipe rather than failing the plan.

## Refused or deferred

| Thing | Verdict | Reason |
|---|---|---|
| A dedicated connect error code for a stale plan | refused | No client can act on it differently from `failed_precondition`. The typed detail carries the structure; the message carries the same report as text, so curl, the Action comment and Slack need no decoding. |
| JSON in the error message | refused | Unreadable in a terminal, which is where the refusal is usually first seen. |
| Keying the plan on the full file set | refused | Any unrelated merge would break binding. The key covers the pending set only, so a removal is reported as a removal exactly when the pending set is unchanged, and otherwise simply produces no matching plan. |
| Bypassing `--plan-ttl` for an explicit `migrate --plan <id>` | refused | An operator pasting an old id gets exactly the stale-observation risk the TTL exists to prevent. |
| A `to_version` key in `godwit.yaml` | refused (#75) | A standing version target would truncate every run in the repository from then on, invisibly. It is a per-invocation intent; the flag is the only way to state it. |
| Treating an unknown `plan_id` as "no plan" | refused | The caller asked for one plan by name; falling back to an implicit run would apply something else under that request. |
| A `reverted` plan state | refused (#45) | After `/godwit revert` the *run* is `reverted` and the plan stays `bound` until the next `CreateRun` with the same key retires it. This is a divergence from the plan document, which assumed a fourth plan state; the plan states on `main` are `ready`, `bound` and `superseded`. |

## Where the plan document was overtaken

The design document behind this record was written before the code and is wrong in places. What shipped:

- **Store migration order.** The document assigned `20260901000010 retry` to the retry work and left inspection unnumbered; the migrations on `main` are `000009 plans`, `000010 retry`, `000011 plan_retention`, then `000012 plan_search_path`, `000013 plan_repeatables`, `000014 plan_expansions`, `000015 run_ledger`.
- **The observation gained `search_path`** (#49) and the plan gained `repeatables` (#50) and `expansions` (#56). None of the three was foreseen; each is load-bearing. `search_path` in particular was discovered while implementing already-applied detection: without mirroring the target's path onto the scratch session, a store role named `godwit` puts unqualified tables in a different schema from the target's role and fingerprints never match.
- **Already-applied detection gained an apply-time re-check** beyond the specified design: the scheduler re-observes the target before recording, so a late hand change or a retry after a partial failure cannot record a migration whose effect is no longer there. The `INVALID` index check is global rather than per-migration index name — an invalid index is always wrong to leave behind.
- **A `note` field on the planned migration** was added so the CLI can say inline *why* a migration was not marked, rather than carrying a separate list.
- **Owner decision, applied before merge.** The document's later addendum chose the Atlantis model: `/godwit apply` from the pull request, a `godwit/applied` status check gating the merge, `main` never carrying an unapplied migration, with `apply-on-merge` kept as an opt-in. That shipped in #45. What did not ship with it is the plan state change described above.
