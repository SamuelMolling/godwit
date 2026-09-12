# Run migrations with the GitHub Action

The Action route keeps godwit inside your CI. A composite action at the root of the godwit repository builds
the CLI in the job and wraps it: it reads the event, decides whether the event carries a command, runs the
command, posts a sticky comment and sets the commit status that gates the merge. Everything it does you could
do with `godwit` in any runner — the Action is the glue written once.

Take this route when you do not want a webhook endpoint pointed at your service, when your review flow already
lives in workflows, or when you need `lint`, `verify` or `diff` on a pull request, which the App does not do.

## What must exist first

- A godwit service the runner can reach, with a target registered on it.
- **Two repository secrets**, because the two halves need different privilege:

| Secret | Scope | Used by |
|---|---|---|
| `GODWIT_TOKEN_READ` | `read` | `lint`, `plan`, `verify`, `diff` |
| `GODWIT_TOKEN_PIPELINE` | `pipeline` | `apply`, `confirm`, `revert`, `migrate` |

A `pipeline` token can create, revert and confirm runs on any target the service registers. It does not need
`operator` or `admin`, and giving it either is how a plan job ends up able to register targets.

- The workflow's own `permissions:` block. Nothing works from a default-read token alone:

```yaml
permissions:
  contents: read
  pull-requests: write   # the sticky comment
  statuses: write        # the godwit/plan and godwit/applied commit statuses on the pull request head
```

`github-token` defaults to `${{ github.token }}` and is what reads the pull request, its reviews and the
commander's repository permission, posts the comment and sets the statuses.

## Pin the action to a commit

```yaml
- uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
```

Not `@main`, and not a tag. A job running `command: apply` holds a `pipeline` token; a moving ref means
whoever obtains one push to that repository executes DDL on your production database at your next apply, with
no version to roll back to. Replace the sha with the godwit commit you reviewed, and pin
`actions/checkout` and the rest the same way — `@v4` is a tag its owner can move.

The action builds godwit from source with `actions/setup-go` and `CGO_ENABLED=1` (libpg_query needs `gcc`),
so the first godwit step in a job takes about a minute. `go-version` pins the toolchain, `1.26` by default.
The runner also needs `bash`, `jq` and `gh`, which GitHub-hosted Linux and macOS runners have; there is no
Windows path.

## The default mode: apply on the pull request

`mode: apply-on-pr` is the Atlantis model. The pull request plans, a `godwit apply` comment applies, and the
merge only verifies — so `main` never carries a migration the database does not have.

| Event | `command:` | What happens |
|---|---|---|
| `pull_request` | `lint`, `plan` | lint the new files; store the admitted plan on the service; sticky comment; `godwit/plan` status on the head commit |
| `issue_comment` — `godwit plan` | `plan` | re-plan the pull request head and store it, without pushing |
| `issue_comment` — `godwit apply`, or `pull_request_review` | `apply` | `migrate` from the pull request head, bound to the stored plan; `godwit/applied` status on the head commit |
| `issue_comment` — `godwit confirm` | `confirm` | the contract phase of the run the pull request left in `awaiting_contract` |
| `issue_comment` — `godwit revert` | `revert` | undoes what the run(s) of the pull request applied, newest first |
| `push` to the default branch | `verify` | `migrate --dry-run`: fails when a migration on `main` is not applied. Never applies |

`command: migrate` is refused in this mode (exit 2) unless `dry-run: "true"`.

The alternative, `mode: apply-on-merge`, keeps the older flow — plan on the pull request, `migrate` on push —
and refuses `apply`, `confirm` and `revert`. Take it only when nothing may touch the database before the
merge; the cost is that `main` can carry a migration the target refused, until someone fixes it forward.

## The workflow

Copy [`examples/github-actions/pr-plan-and-apply.yml`](../../examples/github-actions/pr-plan-and-apply.yml)
into `.github/workflows/` and replace the commented values. Its shape:

```yaml
on:
  pull_request:
    paths:
      - db/migrations/**
      - godwit.yaml
  issue_comment:
    types: [created]
  pull_request_review:
    types: [submitted]

concurrency: migrate-orders   # one apply per target at a time

jobs:
  check:
    if: github.event_name == 'pull_request'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: SamuelMolling/godwit@<sha>
        with:
          command: lint
          dir: db/migrations
          base: origin/main
      - id: plan
        uses: SamuelMolling/godwit@<sha>
        with:
          command: plan
          dir: db/migrations
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_READ }}
          target: orders
          rollout: expand-contract

  apply:
    if: >-
      (github.event_name == 'issue_comment' && github.event.issue.pull_request && contains(github.event.comment.body, 'godwit '))
      || github.event_name == 'pull_request_review'
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          ref: refs/pull/${{ github.event.issue.number || github.event.pull_request.number }}/head
      - uses: SamuelMolling/godwit@<sha>
        with:
          command: apply
          dir: db/migrations
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
          target: orders
          rollout: expand-contract
```

Three things in that file are load-bearing and easy to drop:

- **The apply job checks out `refs/pull/<n>/head`.** An `issue_comment` event runs the workflow from the
  default branch with the default branch checked out; without that `ref:`, the apply would run the migrations
  of `main`, not the ones under review.
- **`concurrency:` is per target, not per workflow.** Two pull requests applying to the same target at once is
  the thing to prevent.
- **`rollout:` must match between the plan and the apply.** They are separate jobs and neither reads the
  other's inputs; a mismatch is a stale plan at apply time.

`GODWIT_PUBLIC_URL` in the workflow `env:` is what makes the reports link the run and the plan to the service
UI. Without it they link neither.

The other examples are the same machinery in different shapes:
[`push-verify.yml`](../../examples/github-actions/push-verify.yml) (verify on merge, plus a `contract` job
gated behind a GitHub environment with required reviewers),
[`pr-dry-run.yml`](../../examples/github-actions/pr-dry-run.yml) (the plan without storing it),
[`apply-on-merge.yml`](../../examples/github-actions/apply-on-merge.yml), and
[`pr-prisma-diff.yml`](../../examples/github-actions/pr-prisma-diff.yml) (generate the migration from a Prisma
schema and commit it to the branch).

## The merge gate

Every applying command sets the **`godwit/applied` commit status** on the pull request head. Make it a
required status check on the base branch and GitHub will not merge until the apply has landed. Also require
*"Require branches to be up to date before merging"*, so a base move re-runs the pull request workflow and the
stored plan is the one computed on the exact set being applied.

The status is per commit: a push after the apply leaves the new head without one, and the pull request needs
`godwit apply` again. That is the point — the commit that merges is the commit that was applied.

An `expand-contract` apply that ends in `awaiting_contract` leaves the status `pending`, so half a migration
on the database cannot be merged as if it were whole. `godwit confirm` turns it green.

`godwit/plan` is a second, optional status carrying the plan's verdict and nothing else, because GitHub cuts a
description at 140 characters — `nothing to apply`, `2 to apply, 1 hazard to acknowledge`, `offline plan; no
target was consulted`. Its state says what is left to decide:

| State | When |
|---|---|
| `success` | the plan stands and what it would apply carries no hazard, or it was an offline plan, which decides nothing about a database |
| `pending` | the plan stands, and what it would apply carries a hazard acknowledged in `ack`. The plan did not fail — the hazard is information — but it is not a green light either |
| `failure` | the plan was refused and nothing was stored: an unacknowledged hazard, a version out of order, a validation failure, an unreachable service |

Requiring `godwit/plan` too is choosing that a migration carrying a hazard cannot merge on the plan alone, and
there is no click that turns it green: acknowledging the code in `ack` moves it from `failure` to `pending`,
and only rewriting the migration — take the recipe printed beside the statement — makes it `success`, because
an `--ack` on the apply comment never re-plans.

What `godwit/applied` says, and when:

| State | Description | Set by |
|---|---|---|
| `pending` | `applying <sha> from pull request #<n>` | `apply`, before it runs anything |
| `pending` | `confirming the contract phase of <sha> from pull request #<n>` | `confirm`, before it runs anything |
| `success` | `applied by run <id>; merge when the review is done` | an apply that reached `succeeded` |
| `pending` | `expand applied; comment godwit confirm to run the contract phase` | an apply that reached `awaiting_contract` — half a migration is on the database, so the pull request stays unmergeable |
| `success` | `contract applied by run <id>; merge when the review is done` | `godwit confirm` |
| `failure` | `plan stale or missing: re-plan on the pull request, then godwit apply again` | an apply the target moved underneath (exit 3) |
| `failure` | `apply failed (run <id>); see the pull request comment` | a run that stopped |
| `failure` | `contract phase failed (run <id>); see the pull request comment` | a confirm whose run stopped |
| `failure` | `reverted by run <id>; comment godwit apply to apply again` | a `godwit revert` that succeeded |
| `failure` | `revert failed (run <id>); see the pull request comment` | a revert that stopped |
| `failure` | the refusal's own text, cut at GitHub's 140 characters | a command godwit refused before running it |

`revert` sets no `pending` status on its way in — there is nothing to hold open — and it leaves the status
alone when it found no run of the pull request to revert. A real `command: migrate` never sets
`godwit/applied` at all: the merge gate belongs to the pull request commands, and the only `godwit/applied` a
`migrate` can produce is the `failure` of a refusal.

**Auto-merge is GitHub's, and it composes with this.** With *Allow auto-merge* on the repository, a reviewer
presses *Enable auto-merge* and GitHub merges the moment every required check is green and the reviews are in,
`godwit/applied` among them. godwit has no merge of its own and will not grow one: an `expand-contract` apply
*ends* at `awaiting_contract` on purpose, so an automerge that fired on a successful apply would merge before
the contract phase, and one that waited for `succeeded` would never fire on the rollout godwit exists to make
safe.

## Who may command an apply

The Action enforces the same three things the service would:

1. **Author association** — `allowed-associations`, defaulting to `OWNER,MEMBER,COLLABORATOR`. It *narrows*;
   it does not authorise. Only those three values are accepted, and anything else is a configuration error
   (exit 2) rather than a silently wider door.
2. **Repository permission** — the commander must hold `write` or `admin`.
3. **An approving review** — `require-approval`, `true` by default, for `apply` and `confirm`. Whether a push
   withdraws that approval is the repository's *"Dismiss stale pull request approvals"* setting, not godwit's.

A pull request from a fork is refused for every applying command, and `pull_request_target` is refused
outright (exit 2): that event runs in your repository with your secrets and a write token even for a fork's
pull request, so whoever opened it would apply their own migrations.

## Silence, and refusals

**A command has to be the whole comment**, backticks around the whole of it stripped — the same parser the App
uses ([the commands](github-app.md#the-commands)). Prose around it names nothing:

| Comment | |
|---|---|
| `godwit apply` | commands |
| `` `godwit apply` `` | commands — GitHub's copy and quote-reply wrap a command in backticks |
| `godwit apply <sha> --ack H001` | commands; the rule is about surrounding content, not arguments |
| `To deploy, comment:` ⏎ `godwit apply` | silence |
| a pasted log or a fenced block containing `godwit apply` | silence |

Silence is `skipped=true`, exit 0 and nothing posted. A **refusal** is the other thing: a commander without
write permission, an `author_association` outside `allowed-associations`, no approving review, a
`godwit apply <sha>` the head has moved past, a flag the command does not take. That posts a
`## godwit <command> refused` comment of its own under a `<!-- godwit:refused -->` marker — never the sticky
report, which is still the truth about the pull request — deletes the previous refusal so exactly one stands
and it is the newest thing on the page, and turns the status that command would have set `failure` with the
same text. `lint`, `verify` and `diff` set no status either way.

A refusal with nowhere to answer stays a warning in the job log: an event naming no pull request, a fork's
read-only `GITHUB_TOKEN`, or a caller's workflow without `pull-requests: write` or `statuses: write`.

Every other report is sticky under a marker of its own — `<!-- godwit:lint -->`, `<!-- godwit:plan -->`,
`<!-- godwit:diff -->`, `<!-- godwit:verify -->`, `<!-- godwit:dry-run -->`, and `<!-- godwit:migrate -->`
shared by `apply`, `confirm`, `revert` and `migrate` so that one comment tells the story of the run — so each
command keeps exactly one comment per pull request and edits it in place.

**Match the comment loosely in your own `if:`.** A job gated on `== 'godwit apply'` refuses
`godwit apply --ack H001` before the Action ever sees it, and never reaches the refusal machinery above. Use
`contains(github.event.comment.body, 'godwit ')` and leave the parsing and the refusals to the Action — which
is also what lets `/godwit apply`, the older form still accepted, through.

## Inputs worth knowing

The full list is in [`action.yml`](../../action.yml); these are the ones that decide behaviour.

| Input | Default | Notes |
|---|---|---|
| `command` | required | `lint`, `plan`, `apply`, `confirm`, `verify`, `revert`, `migrate`, `diff` |
| `mode` | `apply-on-pr` | or `apply-on-merge` |
| `apply-on` | `comment` | `comment`, `approve`, or `comment,approve` — whether an approving review alone applies |
| `dir` | `dir` from `godwit.yaml`, else `migrations` | |
| `base` | `origin/main` | lint only checks migrations added since this ref; empty checks every file |
| `ack` | — | comma-separated hazard codes. A `--ack` on the command comment **adds** its codes for that run |
| `server`, `token`, `target`, `rollout` | from `godwit.yaml` / `GODWIT_SERVER` | |
| `dry-run` | `false` | with `migrate`: the admitted plan, nothing stored and nothing queued |
| `comment` | `true` | post the report, or a refusal's reason, as one sticky pull request comment |
| `allow-data-loss`, `force` | `false` | revert only; a comment flag sets either for that run |

Outputs: `run-id`, `plan-id`, `plan-key`, `plan-verdict`, `plan-hazards`, `stale`, `phase`, `pending`,
`blocking`, `pr-number`, `head-sha`, `skipped`, `changed`, `files`, `summary-path`. `skipped` is `true` when
the event carried no command and nothing ran — check it before acting on any of the others.

**There is no `to-version` input, on purpose.** A [version target](../internals/runs.md#version-targets)
applies part of a branch and leaves the rest pending, which is exactly the state `verify` exists to fail on:
`godwit/applied` would turn green on a pull request only half applied, and the merge would then fail. Run
`godwit migrate --to` from a shell, and split the pull request when the split is meant to last.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success. For `apply` and `migrate` the run reached `succeeded` or `awaiting_contract`; for `verify`, every migration is applied |
| 1 | blocking lint findings, a refusal at admission, a run that ended `failed` or `needs_attention`, a fork trying to apply, a commander without permission, a connection error |
| 2 | the Action's own refusal: an unknown `command` or `mode`, a command the mode refuses, an invalid `allowed-associations` or `require-approval`, or an applying command on `pull_request_target` |
| 3 | the stored plan is stale or required — re-plan on the pull request. The `stale` output is `true` |

The last step of the action re-exits with the CLI's status *after* the summary, the comment and the commit
status have been written, so a failing lint still posts its report. An exit 1 or 2 from reading the event
happens before anything runs, and both still answer on the pull request rather than only in the job log.

## What the Action cannot do that the App can

Both routes run the same commands against the same service. The differences are about where the trust and the
waiting live.

- **Secrets sit in the repository.** Every repository using the Action holds a `pipeline` token, and that
  token reaches every target the service registers. The App holds its credentials in the service and binds
  each repository to named targets, so a repository cannot reach a target nobody bound it to.
- **A long run occupies a runner.** The Action streams the run and the job stays alive until it settles; a job
  timeout or a cancelled workflow loses the report even though the run continues. The App returns as soon as
  the run is queued and posts the outcome later, from the service.
- **The Action needs a workflow file per repository**, kept in step with the service as inputs change. The App
  needs a `godwit.yaml` and a binding.
- **No reaction, no check run.** The Action sets *commit statuses*; the App creates *check runs* with the same
  names, and puts a 👀 on the comment it read as a command.
- **A fork's pull request runs the workflow without your secrets**, so the plan step cannot reach the service
  at all. The App refuses a fork outright and says why on the pull request.

In the other direction, the App is narrower: it understands only `plan`, `apply`, `confirm` and `revert`, and
it subscribes only to pull request events. `lint`, `verify` and `diff`, anything on a push or a merge, and
`mode: apply-on-merge` exist only here.

`diff` in particular will never move: deriving a desired schema from an ORM means running the repository's own
toolchain — compiling a Go package, running `npx prisma`, running `manage.py` — and a central App doing that
would be executing arbitrary code from any installed repository in the process that holds every target's
credential. It belongs in a job that already has a checkout and needs only a `read` token. `lint`'s `E005`
is Action-only for the same reason.
