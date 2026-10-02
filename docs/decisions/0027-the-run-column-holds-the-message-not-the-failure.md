# 0027 — `cp_runs.error` holds the message godwit publishes, not the failure

Amends [0024](0024-the-api-publishes-only-what-it-classified.md), which closed this on the RPC side and left
this half open deliberately.

## The question

0024 inverted `rpcErr`'s `default:` branch: an error godwit did not classify is not published. It could not
reach `cp_runs.error`, because `safe` wraps a *returned error* and that column is a *response field*.
`controlplane.failureDetail` writes the raw error into it, and eleven paths read it back out — the App's run
report on the pull request, the check-run summary, the Action's sticky comment and job log, Slack's message
and its push notification, the webhook payload, the UI's run page and queue cards, `Run.error` on `GetRun`,
`ListRuns`, `WatchRun` and `GetTargetStatus`, `godwit run get`, the server log, and the runbook's SQL. The
leak 0024 named was real on every one of them: a run that fails resolving credentials put the Vault secret
path, or the `projects/…/cryptoKeys/…` resource, into all of them at once.

0024 left it open because the column is also what an operator reads, so redacting at write time and
redacting at read time are not the same choice.

## The decision

**The column is a published field, so it holds the published message.** `failureDetail` runs the error
through the same classifier `safe` uses, and the full error is logged at the failure site under `run failed`
with the run id. Redacting per reader was the other candidate and it loses: it leaves a loaded column that
the next surface publishes by accident, which is the shape of this bug. A second column holding the raw
error is that same choice with a schema migration attached — it still has to decide, surface by surface, who
may read it.

**One classifier, in `internal/redact`.** 0024's list lived in `internal/surface/api`, where the control
plane cannot reach it, and a second copy of a security decision is a second thing to forget. `redact.Message`
is now the single answer to *what does godwit publish for this error*, and `api.safe` is a caller of it.

**Failing closed costs more here than it did on the RPC, so the sites that produce author-facing text say
so.** A refusal on the RPC is usually about registration; a run's failure is usually about the pull request's
own migrations, and *"the call failed"* for a bad SQL file would be a worse product than the leak. Two
markers carry that, applied at the site that knows:

- `redact.Public(err)` — the message is publishable whole. It marks what godwit derives from the submitted
  migrations and the plan: a directory it could not load, a directive it could not expand, an assertion that
  did not hold, a checkpoint gap, an INVALID index, an unknown rollout policy, a bound plan that no longer
  matches the target's schema.
- `redact.Wrap(err, format, args…)` — the *prefix* is publishable and the cause is classified underneath.
  The executor's `statement %d of %s (%s)` is the case this exists for. `report.Run` parses that prefix back
  out of the column to quote the failing statement's SQL in the comment, so dropping godwit's wrapping the
  way 0024 drops it on the RPC would have cost the author the most useful half of the report.

A `pgconn.PgError` still reaches the comment as the server's own answer, under the same ordering 0024 made
load-bearing: the dial cases are decided first, so a refused login does not arrive as one.

## What it costs

- **An operator debugging an unclassified run failure needs the server log.** `godwit run get` and
  `SELECT error FROM cp_runs` give the published message; the failure is one `kubectl logs` away under
  `run failed`, keyed by the run id. That is the trade 0024 already made for `godwit run resume`.
- **A failure class nobody has classified reads as `the call failed`.** For the run path that is a visible
  cost, not a theoretical one — it is paid by whoever adds a failure mode and does not mark it.

Nothing is lost for the failure operators actually read. A statement that failed on the target keeps the
server's own message and the statement it failed at, and the target's `godwit.runs.error` still holds the
engine's error unredacted, because that journal lives on the target database and a credential failure never
reaches it.
