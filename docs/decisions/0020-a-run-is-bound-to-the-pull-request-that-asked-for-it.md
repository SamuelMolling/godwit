# 0020 — A run is bound to the pull request that asked for it, and whoever sees it settle reports it

Shipped in #135.

## The open question

[0019](0019-the-app-reads-the-migrations-at-the-head.md) made `plan` real and stopped there for a reason it named: `plan` is synchronous, and a run is not. `CreateRun` returns an id and the scheduler executes it — on a lease, possibly on another replica, possibly minutes later, possibly after the replica that accepted the comment is gone.

So the App can create a run easily enough. What it cannot do, with what 0019 built, is tell the pull request what the run did. The command's context — which repository, which pull request, which check — lived in the memory of one replica, and that is exactly the thing a run outlives.

"The run happened and nobody was told" is a different kind of wrong from "the plan never appeared". A plan that goes missing costs a second comment. An apply that goes missing leaves a schema changed, a reviewer waiting on a check that never concludes, and no way to tell that from a run that never started.

## The decisions

### The binding is a row, not a goroutine

`cp_github_runs` holds one row per run the App created: the repository and installation, the pull request and the head, the command, the target, the check it opened, the comment marker it answers under, and the report format the project asked for. Everything the report needs, in the store, because the process that knows it is the one most likely to be gone.

The row is keyed by run id and replaces itself: `godwit confirm` on a held run takes the binding over with its own check, so the contract phase concludes the check the confirm's own comment opened rather than the apply's, which is already closed.

### Whoever sees the run settle reports it, not whoever accepted the command

Every replica polls for bindings whose run has reached a state nobody has been told about. The claim is one `UPDATE … RETURNING` against a lease, so of N replicas exactly one takes a given run.

The alternative — the accepting replica watches its own run to the end — is simpler and wrong in the case that matters: it is precisely the replica most likely to be rolled, drained or killed while a long migration runs. Reporting is not the run's own work and should not share its fate.

**The claim is taken before GitHub is called and released only after.** A replica that dies mid-report leaves the claim to lapse and another replica repeats it. That is at-least-once rather than exactly-once, and it is the right way round here: the comment replaces itself under its marker and concluding a check twice is idempotent, so a repeat costs two API calls, while the other ordering costs a report.

### `awaiting_contract` is news, and so is what follows it

An expand-contract run settles twice: once holding its contract phase, once when `godwit confirm` releases it. So the row records the state it last reported, not a boolean. A state the pull request has already been told about is not news; a different one is, however many times the run comes back.

The cost is that this is per-state and not per-transition: a run that fails, is resumed and fails again reports once, because the state did not change. A person watching a pull request is not worse off for that — the report they already have says the run stopped — but it is a real limit, and `godwit run get` is where the attempt count lives.

`reverted` is deliberately not reportable. When a revert succeeds, the run it undid turns `reverted`; reporting that would post older news under the same marker and delete the revert's own report. The revert run reports itself, which is the one a reader wants.

### `revert` undoes the newest run of the pull request, not all of them

The Action collects every un-reverted run of the pull request in a revertable state and loops over them oldest first. On the second iteration that refuses: a run that is not the newest un-reverted one on its target needs `--force`, which the Action does not pass unless the comment did. So the loop only ever works when it has one element.

The App reverts the newest and says so. Identical in every case the Action handles, and it does not half-revert a pull request that applied twice.

The Action also renders a `--dry-run` into the comment before queueing. The App does not: the destructive case is already gated — `RevertRun` refuses a plan that would drop a table or column still holding rows unless `--allow-data-loss` — and the revert run's own report says what it did. One fewer round trip and one fewer place for the two reports to disagree.

### One comment per project

0019 posted a single comment carrying every project a pull request touched. That cannot survive a run: two projects mean two runs, each reporting itself later, each replacing the shared comment and deleting the other's report.

So a marker gains the target when there is more than one project, exactly as the check name does, and a repository with one project — the common case — keeps the Action's plain `<!-- godwit:migrate -->` and `<!-- godwit:plan -->`.

## What is still lost when a replica dies

Named rather than implied:

- **Between the check opening and the binding being written.** The window covers `CreateRun` itself. If the replica dies inside it the run may exist and have no binding: it applies, and the pull request is left with a check spinning and no report. The audit entry names the delivery, and `godwit runs --target <t>` shows the run; there is no automatic recovery. Closing it needs the binding written in the same transaction as the run, which means the App reaching into the store the API owns — not worth the coupling for a window measured in the time one RPC takes.
- **Between accepting a comment and starting on it.** Unchanged from 0019: the queue is in memory, and a command lost there ran nothing. Comment again.
- **A report interrupted part way** is not lost — that is what the lease is for.

## What was refused

- **Watching the run from the replica that accepted the command.** Ties the report to the process least likely to survive the run.
- **Driving the report from the scheduler's notifier.** The notifier is fire-and-forget in the process that executed the run and carries no pull request, so it would need this table anyway — and then the report would be lost whenever a notification was.
- **Keeping bindings forever.** They sweep with the deliveries, on the drift ticker, once reported.
