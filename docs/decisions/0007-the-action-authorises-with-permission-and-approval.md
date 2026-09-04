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
