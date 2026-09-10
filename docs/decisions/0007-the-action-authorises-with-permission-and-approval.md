# 0007 — The Action authorises with repository permission and a review anchored to the commit

Shipped in #81.

## What the guards actually were

#45 gave the Action a comment-driven apply and a set of guards around it: `author_association` in `allowed-associations`, the pull request must be open, the checked-out commit must equal the pull request head. #58 reused all three for `confirm`. A security review of the tree read them as authorisation and found that none of the three is:

1. **`pull_request_target` skipped every one of them.** The event branch fell straight through to the apply, and `docs/ci-cd.md` recommended that shape in as many words — *"the head from the event; no command or association check, the workflow's own `if` is the gate"*. `pull_request_target` runs in the base repository with its secrets and a write token, for pull requests opened from forks. A consumer who followed that sentence handed `secrets.GODWIT_TOKEN_PIPELINE` and `command: apply` against production to anyone who could open a pull request.
2. **The head check is not a race guard.** The comparison is `git rev-parse HEAD` against `.head.sha` from the API. The job checks out `refs/pull/N/head`, so both sides move together when the author pushes. `docs/ci-cd.md` described it as *"if the head moved between the command and the checkout, the step refuses"*, which is not what the code did. `.review.commit_id` — the one field in the payload that records which commit a reviewer approved — appeared nowhere in the repository, so an approval of commit A could apply commit B.
3. **`author_association` is a relationship, not a permission.** `MEMBER` is returned for every member of the organisation that owns the repository, including members with no access to *this* repository and members whose role on it is read-only. `allowed-associations` was also free text, so `CONTRIBUTOR` or `NONE` was silently accepted.

## The decision

The Action authorises with three checks and refuses `pull_request_target` for anything that writes.

**`pull_request_target` is refused for `apply`, `confirm`, `revert` and a non-dry-run `migrate` (exit 2), and refused outright for a fork's pull request whatever the command (exit 1).** Not narrowed, not gated on a flag: there is no shape of that event that is safe for a job holding a pipeline token, and the alternative is one line of workflow (`on: pull_request`). The same event with a same-repository head is still allowed for the read-only commands, because there it is exactly `pull_request`.

**`allowed-associations` narrows; `GET /repos/{owner}/{repo}/collaborators/{login}/permission` authorises.** `admin` or `write`, for the commander and for the approver. A failed lookup refuses. `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, `MANNEQUIN` and `NONE` are rejected as *configuration*, not as a runtime refusal, because anyone who opened a pull request carries one of them: listing one is not a policy choice, it is a mistake.

**`require-approval`, on by default, anchors the apply to a commit.** `apply` and `confirm` need an `APPROVED` review whose `commit_id` is the head being applied, from a login other than the pull request author, not superseded by a later `CHANGES_REQUESTED` from the same person. This is the fix for finding 2, and it is a better one than the head check could ever be: GitHub records the sha a reviewer approved, so a push after the approval invalidates it by construction and nothing has to be inferred. An approving review that triggers the apply is additionally checked against its own `commit_id`.

The command line itself gets two smaller guards on the same theme: `/godwit apply <sha>` pins the commit the commenter was reading, and the command must be a whole line **outside a fenced code block**, so a pasted CI log or a quoted transcript containing `/godwit apply` no longer fires an apply.

## What it costs

- **A second person.** `require-approval: true` means a solo maintainer cannot apply their own pull request without approving it from another account. The escape is `require-approval: "false"`, documented with what it gives up; `/godwit apply <sha>` is then the only anchor available.
- **One or two extra API calls** per commanded apply, and a `github-token` that may read the repository's collaborators. If it may not, the command is refused — fail closed, at the cost of a confusing failure for a consumer with an unusually narrow token. The refusal says which permission is missing.
- **Re-approving after a push.** That is the guarantee, not a side effect: an approval is about a commit.
- **Fork pull requests cannot run the apply part of `action-smoke`.** The smoke's real steps run under `pull_request`, which now refuses to apply for a fork's head. The event matrix still covers every guard from a fork's perspective with fake payloads.

## Rejected

- **Comparing `comment.created_at` against the head commit's `committer.date`** to catch a push that raced the comment. It was the obvious cheap fix for the comment path and it is forgeable: git lets the pusher set the committer date to anything. A guard an attacker can set the value of is not a guard. The approval anchor replaces it with a fact GitHub records itself.
- **Keeping `author_association` as the authorisation and documenting the caveat.** The caveat is the vulnerability. `MEMBER` is not access.
- **Refusing `pull_request` for `apply` outright.** It is the trigger a team applying on every push to a labelled pull request needs, and for a same-repository head it carries the same trust as pushing that branch. It is refused for a fork's head instead — GitHub already withholds the secrets there, so the only thing lost is a confusing authentication error in place of a stated reason.
- **A `plan`-style opt-in for the permission lookup.** Making the honest check optional reproduces the finding for everyone who does not read the option.

## Not fixed here

The Action's `Run godwit` step still puts `GH_TOKEN` and `GODWIT_TOKEN` in the environment of subprocesses that ORM schema sources spawn from the checked-out repository ([decision 0003](0003-orm-schema-sources.md) is what makes that execution deliberate). Those commands are `lint` and `diff`, both `read`-scope, both intended to run on `pull_request` where a fork gets no secret at all; the example that installs npm dependencies now does so with `--ignore-scripts`. Narrowing `cmd.Env` belongs with the schema-source code, not with the Action's guards.

## Amendment — the platform says whether a pull request is approved

The rule above — *an `APPROVED` review whose `commit_id` is the head being applied, from a login other than the pull request author* — is withdrawn for the Action. `require-approval` now asks GitHub whether the pull request is approved and takes the answer: `GET /pulls/{n}/reviews`, latest review per reviewer, one of them `APPROVED`. The `commit_id` comparison and the author comparison are gone.

**The commit anchor made godwit disagree with GitHub about the same pull request.** GitHub does not dismiss an approval when new commits arrive unless the repository asks it to, so the ordinary state after a review-then-push is: the pull request shows green, the merge button is live, and `godwit apply` refuses because the approvals sit on an earlier sha. Two sources of truth for one question, and the one nobody configured wins. That is a bug even when the stricter answer is the safer one — an operator who has read "approved" on the page and is told "not approved" by the tool cannot tell a real guard from a broken one, and the guard that fires when nothing is wrong is the guard people route around.

**The policy the anchor was implementing already exists, one level down, and belongs to the repository.** Branch protection's *Dismiss stale pull request approvals when new commits are pushed* makes GitHub rewrite the review's state to `DISMISSED`, which the check above already refuses. A team that wants approvals to expire on a push turns it on, gets it for the merge button as well as for godwit, and gets GitHub's more careful definition of it — a push that does not change the diff does not dismiss, where a sha comparison cannot tell the difference. A team that leaves it off has decided a push does not withdraw an approval. Neither decision is godwit's to take on their behalf, and taking it in one tool while the merge button takes the other one is the worst of the three positions.

**The author check was dead code.** GitHub refuses an approving review from the pull request's own author — *"Pull request authors cannot approve their own pull requests"* ([reviewing proposed changes](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/reviewing-changes-in-pull-requests/reviewing-proposed-changes-in-a-pull-request)) — so no payload the check could reject can exist. Re-checking it bought nothing and could only produce a refusal nobody could act on. It is worth being explicit that it never protected against what it looks like it protects against: a second account held by the same person approves fine, and always did.

**Atlantis, which is the reference this repository keeps reaching for, does exactly this and less.** `server/events/command_requirement_handler.go` reduces the `approved` requirement to `if !ctx.PullReqStatus.ApprovalStatus.IsApproved`, and `PullIsApproved` in `server/events/vcs/github/client.go` pages `GET /pulls/{n}/reviews` and returns true on the first review whose state is `APPROVED`. There is no sha anywhere on that path — `models.ApprovalStatus` has no field for one — and no author filter for GitHub (Bitbucket Cloud gets one, in its own client, because Bitbucket permits self-approval and GitHub does not). Its only staleness feature, `--discard-approval-on-plan`, is off by default and works by asking GitHub to dismiss the reviews, which is the same instinct: change the platform's answer, do not argue with it. godwit stays stricter than Atlantis on one point, and only because GitHub reports it: a reviewer's later `CHANGES_REQUESTED` supersedes their earlier `APPROVED`, where Atlantis's first-match loop would still see the approval.

### What this does not withdraw

Everything else in the record stands. `pull_request_target` is still refused for anything that writes. `allowed-associations` still narrows and the collaborator permission lookup still authorises, for the commander and for the approver — GitHub answers "is it approved", it does not answer "may this person apply migrations to production", and those are godwit's question. Nor does this touch the guards on the **command**: `godwit apply <sha>` still pins the commit the commenter was reading, and an `apply-on: approve` job still compares `review.commit_id` to the head. That last one reads like the anchor being removed and is not the same check. It asks whether a push raced the event that fired this job, which is a fact about the command's freshness, and it is what stops a review submitted seconds before a push from applying commits the reviewer never saw. Finding 2 of the original record — that the `git rev-parse HEAD` comparison is not a race guard — is still true, and these two are the answer to it.

### Cost

**A repository that never configures stale dismissal can apply a commit nobody reviewed.** Stated plainly, because it is the guarantee #81 bought and this gives back. Approve at A, push B, comment `godwit apply`, and B is applied. Three things make that the right trade anyway: it is exactly what merging that pull request would do in the same repository, so the apply is no weaker than the merge it precedes; the fix is one checkbox, named in `docs/ci-cd.md` and `docs/security.md` beside `require-approval`; and the alternative blocked three approved pull requests in one day, which is how this amendment came to be written.
