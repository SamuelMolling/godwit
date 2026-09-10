# 0016 — The GitHub App is bound to its targets by the server, not by the repository

Shipped in #126 — the receiver and its authorisation only. Fetching the migration files, the Check Runs and the report are not built; where this record describes them it says so.

## The open question

The Action needs a workflow, and the workflow is where the ugliness collects. A consuming repository copies about two hundred lines of YAML that connect the runner to godwit's network, run `lint` and `plan` on a pull request, and gate `godwit apply` behind an `authorize` job — sixty lines of shell re-deriving who is allowed to press the button. One such workflow in the wild re-implements, by hand, the exact guard [decision 0007](0007-the-action-authorises-with-permission-and-approval.md) rejected as forgeable: it compares the head commit's `committer.date` against `comment.created_at` and refuses when the commit is newer. git lets the pusher set that date. The Action it then calls does the honest check anyway, so nothing was lost — but the copy exists, someone wrote it, and the next repository will copy the copy.

That is the argument for the Atlantis model: a service that already holds the target credentials receives the event itself, and the consumer configures a webhook instead of a workflow. Every one of those sixty lines is a decision godwit already took; the only reason they live in the consumer is that the Action has no way to run without a runner.

What was never decided is what a service reachable from GitHub is allowed to believe. The Action's guards run *inside the repository being asked about*, with that repository's own token. A webhook handler runs outside all of them and gets, as its entire evidence, a JSON body and a signature.

## The decisions

### The binding is a property of the target, and `godwit.yaml` stops being believed

This is the one that had to be settled first, and the premise needs correcting before it can be. It is tempting to say a repository's `GODWIT_TOKEN` bounds what it may reach today. It does not. A token is `name:scope:secret` ([0011](0011-token-spec-is-three-fields.md)) and carries no target: any holder of any `pipeline` token can `CreateRun` against **any** target on the service. What a per-repository secret bought was blast radius and revocation — one repository, one rotation — not authority.

The App removes even that. There is no per-repository secret; the only evidence of who is asking is the `repository` field of a payload GitHub signed. Reading `godwit.yaml` from that repository and believing the target it names would let any repository in the installation name any target, including another team's production database.

**So the binding lives on the target, set by `RegisterTarget`, which is `admin`.** `github_repositories` is a target setting like `search_path` or `require_plan`: a comma-separated list, each entry `owner/repo` or `owner/repo:dir` because a monorepo has one `godwit.yaml` per migration directory. The authority to reach a target sits beside that target's credential, in the one RPC that already registers things and already lands in `cp_audit`, and `godwit targets` reads it back. A separate registry would be a second place to look and a place to drift from.

*Rejected: an allowlist in the App's own configuration.* It puts target policy in a deployment manifest instead of in the store, and makes "who may reach production" a thing you learn by reading YAML on a pod.

*Rejected: keying the binding to the installation.* An installation is a reachability fact. Installing the App org-wide says GitHub may talk to godwit about these repositories, not that these repositories may write to these databases.

**`godwit.yaml` stays where it is and keeps naming the target. It is now a request, not a grant.** The server will read it from the head sha, take the name, and look it up in the binding. That is the whole change: the file is read, and not believed.

**A repository bound to nothing gets nothing — not even a plan.** Installing the App changes no behaviour at all until an operator binds something. This is the fail-closed default, and it is the entire answer to "any repository in the installation could name another team's production database". It is also checked before any GitHub call, so a repository that gets nothing costs nothing.

**A repository asking for a target it is not bound to is refused, and the refusal does not say whether that target exists.** It names the repository and the name that was asked for, and says to ask an operator. A webhook caller holds no token and so has no `ListTargets`; a refusal that distinguished "not yours" from "no such target" would hand it one. `bindings.grant` is that check, and it is here rather than with the file fetch precisely so the file fetch cannot ship without it.

**A fork's pull request gets nothing.** The Action's fork story rests on GitHub withholding secrets from a fork's `pull_request` run. The server holds the credentials unconditionally, so the same event would give a fork a live plan — which means executing the fork's DDL on a scratch database ([0009](0009-scratch-databases-are-not-the-store.md)). Bounded in reach, unbounded in cost, and a capability the Action never had. A per-binding opt-in is the obvious next question and is not taken here.

One thing improves as a side effect. `source` is free text on a run today, and `revert`, `confirm` and the fleet view all match on it. Under the App the server writes it from the verified payload — `github.com/<owner>/<repo>@<sha>` — so the provenance the revert path trusts stops being caller-supplied.

### Authorisation moves to the server, and one thing genuinely weakens

The `authorize` job disappears. Its guarantees survive, run against the same GitHub API with an installation token:

1. **Write or admin on the repository.** `GET /repos/{owner}/{repo}/collaborators/{login}/permission`, `admin` or `write`, for the commander and for the approver, a failed lookup refusing rather than allowing. Unchanged.
2. **`author_association` narrows.** It arrives in the signed payload, exactly as it reached the Action. `OWNER,MEMBER,COLLABORATOR` stays the default and `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, `MANNEQUIN` and `NONE` stay configuration errors, refused at start-up.
3. **An approving review on the exact head, by someone other than the author.** `GET /repos/{owner}/{repo}/pulls/{n}/reviews`, same grouping by reviewer, same `commit_id` anchor, and the approver's own permission is checked too. Unchanged.
4. **A command older than the head is refused.** `godwit apply <sha>` and `review.commit_id` are compared against the head resolved live, at the moment of the command.

Two of the Action's checks have no server-side counterpart and lose nothing. `pull_request_target` is a workflow trigger, not a webhook event, so the refusal 0007 built has nothing to refuse. And the comparison of `git rev-parse HEAD` against the API head cannot be made without a checkout — 0007 already recorded that it is not a race guard, since both sides move together.

**What weakens is the token's reach.** `${{ github.token }}` could only ever read its own repository; an installation token can read every repository in the installation. The App therefore mints a token narrowed to exactly the repository the verified payload names (`POST /app/installations/{id}/access_tokens` with `repository_ids`), for that delivery only, and the authorising code is handed a value that can only ask about that one repository. That keeps the blast radius where the Action had it and makes the narrowing a thing that has to keep working: a repository-scoping bug is a cross-repository read.

**`author_association` moves to server configuration rather than onto the binding.** The association is checked before `godwit.yaml` has been read, so there is no target yet to read a per-target list from, and adding a second association list per binding would mean deciding which one wins when a repository is bound to two targets. `--github-allowed-associations` is one list for the deployment. It is a narrowing filter either way; the permission lookup is what authorises.

**A skipped command becomes silence.** Today a comment that is not a command ends a job with `skipped=true` and a green tick. The App posts nothing. That is a legibility loss, not a security one, and it is the right trade: a check run per stray comment is worse.

### The webhook is a second listener, and what a leaked secret actually buys

**`--github-webhook-addr` is a listener of its own, off by default, serving `/github/webhook` and nothing else.** The main listener carries every RPC, `/ui`, and an unauthenticated `/metrics` whose label values are target names. Publishing a path on it makes "only the webhook is exposed" a property of a proxy's path rules; a second listener makes it a property of the process. A tunnel or ingress then points at that port, and nothing else can be reached by mistake.

**Before the signature is verified the endpoint reads at most `--github-webhook-max-bytes`, computes the HMAC and compares it in constant time.** It parses no JSON, touches no store, allocates nothing per delivery beyond the body, and never logs the body. `X-Hub-Signature-256` only; the SHA-1 header is ignored and is never a fallback. A missing signature, an unknown prefix or a modified body is `401` with no body at all, so the only thing an unverified request learns is how many bytes it may send.

**The handler verifies, records the delivery id, hands the command on and returns `202`.** GitHub gives a delivery ten seconds; a plan takes longer than that on any real directory.

**Replay is answered by the delivery id, in the transaction that queues the work.** `X-GitHub-Delivery` is recorded with a unique key and swept by the ticker that already sweeps plans; a second delivery of the same id is `202` and does nothing. Writing it in the same transaction as the enqueue is what stops two replicas from both acting on one redelivery — an advisory check beforehand would not, and so there is no check beforehand: a replay pays for the authorisation calls before it is found to be a duplicate.

**A redelivery hours later is refused three ways, and the cheapest one is `--github-webhook-max-age` (default one hour).** A comment or review older than that is refused before any GitHub call. This looks like the guard 0007 rejected and is not: `comment.created_at` and `review.submitted_at` are GitHub's values inside a body GitHub signed, where `committer.date` is a value the pusher chose. The distinction is the signature. A `pull_request` payload carries no timestamp of its own, so nothing is compared there and the head sha is the whole anchor. Beneath the age check, an approval anchored to a `commit_id` no longer stands once the head moves, and a plan key that no longer matches is refused with `PlanStale`.

**Anything below the receiver failing is `500` and nothing enqueued.** A store that cannot be read, a GitHub that cannot be reached, a transaction that will not commit: the delivery is not recorded, so GitHub's redelivery is a fresh attempt rather than a duplicate. A refusal — unbound, unauthorised, stale, not a command — is `202` with the reason in the body, because redelivering it would not change the answer.

**What an attacker who reaches the endpoint can cause.** Without the secret: nothing. Every request is `401` before the JSON is parsed, so the endpoint is a constant-time HMAC and a counter.

With the secret — that is, if it leaks — they are GitHub, and the interesting part is how little that is worth. They control `repository`, `issue.number`, the comment body and `author_association`. They do not control what the API answers: the commander's permission, the pull request's head and its reviews are all read live. So a forged `godwit apply` still requires a real writer to hold write and a real approval to stand on the current head. The capability it grants is **pressing early a button somebody was already entitled to press**, in a bound repository, on an already-approved pull request. It is not arbitrary SQL and it is not an unbound target. The webhook secret is therefore held like the master key, and it is per installation so it can be rotated without touching anything else.

### The installation is `github:<owner>/<repo>`, and it can never exceed `pipeline`

An installation is not a token spec and does not become one. It resolves to a principal named `github:<owner>/<repo>` — the same shape as `ui:<name>` from [0004](0004-ui-is-a-scoped-client.md), and for the same reason — carrying `read` for the plan path and `pipeline` for the command path.

**`operator` and `admin` are unreachable from a webhook in code, not in configuration.** The map from command name to scope holds four entries and none of them is above `pipeline`, so the RPC that grants a repository access to a target can never be reached by that repository. That is what closes the loop on the binding: the fail-closed default cannot be turned off from the outside.

**The actor is the installation; the human is in the detail.** `cp_audit.actor` reads `github:acme/orders`, and the entry carries the commanding login, the delivery id, the head and the command. Making the commenter the actor would claim godwit authenticated a person. It did not: it authenticated a signature from GitHub that carried a claim about a person. `ListAudit` needs no change to tell the two paths apart — the prefix does it, exactly as `ui:` does.

### Where this stops

The receiver ends at one internal entry point that takes a verified, de-duplicated, authorised command and writes the audit entry saying what it would have run. Nothing reads the repository's files, nothing creates a run, and nothing is posted back to the pull request. `push` and `check_run` are not routed, because they are events that ask for work rather than events that carry a command. The half that fetches `<dir>` from the head sha by blob, checks the admission limits against the listing before downloading anything, calls `bindings.grant` with the name `godwit.yaml` asked for, and reports through Check Runs is a separate change, and is the one that makes the App do anything.

## What it costs

- **Installing the App does nothing.** Every repository starts bound to no target and gets no plan, no lint, no comment. Someone will read that as broken before they read it as fail-closed, so the refusal has to be one a human can act on; today it is in the delivery response and the service log, and it will be a comment when the App can post one.
- **`RegisterTarget` becomes the security boundary for two things at once**: the credential and the repository allowlist. It was already `admin`; it now has a second reason to be. It also upserts the whole config, so a `target add` that omits `--github-repo` unbinds the target.
- **The webhook secret is a target credential by transitivity**, since holding it lets someone apply an already-approved pull request in a bound repository.
- **One installation token can read every repository in the installation.** Narrowing it per delivery is what keeps that from mattering.
- **`godwit.yaml` will gain a reader that is not the CLI.** Its parse will run on untrusted content from an arbitrary head sha, in the server process. It has always parsed untrusted files, but not on this path.
- **This record contradicts one sentence in [security](../security.md).** "Treat the `pipeline` token like the target's own credential" is right; what the page did not say, and now does, is that a `pipeline` token reaches *every* target. The binding above is the first thing in godwit that scopes access by target.

## Rejected or deferred

| Thing | Verdict | Reason |
|---|---|---|
| A `requested_action` button that applies | refused | An apply with no comment, no sha and no line in the pull request timeline. The command stays something a person wrote down. |
| The App setting commit statuses as well as Check Runs | refused | Two surfaces for one fact, which is 0004's argument in different clothes. A repository uses one integration or the other. |
| A poller reconciling `GET /app/hook/deliveries` | refused for now | A second source of truth about what a pull request asked for, turning a silent failure into an inconsistent one. A lost delivery produces nothing; recovery is a push or another comment. Revisit only with evidence that deliveries are actually being lost. |
| A shared, publicly listed App | refused | One App per deployment, its private key held like the master key. A shared App would mean one webhook secret across unrelated fleets. |
| Deprecating the Action | refused | It is the zero-infrastructure path, and the only path for anyone who cannot expose godwit to GitHub at all. `diff` with an ORM schema source and `lint`'s `E005` need the repository's own toolchain in a checkout ([0003](0003-orm-schema-sources.md)), which the server does not have. |
| Rate limiting per repository and per installation | deferred | The binding is checked before any GitHub call and before any store write, so the thing worth limiting — the API budget — is not yet spendable by an unbound repository. It belongs with the half that fetches files. |
| A fork's pull request planned when the binding says so | deferred | Per-binding policy of any kind is deferred; the fail-closed default is what this change is for. |
| Per-branch or per-approver bindings, and `apply_on: approve` | deferred | `repository → targets` is the binding that closes the hole. Whether production also wants "only from `main`" or "approved by one of these three" is policy nobody has taken. |
| GitHub App *user* access tokens, acting as the commenter | deferred | It would make `cp_audit.actor` a person honestly rather than by attribution, and it needs each commenter to authorise the App. Worth wanting; not worth blocking on. |
| GitHub Enterprise Server, and non-GitHub forges | not decided | `--github-api-url` exists because the tests need it. The event names, the signature header and the Check Run vocabulary are all GitHub's, and nothing here commits to generalising them. |

## Not verified

Asserted from documentation or from reasoning, and not tested against a live installation. Each one changes something above if it turns out otherwise.

- **Which installation permission grants `GET /repos/{owner}/{repo}/collaborators/{login}/permission`.** The manifest below asks for `Metadata: read` and assumes `Administration: read` is the fallback. If only the latter works, the App asks for a permission most reviewers will balk at.
- **Whether `repository_ids` on `POST /app/installations/{id}/access_tokens` narrows the way the documentation says.** The whole "the blast radius is where the Action had it" claim rests on it, and it was exercised only against a fake.
- **Whether a Check Run named `godwit/applied` satisfies a required status check configured under that name**, and whether `conclusion: neutral` blocks one. Both belong to the half that is not built; they are named here because they decide whether a repository can move from the Action to the App without touching branch protection.
- **Whether GitHub automatically retries a failed delivery for an App**, and how many times. "A lost delivery produces nothing" is written as if it never retries, which is the pessimistic reading. It is also why a `500` is returned rather than a `202` for anything that might succeed on a second attempt.
- **The installation-token rate limit for a large installation.** One command costs three to five calls today. The blob cache the next change needs is designed to keep the answer from mattering; that reasoning has not been checked against a real budget.
- **How long GitHub may redeliver an id for.** Delivery ids are kept for four times `--github-webhook-max-age`, which is longer than a delivery could survive the age check anyway, but the redelivery window itself is not documented anywhere we could find.
