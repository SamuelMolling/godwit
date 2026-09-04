# 0014 — The target's journal is authoritative; the ledger is the control plane's copy of it

Shipped in #96.

## The open question

godwit keeps the same fact in two places.

- **The target's journal** — `godwit.migrations` and `godwit.repeatables` in the database being migrated. The executor writes it in the same transaction as the DDL. It survives losing the service entirely.
- **The control plane's ledger** — `cp_run_applied` in the store. #68 put it there so a revert would undo what a run applied rather than what its directory carried, and #71 and #75 made it the one thing every other reader consults: the out-of-order guard (`Store.Applied`), the scratch replay (`Store.History`), the applied count on `godwit targets`, the "a directive is expanded once" rule.

Nothing reconciled them, and the ledger won every argument by default. Three consequences, each reproduced against PostgreSQL 17 before the fix:

1. **A migration applied outside this service stayed pending for ever.** An earlier tool, a hand `psql`, `godwit apply`, or a godwit instance whose store was lost — the target records it, `Store.Applied` does not. The order guard then refuses every later version below the newest ledger row, and `--allow-out-of-order` only gets you to the next failure: the scratch replay rebuilds only the service's own runs, so validation fails with `relation "…" does not exist`. This is what broke `docs/getting-started.md` §1 → §3 (#94, out-of-scope finding 1).
2. **`baseline` could not rescue it.** `engine.MarkApplied` refused whenever the target's journal held any applied version — which is exactly the state such a target is in. `baseline` adopted a database godwit had never journalled and nothing adopted one it had journalled elsewhere.
3. **A `migrate` over a target that already records everything succeeded and wrote nothing.** The executor skips a migration whose journal row matches the file's checksum, and the scheduler's recorder returned early on every skip. `godwit targets` said `0 applied` while `godwit target status` listed all of them, for ever.

The question is not "which store is nicer". It is: when the two disagree, which one is the fact?

## The decision

**The target's journal is authoritative about what a target has applied. `cp_run_applied` is the control plane's copy of it, plus the provenance only the control plane has.**

The asymmetry is real and it points one way:

- The journal is written **transactionally with the DDL**. If the statement committed, the row committed. Nothing in the store can make that claim; the ledger row is a second write, over the network, after the fact.
- The journal **survives losing the store**. Restore the store from a week-old backup and every target still knows exactly what it holds. Restore a target from a backup and the ledger's opinion of it is worthless.
- The ledger holds what the journal cannot: which run applied it, when, under whose token, with which frozen expansion, and undone by which revert. The journal has no room for any of that, and a reconcile cannot invent it.
- The ledger is queryable **without connecting to any target**. That is its whole reason to exist — `godwit targets`, the fleet view, the order guard and the replay all run on the store alone — and it is why "just read the target" is not an answer.

So the ledger keeps its readers, and gains an obligation: it must be able to be brought back into agreement with every target, and it must say so loudly when it cannot.

### What follows

**A ledger row records how it got there.** `cp_run_applied.adopted` marks a row the run *found* rather than *applied*. Adopted rows are standing rows for #75's predicate — `Applied`, `History`, the applied count and the order guard all see them, which is the point — and they are **out of scope for `revert`** (#68: a revert undoes what the run applied). `baseline` and `reconcile` write only adopted rows; a revert of either is refused by kind, as before.

**Three ways in, one shape.**

| The target has | Command | What it does |
|---|---|---|
| the schema, no `godwit` journal | `godwit target baseline <t> --dir … --version N` | writes the journal rows for `≤ N`, then the ledger rows |
| a `godwit` journal from elsewhere | `godwit target reconcile <t> --dir …` | writes **only** ledger rows, read from the target's journal |
| a journal this service partly knows | either | both are idempotent: a row already present on the side that owns it is left alone |

`baseline` stopped refusing a journalled target. It now refuses only when there is nothing left to adopt on *either* side (`ErrAlreadyMigrated`) or when the directory's checksum disagrees with the target's (`ErrHistoryConflict`). The old blanket refusal conflated "godwit already adopted this" with "this database had a life godwit was not part of", and the second is the adoption case. `--version` stays required and explicit, which is what actually keeps `baseline` from being a skip mechanism.

**A checksum mismatch is drift between the repository and the target, and it is loud.** `reconcile` refuses the whole call and names what it found, in three classes it will not decide on its own: recorded under different content, recorded on the target and absent from the directory, standing in the ledger and absent from the target. Only the fourth class — on the target, missing from the ledger — is repaired.

**Reconciling is explicit; detecting is automatic.** Before planning against a target with no stored plan to explain its history, the control plane compares the observation it just took with the ledger and refuses when the journal is ahead:

```
target records migrations the ledger does not: app records 20260101000000_orders, 20260101000001_total;
run `godwit target reconcile app --dir <migrations>` to adopt what it already has
```

*Rejected: reconciling silently on first contact.* The repair needs the migration bodies — the replay cannot rebuild a migration whose SQL the store does not hold — so it needs a directory, which the planning call may not be carrying the right version of. And a target that has moved under godwit is news, not housekeeping.

*Rejected: making the gate universal.* It fires where a run is being planned forward, and **not** on `revert`. A revert acts on the ledger's own rows and is well defined whatever else the target holds; gating the incident tool on a repair the operator may not be able to run (a divergence `reconcile` refuses) is the wrong trade.

*Rejected: firing the gate when a stored plan exists.* A stored plan is already an account of the target's history: a change it cannot attribute to a run is `PlanStale{history}`, with the diff and the hint. That refusal is better than this one, and a plan can only have been stored on a target that passed this gate.

**A run repairs what it walks past.** When the executor skips a migration because the target's journal already records it under the same checksum, the scheduler writes an adopted ledger row — unless a standing row already accounts for it, so a second `migrate` over the same directory adopts nothing and no run ever claims a migration another one holds. This is what makes the third defect impossible rather than merely fixed, and it covers what the gate cannot see (a run with no observation, a concurrent apply by another instance).

## What it costs

- **One extra query** on the planning path (`Store.Applied`) before admission, on targets with no stored plan.
- **`revert` is narrower on an adopting run.** A run that applied `b` and adopted `a` reverts only `b`. That is the correct reading of #68 and it is a behaviour change for anyone who expected a run's ledger to be its revert scope in full.
- **An adopted directive has no frozen expansion**, so a revert that reaches it is refused the way a baselined one already was. The replay records it without running it, as `recordUnexpanded` already does for baselines.
- **`reconcile` needs the directory that built the target.** A repository that has since deleted or rewritten those files cannot reconcile, and gets `absent from the directory` or `different content` — which is the honest report, not a workaround to be added.
