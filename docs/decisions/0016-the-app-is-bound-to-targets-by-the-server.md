# 0016 — The GitHub App is bound to its targets by the server, not by the repository

Design only. Nothing here is built, and no pull request implements it yet; the decisions are taken, which is what makes this a record rather than a page under `open/`.

## The open question

The Action needs a workflow, and the workflow is where the ugliness collects. A consuming repository copies about two hundred lines of YAML that connect the runner to the network godwit is on, run `lint` and `plan` on a pull request, and gate `/godwit apply` behind an `authorize` job — sixty lines of shell re-deriving who is allowed to press the button. One such workflow in the wild re-implements, by hand, the exact guard [decision 0007](0007-the-action-authorises-with-permission-and-approval.md) rejected as forgeable: it compares the head commit's `committer.date` against `comment.created_at` and refuses when the commit is newer. git lets the pusher set that date. The Action it then calls does the honest check anyway, so nothing was lost — but the copy exists, someone wrote it, and the next repository will copy the copy.

That is the argument for the Atlantis model: a service that already holds the target credentials receives the pull request event itself, and the consumer configures a webhook instead of a workflow. Every one of those sixty lines is a decision godwit already took; the only reason they live in the consumer is that the Action has no way to run without a runner.

What was never decided is what a service reachable from GitHub is allowed to believe. The Action's guards run *inside the repository being asked about*, with that repository's own token. A webhook handler runs outside all of them and gets, as its entire evidence, a JSON body and a signature.

## The decisions

### The App receives four events and ignores the rest

| Event | Actions handled | What it does |
|---|---|---|
| `pull_request` | `opened`, `synchronize`, `reopened`, `ready_for_review` | `lint` and `plan` at the head sha |
| `issue_comment` | `created` only | `/godwit apply`, `/godwit confirm`, `/godwit revert` |
| `pull_request_review` | `submitted` | the same commands in a review body; an approval when the binding says `apply_on: approve` |
| `push` | default branch | `verify` |
| `check_run` | `rerequested` on `godwit/plan` | re-plan |

`edited` on a comment is ignored, as it is in the Action: a command is a thing someone posted, not a thing a comment currently says. `installation` and `installation_repositories` are recorded and run nothing — see the binding below, which is why an installation grants no authority to record. Everything else is answered `202` and dropped with a counter; the endpoint does not enumerate what it does not handle.

`rerequested` on `godwit/applied` is refused. A re-run button that applies is an apply with no comment, no `<sha>` and no line in the pull request timeline.

### Authorisation moves to the server, and one of the four checks gets stronger

The `authorize` job disappears. Its four guarantees survive, run against the same GitHub API with an installation token:

1. **Write or admin on the repository.** `GET /repos/{owner}/{repo}/collaborators/{login}/permission`, `admin` or `write`, for the commander and for the approver, a failed lookup refusing rather than allowing. Unchanged.
2. **`author_association` narrows.** It arrives in the signed payload, exactly as it reached the Action. `OWNER,MEMBER,COLLABORATOR` stays the default and `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, `MANNEQUIN` and `NONE` stay configuration errors. What changes is where the list lives: on the binding, not in a workflow the repository writes about itself.
3. **An approving review on the exact head, by someone other than the author.** `GET /repos/{owner}/{repo}/pulls/{n}/reviews`, same grouping, same `commit_id` anchor. Unchanged.
4. **A command older than the head is refused.** `/godwit apply <sha>` and `review.commit_id` are checked as they are today.

One check does not survive, and should not: the Action compares `git rev-parse HEAD` with the API head. 0007 already recorded that this is not a race guard, since both sides move together. The App has no checkout, so it cannot make the comparison and does not need to — it resolves the head once and pins the file read, the approval and the run to that sha.

Two things genuinely weaken, and neither is the authorisation:

- **The token is no longer scoped by the runner.** `${{ github.token }}` could only ever read its own repository. An installation token can read every repository in the installation, so the routing decision — which repository this delivery is about — becomes security-relevant where it was previously free. The App therefore mints a token narrowed to exactly the repository the verified payload names (`POST /app/installations/{id}/access_tokens` with `repository_ids`), for that delivery only.
- **A skipped command becomes silence.** Today a comment that is not a command ends a job with `skipped=true` and a green tick. The App posts nothing. That is a legibility loss, not a security one, and it is the right trade: a check run per stray comment is worse.

### The binding is a property of the target, and `godwit.yaml` stops being believed

This is the hard one, and the premise needs correcting first. It is tempting to say the repository's `GODWIT_TOKEN` bounds what it may reach today. It does not. A token is `name:scope:secret` ([0011](0011-token-spec-is-three-fields.md)) and carries no target. Any holder of any `pipeline` token can `CreateRun` against **any** target on the service. The hole the App is accused of opening is already open; what a per-repository secret bought was blast radius and revocation — one repository, one rotation — not authority.

The App removes even that. There is no per-repository secret; the only evidence of who is asking is the `repository` field of a payload GitHub signed.

**So the binding moves onto the target, set by `RegisterTarget`, which is `admin`.** A target carries the list of repositories permitted to run against it, keyed `owner/repo` and optionally `owner/repo:dir` because a monorepo has one `godwit.yaml` per migration directory. The authority to reach a target lives beside that target's credential, in the one RPC that already registers things and already lands in `cp_audit`. A separate registry would be a second place to look and a place to drift from.

*Rejected: an allowlist in the App's own configuration.* It puts the target policy in a deployment manifest instead of in the store, where `ListTargets`, `ListAudit` and the UI can already see it, and it makes "who may reach production" a thing you learn by reading YAML on a pod.

*Rejected: keying the binding to the installation.* An installation is a reachability fact. Someone installing the App org-wide is saying GitHub may talk to godwit about these repositories, not that these repositories may write to these databases.

**`godwit.yaml` stays exactly where it is and keeps naming the target. It is now a request, not a grant.** The server reads it from the head sha, takes the name, and looks it up in the binding. That is the whole change: the file is read, and not believed.

**A repository bound to nothing gets nothing — not even a plan.** Installing the App changes no behaviour at all until an operator binds something. This is the fail-closed default and it is the entire answer to "any repository in the installation could name another team's production database".

**A repository asking for a target it is not bound to is refused, and the refusal does not say whether the target exists.** It names the repository and the name it asked for, and says to ask an operator to bind it. A webhook caller holds no token and so has no `ListTargets`; leaking the target inventory through a refusal message would hand it one.

One thing improves as a side effect. `source` is free text on a run today, and `revert`, `confirm` and the fleet view all match on it. Under the App the server writes it from the verified payload — `github.com/<owner>/<repo>@<sha>[:<dir>]` — so the provenance the revert path trusts stops being caller-supplied.

**A fork's pull request is planned only when the binding says so, and never applied.** The Action's fork story rests on GitHub withholding secrets from a fork's `pull_request` run, so a fork silently plans offline. The server holds the credentials unconditionally, so the same event would give a fork a live plan — which means executing the fork's DDL on the scratch database ([0009](0009-scratch-databases-are-not-the-store.md)). 0009 makes that bounded rather than dangerous, but it is unbounded in *cost*, and it is a capability the Action never had. Default off, per binding.

### The files come from the head sha, by blob, and are checked before they are fetched

`GET /repos/{owner}/{repo}/contents/{dir}?ref=<head sha>` lists the directory; the listing form returns `sha`, `size` and `type` and no content, so each file is then `GET /repos/{owner}/{repo}/git/blobs/{sha}`. Never a branch name, never the merge ref, never a recursive walk — `dir` is one directory, which is what the loader has always read.

Ordering does not come from the API. It comes from the version in the filename, as it does for every other caller; the listing's order is unspecified and is not relied on anywhere.

**The admission limits are checked against the listing, before a single blob is downloaded.** `--max-files`, `--max-file-bytes` and `--max-migrations` already exist and already bound what one call may make a replica allocate; the listing carries `size` per entry, so a directory godwit would refuse is refused for the price of one API call.

**Blobs are cached by their sha.** They are content-addressed and immutable, so a `synchronize` storm on a forty-file directory re-fetches the one file that changed. This is the whole answer to the rate limit: without it, a plan costs roughly one call per migration file plus five, and a busy repository would spend an installation's hourly budget on pushes.

**Nothing new is invented for a head that moves between plan and apply.** Two mechanisms already cover it and a third would be a second definition of staleness. The approval anchors to a `commit_id`, so a push invalidates it; the plan key is a pure function of the files and the target's history ([concepts: plans](../concepts.md#plans)), so a set that changed finds no plan and is refused with `PlanStale` or `PlanRequired`. The App re-resolves the head at the moment of the command and fetches at that sha. That is all it does.

### The installation is `github:<owner>/<repo>`, and it can never exceed `pipeline`

An installation is not a token spec and does not become one. It resolves to a principal named `github:<owner>/<repo>` — the same shape as `ui:<name>` from [0004](0004-ui-is-a-scoped-client.md), and for the same reason — carrying `read` for the plan path and `pipeline` for the command path.

**`operator` and `admin` are unreachable from a webhook, in code, not in configuration.** The App never resumes, parks, baselines or registers, so nothing is lost; and it means the RPC that grants a repository access to a target can never be reached by that repository. That is what closes the loop on the binding: the fail-closed default cannot be turned off from the outside.

**The actor is the installation; the human is in the detail.** `cp_audit.actor` reads `github:acme/orders`, and the entry carries the commanding login and the delivery id. Making the commenter the actor would claim godwit authenticated a person. It did not: it authenticated a signature from GitHub that carried a claim about a person. `ListAudit` needs no change to tell the two paths apart — the prefix does it, exactly as `ui:` does.

### One extra listener serving one path, and what a leaked secret actually buys

**The webhook is a second listener (`--webhook-addr`, off by default), not a path on the existing one.** The main listener carries every RPC, `/ui`, and an unauthenticated `/metrics` whose label values are target names. Publishing a path on it makes "only the webhook is exposed" a property of a proxy's path rules; a second listener makes it a property of the process. A tunnel or ingress then points at that port and nothing else can be reached by mistake.

**Before the signature is verified the endpoint reads at most `--max-request-bytes`, computes the HMAC, compares it in constant time, and increments a counter.** It parses no JSON, touches no store, allocates nothing per delivery beyond the body, and never logs the body. `X-Hub-Signature-256` only; the SHA-1 header is ignored and is never a fallback. A missing signature, an unknown prefix or a truncated body is `401` with no body.

**The handler verifies, records the delivery id, enqueues and returns `202`.** GitHub gives a delivery ten seconds; a plan takes longer than that on any real directory. Doing the work in the handler would make every slow target look like a failed delivery.

**Replay is answered by the delivery id, in the transaction that queues the work.** `X-GitHub-Delivery` is recorded with a unique key and swept by the ticker that already sweeps plans; a second delivery of the same id is `202` and does nothing. Writing it in the same transaction as the enqueue is what stops two replicas from both acting on one redelivery — an advisory table checked beforehand would not.

**A delivery whose own event timestamp is older than `--webhook-max-age` (default one hour) is refused.** This looks like the guard 0007 rejected and is not: `comment.created_at` and `review.submitted_at` are GitHub's values inside a body GitHub signed, where `committer.date` is a value the pusher chose. The distinction is the signature.

Rate limiting is per repository and per installation, applied before any GitHub call and before any store write. An unbound repository gets a much smaller bucket, since the only thing it can ever produce is a refusal.

**What an attacker who reaches the endpoint can cause.** Without the secret: nothing. Every request is `401` before the JSON is parsed, so the endpoint is a constant-time HMAC and a counter.

With the secret — that is, if it leaks — they are GitHub, and the interesting part is how little that is worth. They control `repository`, `issue.number`, the comment body and `author_association`. They do not control what the API answers: the commander's permission, the pull request's head and its reviews are all read live. So a forged `/godwit apply` still requires a real writer to hold write and a real approval to stand on the current head. The capability it grants is **pressing early a button somebody was already entitled to press**, in a bound repository, on an already-approved pull request. It is not arbitrary SQL and it is not an unbound target. It also grants forged `synchronize` events, which is scratch-database DDL for any head sha in a bound repository, bounded by 0009 in reach and by the rate limiter in cost. The webhook secret is therefore held like the master key, and it is per installation.

### The Action and the App share a package, not a shape

Both stay. The Action is the path for someone who cannot host godwit; the App is for someone who can. The risk is not that one becomes redundant, it is that two implementations of "who may apply" drift.

Most of it is already single-sourced and people worry about the wrong half. Hazard gating, admission, the plan key, the report markdown and the `PlanStale` refusal are all produced by the service or the CLI; a workflow cannot drift from them because it does not implement them. **The part that will drift is `scripts/action-context.sh`, three hundred and thirty-seven lines of bash, which is exactly the part that moves.** The authorisation rules and the status/verdict mapping become one Go package, and `action-context.sh` shrinks to invoking a `godwit` sub-command with the event payload on stdin. The Action already builds the binary in the job, so this costs no new dependency and no new download.

The plan-key, verdict and hazard HTML markers stay, and stay Action-only. They exist because a bash step can only read stdout; the App reads the `PlanRun` response.

**The App is deliberately not a superset of the Action.** `diff` with an ORM schema source, and `lint`'s `E005` check, need the repository's own toolchain in a checkout ([0003](0003-orm-schema-sources.md)) and the server has neither. The App lints the files it fetched and reports the schema check as not run, in the existing `W002` shape. A team that generates migrations from Prisma keeps a workflow.

### Check Runs, and what 0004 actually refused

0004 refused **two contexts for one question** — splitting `godwit/applied` into an expand check and a contract check, when `pending` already says "did what it was asked, not finished". #119 then added `godwit/plan`, a second context for a *different* question: what the pull request would do to the database, against whether it was done. Those are consistent, and the rule that reconciles them governs this: a new surface earns its name by carrying a question no existing name carries.

**The App creates two Check Runs, named `godwit/plan` and `godwit/applied`, with the same three states and the same descriptions as the statuses the Action sets. No third check.** What Check Runs add is not a new question, it is a better rendering of the two that exist: the report goes in the check's own summary instead of only in a comment, and hazards become annotations on the line of the migration file that raises them, which is where the author is looking.

The state mapping is where 0004's reasoning has to be carried over exactly. `success` → `conclusion: success`. `failure` → `conclusion: failure`. **`pending` → `status: in_progress` with no conclusion**, not `conclusion: neutral`. The whole point of `pending` on a held apply is that a required check stays unsatisfied without claiming an error; a conclusion that branch protection treats as passed would let a half-applied expand phase merge, which is the exact thing that status exists to prevent. The cost is ugly and named: a run awaiting `/godwit confirm` for two days shows a spinner for two days. A spinner that is honest beats a tick that is not.

**The App does not also set commit statuses.** Two surfaces for one fact is the drift 0004 argued against in different clothes. A repository uses one integration or the other.

### Delivery is at-least-once, and one direction of failure is silent

- **A lost delivery produces nothing**: no plan, no check, no comment. Stated plainly because there is no cure on offer — the App has no poller and will not grow one, since a poller is a second source of truth about what a pull request asked for. Recovery is a push, a re-request of the `godwit/plan` check, or a new comment.
- **A duplicate delivery is a no-op**, by delivery id, and beneath that by the plan bind: a plan already bound to a run is refused (`plan <id> is bound to run <r>`).
- **A redelivery hours later is refused three times over** — by `--webhook-max-age`, by the approval anchor if the head moved, and by `--plan-ttl`. The age check exists so the refusal is cheap and legible, not because the other two are insufficient.
- **GitHub unreachable during authorisation fails closed**, exactly as the Action does when its permission call fails.
- **GitHub unreachable after admission changes nothing about the database.** The run executes; the check run and the comment retry with backoff and are then given up on with a log line. The ledger is the record ([0014](0014-the-target-journal-is-authoritative.md)), and `godwit runs`, `godwit run get` and the UI all still answer.
- **What is not idempotent**: the sticky comment, if the marker read fails, can post a second one — already true of the Action, not a regression. And an implicit run, for a file set with no stored plan, is only serialised rather than deduplicated: godwit serialises runs per target and re-attaches to the run the same files, target and rollout created, so a race past the delivery-id row attaches instead of queueing a second run.

## Consequences to live with

- **Installing the App does nothing.** Every repository starts bound to no target and gets no plan, no lint, no comment. Someone will read that as broken before they read it as fail-closed, so the first delivery from an unbound repository must produce a refusal a human can act on rather than a dropped event.
- **`RegisterTarget` becomes the security boundary for two things at once**: the credential and the repository allowlist. It was already `admin`; it now has a second reason to be.
- **The webhook secret is a target credential by transitivity**, since holding it lets someone apply an approved pull request in a bound repository. It belongs wherever the master key belongs, and it is per installation so it can be rotated without touching anything else.
- **One installation token can read every repository in the installation.** Narrowing it per delivery keeps the blast radius where the Action had it, and makes the narrowing a thing that has to keep working — a repository-scoping bug is a cross-repository read.
- **`godwit.yaml` gains a reader that is not the CLI.** Its parse now runs on untrusted content from an arbitrary head sha, in the server process. It has always parsed untrusted files, but not on this path.
- **This record contradicts one sentence in `docs/security.md`.** "Treat the `pipeline` token like the target's own credential" is right; what the page does not say, and should, is that a `pipeline` token reaches *every* target, not the ones its holder was thinking of. The binding above is the first thing in godwit that scopes access by target, and the page will need to say which mechanism scopes what. Nothing is edited here — a record is not the place to fix a page.

## Refused or deferred

| Thing | Verdict | Reason |
|---|---|---|
| A `requested_action` button that applies | refused | An apply with no comment, no sha and no line in the timeline. The command stays something a person wrote down. |
| The App setting commit statuses as well as Check Runs | refused | Two surfaces for one fact, which is 0004's argument in different clothes. |
| A poller reconciling `GET /app/hook/deliveries` | refused for now | It is a second source of truth about what a pull request asked for, and it turns a silent failure into an inconsistent one. Revisit only with evidence that deliveries are actually being lost. |
| A shared, publicly listed App | refused | One App per deployment, registered by the operator, its private key held like the master key. A shared App would mean one webhook secret across unrelated fleets. |
| `contents: write` — the App committing a generated migration | deferred | The `diff`-on-pull-request flow needs it, and it is a different trust decision: a service that can write to the repository can write the migration it is about to apply. |
| Per-branch or per-approver bindings | deferred | `repository → targets` is the binding that closes the hole. Whether production also wants "only from `main`" or "approved by one of these three" is policy the owner has not taken, and every part of it is a separate question. |
| Gating a run on a predecessor environment | deferred | Already deferred by [0013](0013-the-fleet-view-is-keyed-by-content.md) and unchanged by this. |
| GitHub App *user* access tokens, acting as the commenter | deferred | It would make `cp_audit.actor` a person honestly rather than by attribution, and it needs each commenter to authorise the App. Worth wanting; not worth blocking on. |
| GitHub Enterprise Server, and non-GitHub forges | not decided | The shape generalises. Nothing here commits to it, and the event names, the signature header and the Check Run vocabulary are all GitHub's. |
| Deprecating the Action | refused | It is the zero-infrastructure path, and it is the only path for anyone who cannot expose godwit to GitHub at all. |

## Not verified

Everything below is asserted from documentation or from reasoning, and none of it was tested against a live installation. Each one changes a decision above if it turns out otherwise.

- **Which installation permission grants `GET /repos/{owner}/{repo}/collaborators/{login}/permission`.** The design assumes `Metadata: read` suffices and that `Administration: read` is the fallback. If only the latter works, the App asks for a permission most reviewers will balk at, and that is worth knowing before the manifest is written.
- **Whether a Check Run named `godwit/applied` satisfies a required status check configured under that name.** The intent is that a repository moves from the Action to the App without touching branch protection. If checks and statuses are not interchangeable there, the App's names need a suffix and the migration note has to say so.
- **Whether `conclusion: neutral` blocks a required check.** If it does, it is the better fit than `in_progress` and the spinner goes away. The choice above is the safe one under uncertainty, not the known-correct one.
- **The installation-token rate limit for a large installation.** The blob cache is designed to make the answer not matter; that reasoning has not been checked against a real budget.
- **Whether GitHub automatically retries a failed delivery for an App**, and how many times. "A lost delivery produces nothing" is written as if it never retries, which is the pessimistic reading.
- **The size limit on a Check Run's `output.text`.** The plan report for a large directory is not small, and a truncated report in the check is worse than a link to a comment holding the whole one.
- **Whether the deployment's tunnel actually gives per-path scoping.** The module in use expresses an ingress rule with a hostname, a path and a service, and path-scoped Access applications exist elsewhere in that configuration, so the shape is expressible. Whether the tunnel's path matching and an Access bypass on exactly one path behave as intended has not been tested — which is a second reason the webhook is its own listener: if the path scoping does not hold, the port still does.
