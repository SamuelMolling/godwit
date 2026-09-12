# Runs

The life of a run on the control plane: its states, the lease that executes it, how a rollout is split, how far a run may go, and how one is undone.

## Runs and states

A **run** is one request to apply a set of files to one target. `cp_runs` carries `state`, `attempts`, `rollout`, `phase`, `kind`, `reverts`, `created_by`, `source`, per-run timeouts and the error; `cp_run_applied` carries one row per migration the run actually applied, which is what a [revert](#revert) acts on.

```
              CreateRun / RevertRun                       ConfirmRollout
                     │                                          │
                     ▼          claim                           ▼
                  queued ───────────────► running ◄────────── queued (phase = contract)
                                            │
             ┌──────────────┬───────────────┼───────────────────┐
             ▼              ▼               ▼                   ▼
         succeeded      failed      awaiting_contract     needs_attention
             │              │                                   │
             │              └─────── ResumeRun ──► queued ◄──────┘
             ▼
         reverted   (set on the original when its revert succeeds)
```

| State | Meaning | Terminal |
|---|---|---|
| `queued` | admitted, waiting for a replica to claim it | no |
| `running` | a replica holds the lease and is executing | no |
| `succeeded` | every statement of the phase applied | yes (until reverted) |
| `awaiting_contract` | `expand-contract` run: expand phase done, contract statements held back | until `ConfirmRollout` |
| `failed` | a statement failed with a genuine SQL error (`sql:`); the journal keeps the progress | until `ResumeRun` |
| `needs_attention` | the attempt budget is exhausted (`transient: gave up after N attempts`) or an operator parked it with `ParkRun` | until `ResumeRun` |
| `reverted` | a later revert run of this run succeeded | yes |

A genuine SQL failure goes straight to `failed`. A transient one (classes `08`, `53` and `57` except `57P04`, plus `40001`, `40P01`, `55P03`, `58000`, `58030`, a lost connection, a deadline) puts the run back in `queued` with `not_before` set: the wait is `--tick-interval` doubled per attempt, capped at 5 minutes, with ±20% jitter, and `retries` counts them. `needs_attention` is reached when the run has used `--max-attempts` attempts, whether lost leases or transient failures, or by `ParkRun`. `ResumeRun` puts either back in `queued` with `attempts = 0` and no wait.

A pipeline re-run does not queue a second run: `CreateRun` with the same files, target and rollout as a bound plan re-attaches to that plan's run (`reattached` in the response, `run.reattach` in the audit). A `queued`, `running` or `awaiting_contract` run is simply followed; a `succeeded` one is followed too if the target still has everything it applied; a `failed` or `needs_attention` one is resumed when the only history the plan did not know is the run's own progress; a `reverted` one releases the plan and a fresh run is created. An explicit `--plan <id>` skips re-attaching.

`kind` is `migrate`, `baseline` or `reconcile`; `phase` is `expand` or `contract`; `reverts` links a revert run to the run it undoes, and `cp_run_applied.reverted_by` links each undone migration to it.

**A run's state is not a verdict on its migrations.** A run that applies three migrations and fails on the fourth leaves the three standing: they are in the target's `godwit.migrations`, in its journal, and in `cp_run_applied`. So the applied set, the applied count, the out-of-order guard and the scratch replay are all scoped to the ledger row, never to `cp_runs.state`: a migration counts when its row is not `held` and not `reverted_by`. `held` is the other half — the run applied that migration's expand statements but its contract phase never ran, so the target records nothing for it yet, and it counts as applied only once the contract phase lands (a revert still undoes it, from the `down_held_sql` frozen on the row). The one thing `state` still decides is whether the run itself can be resumed, confirmed or reverted.

**A ledger row also records how it got there.** `adopted` marks a migration the run *found* already recorded on the target rather than one it applied — every row a `baseline` or `reconcile` writes, and the row a `migrate` writes when it walks past a migration the target's journal already holds. Adopted rows are standing rows everywhere the applied set matters, and they are out of `revert`'s scope, because a revert undoes what its run applied.

## Leases

Runs are executed by whichever replica claims them. Every replica ticks every `--tick-interval` (2s):

- `Claim`: pick one `queued` or `running` run whose lease is missing or expired, at most one per target, `FOR UPDATE SKIP LOCKED`. Set it `running`, `attempts + 1`, write `cp_leases(run_id, holder, expires_at = now + ttl)`.
- Heartbeat every `ttl / 4` while executing; a beat that fails is retried every `ttl / 10`. Past a fifth of the lease of failing beats, and at once on a lease taken by another holder, the replica logs `heartbeat lost`, counts `godwit_heartbeat_failures_total`, cancels the run it can no longer prove it owns and writes nothing more about it.
- On finish, delete the lease and set the final state.

A replica that dies stops heartbeating; after `--lease-ttl` (30s) another replica claims the same run, `attempts` becomes 2, and the executor resumes from the journal. If `attempts` exceeds `--max-attempts` (5) at claim time the run is finished as `needs_attention` without executing; a run held back by `not_before` is not claimed until the store's clock passes it.

`holder` identifies the **process**, not the machine: `<name>/<16 hex characters>`, where the name is `--holder` (or `GODWIT_HOLDER`, or the hostname) and the suffix is drawn once at start-up. Everything that keeps two replicas off one run compares it whole, so the suffix is what makes the lease exclusive when two replicas share a hostname — ECS `host` networking, a cloned VM image, two processes on one box. A restarted replica comes back with a new identity and therefore cannot reclaim its own in-flight run before the lease expires, which is deliberate: the backend it left in the target still holds that target's advisory lock, and the TTL is what gives it time to die.

One target executes one run at a time: the claim query hands out one lease per target, and the advisory lock on the target itself serialises executors that end up overlapping (a stalled replica whose lease expired and its successor), so the second one waits rather than interleaving statements.

## Version targets

`godwit plan --target <name> --to <version>` and `godwit migrate --to <version>` stop at a chosen migration: everything at or below that version runs, everything above it is left for a later run. It is how you land a large branch one migration at a time without editing the directory, and how a pipeline applies the expand-side migration today and the contract-side one tomorrow from the same commit.

**The whole directory is still submitted.** The client sends every file and the version target as a separate field; the service is what cuts the set. Filtering client-side and sending fewer files would produce the same run and a plan that *silently* covers less than the directory — the reviewer has no way to tell an intentional stop from a directory that only holds three migrations. So the migrations above the target stay on the plan, marked `withheld`, and appear in the text output, the markdown pull-request comment, `godwit plan show` and the JSON:

```
1 migration will be applied to app.
withheld: 2 migration(s) in the directory this plan does not cover (20260901120100_b, R__v)

+ 20260901120000_a  1 statement
  statement 0, runs inside a transaction
      CREATE TABLE a (id int);

not executed by this run (2):
  20260901120100_b  held back by --to
  R__v  held back by --to
```

A withheld migration is not part of the pending set: it is not planned, not validated, not expanded, not hazard-gated and not in the run's files. It is a name on the report and nothing else.

**The plan key already handles it.** The key is a hash of the *pending set*, not of the directory ([plans](admission.md#plans)), so a plan taken with `--to 3` gets its own key and a `migrate --to 3` from the same commit binds to it. A `migrate` without the target computes a different key from the same files and simply finds no plan — an implicit run, or `PlanRequired` naming the nearest plan on a `require_plan` target. Nothing about the key changed for this feature.

**Repeatables are held back with the versions they ship with.** An `R__` file states the object the whole directory declares, and it is usually edited in the same commit as the migration that makes it valid. Building it against a history the target has not reached yet either fails validation or installs a view over columns that do not exist. So: a repeatable runs when the version target holds back no pending versioned migration, and is withheld otherwise. A target at or above the newest pending version withholds nothing and is a no-op.

**A directive below the target expands for the truncated point in time.** Expansion runs on a scratch database carrying the target's replayed history plus the submitted files — which are now only the files at or below the target — so a `change-type` at version 2 expands exactly as it would have if versions 3 and 4 had not been written yet, and the expansion is frozen on the run as usual. Under `expand-contract` the rollout split also happens over the truncated set, so the contract phase covers only what is at or below the target.

**The order guard is untouched.** Cutting the tail off a pending set cannot produce a version below the newest *applied* one, so a version target never trips the out-of-order guard and never needs `allow_out_of_order`; deferring the tail and back-filling a hole are different things. Applying the rest later is in order by construction.

**"Applied" here is the ledger's answer**, the same one the order guard asks: every migration a run carried to completion that no revert undid, including one a run that later failed had already landed. So a version a failed run got in is genuinely behind the history and refused, and it genuinely holds nothing back — a repeatable above it still runs.

**Reverting is unaffected.** A run created with a version target records in its ledger exactly the migrations it applied, and `revert` acts on that ledger and on the newest un-reverted run ([revert](#revert)) — so `godwit revert` after `migrate --to 3` undoes migrations 1 to 3 and nothing else, with no `--to` of its own. A `revert --to <version>` walking backwards *across* runs is a different feature with a different unit and a different data-loss story, and is not built.

### What a version target refuses

Every refusal names the version and what the target holds, before anything is stored or run.

| Situation | Refusal |
|---|---|
| The submitted set has no migration with that version | `invalid_argument`: `no migration in this set has version <v>`, listing the versions it was given. A version target names a file, never a point between two |
| The version is below the newest one the target has applied | `failed_precondition`: `version target <v> is behind version <w>, already applied on <t>: a target stops a run short, it never reverts`. This is the Django and Alembic reflex — there `migrate <app> <target>` unapplies — and a silent no-op would read as success. `godwit revert` is the undo |
| Everything at or below the version is applied while migrations above it are pending | `failed_precondition`, naming the first pending migration. A stated intent that selects nothing is a mistake, not an idempotent re-run. A target with nothing pending anywhere is the idempotent case and runs as usual |
| `--to` together with `--plan <id>` | `invalid_argument`: the stored plan already fixes the set it covers |
| `--to` without a target | `--to needs --target`: what it holds back is decided against the versions that target has applied, and the offline `godwit plan` has no target to ask |
| `--to 0` or a negative value | `--to takes a migration version, the 14 digits its file name starts with` |

There is no `to_version` key in `godwit.yaml`. A standing version target would truncate every run in the repository from then on, invisibly — the one failure mode this design exists to prevent.

## Rollout policies

| Policy | Behaviour |
|---|---|
| `direct` (default) | every plan runs in the expand phase; the run ends `succeeded`. The plan report names no phase here: nothing is held, so there is no second half to distinguish from the first. |
| `expand-contract` | statements up to the first contract statement run now; that statement and everything after it are held; the run ends `awaiting_contract` (or `succeeded` when nothing was held). `ConfirmRollout` re-queues it with `phase = contract`; the executor skips the already-applied plans and runs the rest. |

The split is by statement, and a migration whose statements carry no phase of their own has a single phase. A statement belongs to the contract phase when it says so (`Statement.Phase`, which only a directive expansion sets today) or, failing that, when its migration carries a contract hazard — so a hand-written migration mixing `ADD COLUMN` and `DROP COLUMN` still lands in the contract phase whole, and only an expansion splits a migration down the middle.

On a pull request the GitHub Action makes the hold visible: an apply that ends `awaiting_contract` leaves the `godwit/applied` commit status at `pending` ("expand applied; comment `godwit confirm` to run the contract phase"), so branch protection keeps the pull request unmergeable until `godwit confirm` releases the same run and the status turns `success` ([CI/CD](../ci-cd.md#pull-request-confirm-the-contract-phase)).

A migration split down the middle is **not** run twice. The expand phase stops at `Plan.HoldFrom`, the index of the first held statement, and returns without recording the migration: the target's `godwit.runs` row stays `running` and no `godwit.migrations` row appears. `ConfirmRollout` re-queues the same control-plane run with `phase = contract`, which rebuilds the plan with every statement and no hold; `openRun` finds that still-open target run, `loadProgress` checks the hash of each journalled statement against the rebuilt plan — the list is identical, only the hold differed — and execution resumes at `lastDone + 1` on the same run id before finalising it. A crash anywhere in either phase resumes through the same journal, with no extra state.

## Revert

Why the scope is what it is, and the survey behind it: [decision 0005](../decisions/0005-revert-scoped-to-the-ledger.md).

`RevertRun` queues a new run of kind `migrate` with `reverts` set, whose plans are the **down sides of the
migrations the original run actually applied**, in reverse order of application. Not every file it carried:
`godwit migrate` sends the whole migration directory on every run, and the files it skipped as already
applied belong to whoever applied them.

**The ledger is the scope.** As a run applies each migration the scheduler writes a row in
`cp_run_applied` — the migration id, its order, whether its contract phase is still held, and the directive
expansion frozen for it. A skipped migration writes nothing. `RevertRun` reads those rows back, narrows the
run's stored files to their up/down pairs, and reverses them. That is what `godwit run get` prints under
`applied:` and what the run page shows as *What it applied*. This is Liquibase's `rollback-one-update`
scope — DATABASECHANGELOG rows, not changelog files — and it is why `revert` needs no confirmation prompt:
the set is a record of fact, not a statement of intent.

Because the expansion lives on the migration's own ledger row rather than on the run, a migration carrying
a `-- godwit:` directive no longer blocks the revert of any *later* run: the later run's plan never contains
it, and the directive run's own revert reads the inverse frozen for it.

**Target.** `RevertRun{target}` with no `run_id` acts on the newest un-reverted run of that target, and
never on anything wider — there is no "revert everything". Naming an older run is refused
(`run y is newer and still stands`) unless `force` is set; unwinding three runs is three explicit calls,
newest first. Baseline runs cannot be reverted, and neither can a revert.

**Plan first.** The response always carries the plan — the down statements per migration, in the order they
will run, plus what the plan would destroy — and `dry_run` returns that plan without queueing anything.
`godwit revert` prints it before it watches the run; the GitHub Action puts the dry run in the pull-request
comment.

**Data loss is refused, not warned about.** godwit counts what each `DROP TABLE` (rows) and `DROP COLUMN`
(non-null values) in the plan would destroy on the live target, and refuses the whole revert when anything
is left:

```
revert would destroy data: 20260101000000_orders drops table public.orders holds 12482 row(s);
pass allow_data_loss (--allow-data-loss) to run it anyway
```

Atlas is the only other tool in this space that blocks rather than warns, and it is the right call: the
failure mode everyone documents is data loss, and a warning in CI logs is not read. The trade-off is
deliberate — this makes `revert` fail in exactly the moment someone is panicking, when the correct action
is usually roll-forward or restore-from-backup. The gate reads the down files *you* wrote; a
`-- godwit: revert` inverse is generated, and godwit refuses to generate one wherever it would not be
lossless, so generated inverses are exempt.

**History is added to, never subtracted from.** The revert run is its own row; the original stays and turns
`reverted`; its ledger rows record `reverted_by`. Nothing is deleted, so the audit trail survives the
incident review and a second revert of the same run is refused because the ledger says nothing of it still
stands. A revert that fails part-way marks only what it undid, and a second `revert` picks up the rest.

Everything else is as before: the same hazard gate (a `DROP TABLE` in a down file needs `--ack H002`), the
same scratch validation, the same lease, and the target must have nothing `queued` or `running` on it. The
plan the original was bound to stays `bound` until the next `CreateRun` with the same key retires it
(`superseded`); there is no plan state for "reverted" — the run carries that.

**Use it for the minutes after a bad apply, not as the production recovery mechanism.** The consensus
across every vendor surveyed, including those who sell rollback, is roll forward in production and keep
down files as a review artifact.

## Actors and provenance

Every token has a name; the name is the **actor** on the access log, on notifications, on `cp_runs.created_by` and on every `cp_audit` row. `CreateRun{source}` is free text stored on the run; the GitHub Action fills it with `<host>/<owner>/<repo>@<sha>[:<dir>]`. Every mutating RPC writes an audit row (`target.register`, `target.baseline`, `target.reconcile`, `run.create`, `run.revert`, `run.resume`, `run.park`, `run.confirm`, `drift.accept`, `plan.create`, `plan.supersede`) after it succeeds; reads are not audited (`Diff` creates and drops a scratch database on the scratch server but writes nothing in the store). `PlanRun{persist}` is the one `read`-scope call that writes: the plan and its `plan.create` row. The CLI exposes `persist` as `plan --save`, so the write is asked for rather than implied.
