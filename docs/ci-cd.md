# CI/CD

Two integrations ship in the repository: a composite GitHub Action (`action.yml` at the root) and ArgoCD hook Jobs (`deploy/argocd/`). Both are thin wrappers over the CLI; anything they do you can do with `godwit` in any runner. Complete workflows and `Application` manifests to copy are in [examples/](../examples/README.md).

The CLI outside GitHub comes from the image `ghcr.io/samuelmolling/godwit` (`main`, `sha-<short commit>`; built by `.github/workflows/publish.yml` on every merge) or, once a `v*` tag exists, from the GitHub release and `brew install SamuelMolling/tap/godwit` (`.github/workflows/release.yml`, GoReleaser).

## Exit codes

| Code | CLI | Meaning |
|---|---|---|
| 0 | all | success; for `migrate`, `apply` and `revert`: the run reached `succeeded` or `awaiting_contract`; for `confirm`: the contract phase ran; for `verify`: every migration is applied |
| 1 | all | error: blocking lint findings, refusal at admission, run `failed` or `needs_attention`, a migration `verify` found pending, no run of the pull request awaiting its contract phase, a refused command ([who may command an apply](#who-may-command-an-apply)), a fork trying to apply, connection or usage error |
| 2 | Action only | unknown `command` or `mode`, a command the mode refuses, an `apply-on` that enables nothing, an `allowed-associations` or `require-approval` that is not a valid value, or an applying command on `pull_request_target` |
| 3 | `migrate`, `apply` | plan stale or required: re-plan on the pull request |

The Action's last step re-exits with the CLI status after the summary, the comment and the status are written, so a failing lint still posts its report.

## GitHub Action

```yaml
- uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
  with:
    command: lint | plan | apply | confirm | verify | revert | migrate | diff
```

It builds godwit from the checked-out action ref with `actions/setup-go` (`CGO_ENABLED=1`, needs `gcc`), so the first run in a job takes a minute; `go-version` pins the toolchain. The runner also needs `jq` and `gh` (present on GitHub-hosted runners).

**Pin the sha, not a branch.** The examples and the snippets on this page use a commit because a job running `command: apply` holds a `pipeline` token: `@main` means whoever obtains one push to this repository executes DDL on every consumer's production database at their next apply, with no version to roll back to. Replace the sha with the godwit commit you reviewed, and pin every other action in those workflows the same way — `actions/checkout@v4` is a tag its owner can move.

### Two modes

The default, `mode: apply-on-pr`, is the Atlantis model: the pull request plans, `/godwit apply` on the pull request applies, and the merge only verifies. `main` never carries a migration the database does not have, because the merge is gated on the `godwit/applied` commit status that only a successful apply sets.

| Event | Command | What happens |
|---|---|---|
| `pull_request` | `lint`, `plan` | lint the new files; store the admitted plan on the service; sticky comments |
| `issue_comment` `/godwit apply`, or `pull_request_review` | `apply` | `migrate` from the pull request head, bound to the stored plan; status `godwit/applied` on the head commit |
| `issue_comment` `/godwit confirm` | `confirm` | the contract phase of the run the pull request left in `awaiting_contract`; the status goes from `pending` to `success` |
| merge (`push`) | `verify` | `migrate --dry-run`: fails when a migration on `main` is not applied; never applies |
| `issue_comment` `/godwit revert` | `revert` | undoes what the run(s) of the pull request applied, newest first; the dry-run plan goes in the comment before anything is queued; status back to failure |

In this mode `command: migrate` is refused (exit 2) unless `dry-run: "true"`. `mode: apply-on-merge` keeps the previous flow: `plan` on the pull request, `migrate` on push; `apply`, `confirm` and `revert` are refused there (confirm the contract phase from the deploy pipeline with `godwit run confirm --latest --allow-none --target <t>`, [below](#expand--contract-in-a-pipeline)). Use it when nothing may touch the database before the merge (for example when the PreSync hook in [ArgoCD](#argocd) is the only thing allowed to apply).

### Action inputs and outputs

| Input | Default | Used by |
|---|---|---|
| `command` | required | `lint`, `plan`, `apply`, `confirm`, `verify`, `revert`, `migrate`, `diff` |
| `mode` | `apply-on-pr` | `apply-on-pr` (apply, confirm and revert from the pull request, verify on push, migrate refused) or `apply-on-merge` (migrate on push, apply, confirm and revert refused) |
| `apply-on` | `comment` | apply: `comment` (a `/godwit apply` comment or review body), `approve` (an approved review), or `comment,approve`. `confirm` is always commanded by a `/godwit confirm` comment or review body |
| `allowed-associations` | `OWNER,MEMBER,COLLABORATOR` | apply, confirm, revert: `author_association` values whose comment or review counts. It **narrows**, it does not authorise ([below](#who-may-command-an-apply)); only `OWNER`, `MEMBER` and `COLLABORATOR` are accepted, anything else is a configuration error (exit 2) |
| `require-approval` | `true` | apply, confirm: an approving review, by someone other than the pull request author who holds write or admin permission, must stand on the exact commit being applied. `false` removes the anchor and the review requirement |
| `dir` | `dir` from `godwit.yaml`, else `migrations` | all but revert; diff writes the generated pair there |
| `base` | `origin/main` | lint: only migrations added since the ref are linted, files modified since it are `E003`; empty checks every file. The ref is fetched depth-1 when missing |
| `ack` | — | lint, plan, apply, verify, migrate: comma-separated hazard codes; revert: the codes found in the down files (`H002` for `DROP TABLE`, `H009` for `DROP INDEX`, ...) |
| `allow-data-loss` | `false` | revert: run a plan that drops a table or column still holding rows. godwit refuses it by default and names the objects and their row counts in the comment |
| `force` | `false` | revert: undo a run that is not the newest un-reverted one on its target |
| `server` | `server` from `godwit.yaml` or `GODWIT_SERVER` | plan, apply, verify, revert, migrate, diff |
| `token` | — | plan, verify and diff (`read`), apply, confirm, revert and migrate (`pipeline`); exported as `GODWIT_TOKEN`, never passed on the command line |
| `target` | `target` from `godwit.yaml` | plan, apply, verify, migrate, diff; confirm and revert (optional, narrows the run search). With a target, `plan` runs on the service and stores the plan; without one it parses the files offline |
| `rollout` | `godwit.yaml`, else `direct` | plan, apply, migrate: part of the plan key, so all must agree |
| `dry-run` | `false` | migrate: `PlanRun` without persisting, markdown report, no run (`command: plan` is the persisting variant); allowed in both modes. diff: report the migration without writing the files |
| `schema` | — | diff: file holding the whole desired database as DDL |
| `prisma` | — | diff: `schema.prisma` rendered to DDL by the Prisma CLI, which the checkout must provide (`npm ci`); exclusive with `schema` |
| `prisma-bin` | `npx prisma`, or `GODWIT_PRISMA_BIN` | diff: command line that runs the Prisma CLI, e.g. `node_modules/.bin/prisma --config prisma.config.ts` |
| `name` | — | diff: migration name, snake_case; the files are `<timestamp>_<name>.{up,down}.sql` in `dir` (required unless `dry-run`) |
| `source` | `<host>/<owner>/<repo>@<pull request head sha, else sha>[:<dir>]` | plan, apply, verify, migrate: provenance stored on the plan (`cp_plans.source`) or run (`cp_runs.source`); revert finds the runs of a pull request by it |
| `comment` | `true` | post the lint, plan, diff, dry-run, apply or revert report as one sticky pull request comment |
| `comment-on-push` | `true` | on `push`, post the migrate outcome, or a failed verify, on the pull request(s) the commit merged |
| `github-token` | `${{ github.token }}` | reads the pull request, its reviews and the commander's repository permission, posts the comments (`pull-requests: write`) and sets the status (`statuses: write`) |
| `go-version` | `1.26` | build |

| Output | Meaning |
|---|---|
| `run-id` | id of the run created by `apply` or `migrate`, resumed by `confirm`, or of the last revert run (empty for dry runs, verify and refusals) |
| `plan-id` | id of the plan stored by `plan` on the service, or bound by `apply`, `confirm` or `migrate` (empty offline and for implicit runs) |
| `plan-key` | key of the plan stored by `plan` (same files, target and rollout give the same key on every push) |
| `stale` | `true` when `apply` or `migrate` exited 3: the stored plan is stale or the target requires one |
| `phase` | `awaiting-contract` when the run applied its expand phase and holds the contract one, `contract` when the contract phase ran, empty for a single-phase run |
| `pending` | number of migrations `verify` found not applied |
| `blocking` | number of blocking lint findings |
| `changed` | `true` when `diff` found the target and the desired schema differ |
| `files` | space-separated paths of the up and down files `diff` wrote (empty on `dry-run` or `no changes`) |
| `pr-number` | pull request `apply` or `revert` acted on (or the pull request of a `pull_request` event) |
| `head-sha` | head commit of that pull request: the one checked out, applied and carrying the status |
| `skipped` | `true` when the event carried no command for `apply`, `confirm` or `revert` (a comment that is not the command, a review that does not apply): nothing ran, the step succeeds |
| `summary-path` | file with the markdown report, also appended to the job summary |

Comments are sticky: each is found and replaced by a hidden marker, `<!-- godwit:lint -->`, `<!-- godwit:plan -->`, `<!-- godwit:diff -->`, `<!-- godwit:dry-run -->`, `<!-- godwit:verify -->` or `<!-- godwit:migrate -->` (shared by `apply`, `confirm`, `revert` and `migrate`: one comment tells the story of the run), so each command keeps one comment per pull request. On `pull_request` events lint, plan, diff and dry run post their report; `apply`, `confirm` and `revert` post on the pull request they were commanded from; a real `migrate` never posts there. On `push` events only `migrate` and a failed `verify` post, and they look the pull request(s) up from the commit (`GET /repos/{owner}/{repo}/commits/{sha}/pulls`): the outcome of the merge lands on the pull request that shipped it, with the run id and state, the SQL error, the `PlanStale` report or the list of migrations still pending. A push that merged no pull request posts nothing; a failed comment is a workflow warning, not a failure.

### Pull request: lint and plan

```yaml
name: migrations
on: pull_request
permissions:
  contents: read
  pull-requests: write
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with: { command: lint }
      - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with:
          command: plan
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_READ }}
          target: orders
```

`lint` needs no service, unless a `schema_source` block is declared and `server`/`target` are given: it then also checks that the committed migrations still express the ORM schema (`E005`, [below](#pull-request-a-migration-from-the-prisma-schema)). `plan` with a target needs a token with the `read` scope only: it asks the service for the admitted plan against the real target (hazards, out-of-order check, scratch validation, which versions are already applied, which statements would be deferred to contract), stores it with an observation of the target, and posts a `## godwit plan <id>` comment carrying the key, the observation and a `### Changes outside migrations` diff when the target already differs from what the last run left. That stored plan is what the apply binds to ([concepts: plans](concepts.md#plans)); a refusal becomes a `## godwit plan` comment with the reason and a failed step. Without a target the step parses the files offline as before. `dry-run: "true"` on `migrate` gives the same report without storing anything. On `pull_request` events the `source` records the pull request head, not the merge commit GitHub checks out.

Acknowledging a hazard is a code change, visible in the workflow file or in the migration author's `--ack` list, never a click:

```yaml
      - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with:
          command: lint
          ack: H003
```

### Pull request: a migration from the Prisma schema

A team that edits `prisma/schema.prisma` and never writes SQL gets the migration written for it, on the pull request, before lint and plan see it ([pr-prisma-diff.yml](../examples/github-actions/pr-prisma-diff.yml) is the whole workflow):

```yaml
      - uses: actions/setup-node@v4
        with: { node-version: 22, cache: npm }
      - run: npm ci
      - id: diff
        uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with:
          command: diff
          dir: db/migrations
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_READ }}
          target: orders
          prisma: prisma/schema.prisma
          name: prisma
```

The step runs `prisma migrate diff --from-empty --script` on the schema with the CLI the project pins, sends the resulting DDL as the desired state and writes the pair into `dir`; `changed` and `files` say whether it wrote anything and what. The Action never commits: the workflow does, with a bot identity, so `contents: write` and a checkout of `github.event.pull_request.head.ref` (not the merge commit) are what let the pair land on the branch. Because a push made with `GITHUB_TOKEN` triggers no workflow, the same job continues with `lint` and `plan` after the commit, otherwise the stored plan would not cover the files just added. The generated pair is regenerated on every push: delete the pair the previous run added (`git rm` the `*_<name>.{up,down}.sql` files added since the base) before the diff step, so the pull request always carries exactly one.

The desired schema must describe the whole database ([getting started](getting-started.md#3b-write-the-next-migration-from-a-schema)); anything the target has that the Prisma schema does not is a `DROP` in the generated `up`, including `_prisma_migrations` if the project ever ran `prisma migrate` against that database.

The commit is what keeps the two in step; nothing stops a later pull request from editing `schema.prisma` without regenerating, or hand-editing the generated `.sql`. The `lint` step catches that when it is given `server` and `target`, and the directory declares its source in `godwit.yaml`:

```yaml
      - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with:
          command: lint
          dir: db/migrations
          base: origin/main
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_READ }}
          target: orders
```

godwit replays the committed migrations on a scratch database (the target's recorded history first, then the files) and diffs the result against the rendered ORM schema. Empty means they match; anything left is `E005` with the residual SQL under it, and the step fails ([concepts](concepts.md#keeping-the-generated-sql-and-the-orm-schema-together)). `schema_source.lint: false` in `godwit.yaml` makes it a warning instead. Drop `server` and the check reports `W002` and lint runs offline as before — the check lives in `lint` rather than in the workflow precisely so the local command and the CI one agree.

### Pull request: apply

```yaml
on:
  issue_comment: { types: [created] }
  pull_request_review: { types: [submitted] }
permissions:
  contents: read
  pull-requests: write
  statuses: write
jobs:
  apply:
    if: github.event_name == 'pull_request_review' || (github.event.issue.pull_request && startsWith(github.event.comment.body, '/godwit '))
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          ref: refs/pull/${{ github.event.issue.number || github.event.pull_request.number }}/head
      - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with:
          command: apply
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
          target: orders
```

Once the review is done, a collaborator comments `/godwit apply` (the whole comment, or one line of it; also accepted as the body of a review). With `apply-on: comment,approve` an approving review applies as well. The step:

1. Reads the event. The command is a whole line of the comment, outside any fenced code block, so a pasted log or a quoted reply carrying `/godwit apply` does not fire; the line may name the commit the commenter was reading (`/godwit apply <sha>`), and then that sha must be the head. A comment that is not the command, a review that does not apply, an edited comment or a comment on an issue end the step with `skipped=true` and exit 0.
2. Authorises the commander — `allowed-associations`, then the real permission lookup and the approval anchor described [below](#who-may-command-an-apply). Every refusal exits 1 and says which check failed.
3. Reads the pull request through the API: it must be open, and the checked-out commit must be its head, so the job has to check out `refs/pull/<n>/head` (the default checkout of `issue_comment` is the default branch). This catches a mis-configured checkout. It does **not** catch a push that raced the job: both sides of that comparison move together, which is what the approval anchor in step 2 is for.
4. Sets the commit status `godwit/applied` to `pending` on the head, then runs `godwit migrate` from the pull request files with the same `dir`, `target` and `rollout` as the plan step, so it binds to the stored plan ([concepts: plans](concepts.md#plans)) and refuses when the target moved since (exit 3, `stale=true`).
5. Posts the `## godwit apply` report on the pull request and sets the status: `success` ("applied by run …; merge when the review is done"), `pending` when the run stopped at `awaiting_contract` ("expand applied; comment /godwit confirm to run the contract phase", output `phase=awaiting-contract`), or `failure` with the reason (stale plan → re-plan then command again; SQL error → the run's error is in the comment). The status links to the comment.

The status is per commit, so a push after the apply leaves the new head without one. Branch protection on the base branch should require the `godwit/applied` status and **"Require branches to be up to date before merging"**: the first makes the apply the gate of the merge, the second makes GitHub re-run the pull request workflow (re-plan) when the base moves, so the plan stored last is the one computed on the exact set the pull request applies. The `source` recorded on the run is `github.com/<owner>/<repo>@<head sha>[:<dir>]`, which `godwit runs`, `godwit audit` and `revert` use.

The `apply` step also accepts `pull_request` events, for teams that apply on every push to a labelled pull request. There is no comment to authorise there, so the guards that remain are the approval anchor (`require-approval`, on by default: an approving review must stand on the head being applied) and a refusal when the pull request comes from a fork — GitHub withholds the secrets from a fork's `pull_request` run anyway, and a stated refusal beats a confusing authentication failure. The workflow's own `if` is the rest of the gate.

**`pull_request_target` is refused for `apply`, `confirm`, `revert` and a real `migrate` (exit 2), and refused outright for a pull request opened from a fork (exit 1).** That event runs in the base repository with its secrets and a write token *for code the fork controls*: a workflow that reached `apply` from it would hand `secrets.GODWIT_TOKEN_PIPELINE` and production DDL to anyone who can open a pull request. Earlier versions of this page recommended exactly that shape. They were wrong; use `pull_request`, or command the apply from a comment.

### Who may command an apply

`author_association` is a **relationship, not a permission**: `MEMBER` is returned for every member of the organisation that owns the repository, including members with no access to this repository at all. `allowed-associations` therefore only narrows; three checks authorise, in this order, and each refusal names itself:

1. **`allowed-associations`** — the commenter's `author_association` must be in the list. `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, `MANNEQUIN` and `NONE` are rejected as *configuration* (exit 2): anyone who opened a pull request carries one of them, so listing one would authorise the world.
2. **Repository permission** — `GET /repos/{owner}/{repo}/collaborators/{login}/permission` must return `admin` or `write`, for the commander and, when an approval is required, for the approver too. The `github-token` must be able to read that; if the call fails, the command is refused rather than allowed.
3. **An approving review on the exact commit** (`require-approval`, default `true`) — the pull request must carry an `APPROVED` review whose `commit_id` is the head being applied, from a login other than the pull request author, whose latest review is not a later `CHANGES_REQUESTED`. This is the anchor: GitHub records the sha a reviewer approved, so a push after the approval invalidates it and the apply is refused. It is what makes `/godwit apply` mean "apply what was reviewed" rather than "apply whatever is on the branch now"; a comment payload carries no sha of its own, which is why the anchor is a review and not the comment.

An approving review that itself triggers the apply (`apply-on: approve`) is additionally checked against `review.commit_id`, so approving a stale page in the browser cannot apply a newer head.

Three costs, stated plainly. `require-approval: true` needs a **second person** — a solo maintainer applying their own pull request must approve it from another account or set `require-approval: "false"`. The permission lookup is **one extra API call** per command (two when an approval is checked), and it needs a `github-token` that may read collaborators. And an approval is anchored to a commit, so a push during review means re-approving before applying — which is the guarantee, not a side effect.

### Pull request: confirm the contract phase

An `expand-contract` apply whose plan holds statements back ends in `awaiting_contract`: the expand phase is on the database, the contract phase is not, and the migration is only half done. The step exits 0, so `godwit/applied` must **not** say `success` there — it stays `pending` with "expand applied; comment /godwit confirm to run the contract phase". Branch protection keeps the pull request unmergeable until the contract phase runs, which is the point: `main` never carries a migration the database has only half of.

```yaml
      - name: Run the contract phase held by the apply
        if: contains(github.event.comment.body, '/godwit confirm')
        uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with:
          command: confirm
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_PIPELINE }}   # pipeline scope, same as apply
          target: orders
```

Once the application version that reads both shapes is deployed, a collaborator comments `/godwit confirm`. The step reads the event under the same rules as `apply` ([who may command an apply](#who-may-command-an-apply), open pull request, the checked-out commit must be the head), sets `godwit/applied` to `pending` ("confirming the contract phase of …"), then:

1. Lists the commits of the pull request and the runs of the target, and takes the newest run whose `source` is `<repo>@<one of those commits>` and whose state is `awaiting_contract` — the same provenance match `revert` uses, so it can only release what this pull request applied.
2. Calls `ConfirmRollout` on it and streams the run to its end. It is the **same run id**, resumed at the statement it stopped at with `phase = contract`: no second plan, no second bind, nothing re-executed ([concepts: rollout policies](concepts.md#rollout-policies)).
3. Posts the `## godwit confirm` report and sets `godwit/applied` to `success` ("contract applied by run …; merge when the review is done"), or `failure` with the run's error. Output `phase` is `contract`.

No `dir` and no `rollout`: the contract phase runs the statements the plan already froze, so the files are never re-read. When no run of the pull request is awaiting its contract phase the step fails (exit 1) with "no run of pull request #N is awaiting its contract phase" and **leaves the status alone** — a green apply is not turned red by a command that did nothing.

If the head moved while the run was awaiting its contract phase, the head guard refuses the command (the status on the new head was never set by that apply, and a new apply is refused while the target has a run awaiting contract). Confirm it from the CLI instead — `godwit run confirm --latest --target orders` — and re-plan on the new head.

### Pull request: revert

When a pull request is closed without merging after it applied, `/godwit revert` runs `command: revert`: it lists the commits of the pull request, finds every run whose `source` is `<repo>@<one of those commits>` and is `succeeded`, `awaiting_contract`, `failed` or `needs_attention`, and calls `godwit revert <id>` for each, newest first, stopping at the first failure. The down files pass the hazard gate, so `ack` must carry their codes (`H002`, `H009`, ...). A merged pull request is refused: its migrations belong to the base branch now, revert them from a new pull request. The report is posted on the pull request and the status goes back to `failure` ("reverted by run …"), so the pull request cannot be merged until it applies again. The original run ends `reverted`, the plan it was bound to is retired when the next apply binds ([concepts: revert](concepts.md#revert)); a re-plan stores a fresh plan and `/godwit apply` binds to that one.

### Merge: verify

```yaml
name: migrations on main
on:
  push:
    branches: [main]
    paths: [db/migrations/**]
jobs:
  verify:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with:
          command: verify
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_READ }}
          target: orders
```

`verify` runs `godwit migrate --dry-run --json` with a `read` token: the same admission as a plan (hazards, `ack`, scratch validation) plus the list of versions the target has. Exit 0 with `pending=0` when every migration on `main` is applied; exit 1 with the pending list, posted on the merged pull request, otherwise. It never applies: a migration that reached `main` unapplied is fixed by applying it from a new pull request (or by `mode: apply-on-merge`, below).

**There is no `to-version` Action input, on purpose.** A [version target](concepts.md#version-targets) applies part of a branch and leaves the rest pending, which is exactly the state `verify` exists to fail on and the `godwit/applied` status exists to keep out of `main`: the status would turn green on a pull request whose migrations are only half applied, and the merge would then fail `verify`. Run `godwit migrate --to` from a shell or from a workflow of your own that calls the binary, and split the pull request when the split is meant to be permanent.

### Merge: apply-on-merge

```yaml
name: migrate
on:
  push:
    branches: [main]
    paths: [db/migrations/**]
concurrency: migrate-orders
jobs:
  migrate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - id: migrate
        uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with:
          command: migrate
          mode: apply-on-merge
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
          rollout: expand-contract
      - run: echo "run ${{ steps.migrate.outputs.run-id }}"
```

The step streams `godwit migrate --json` events, writes `## godwit migrate` with the final state, the bound plan id (or `implicit plan`) and the error when there is one, to the job summary and to the merged pull request, and exits with the run. No plan id is passed: the service computes the key from the files, the target and the rollout, finds the plan the pull request stored and refuses when the target moved since (exit 3, output `stale=true`, the report on the pull request says what moved and how to fix it). Both steps must send the same `dir`, `target` and `rollout`, otherwise the keys differ and the run is implicit (or refused when the target has `require_plan`). `concurrency` is optional: the service serialises runs per target itself, and re-running the job re-attaches to the run the first attempt created (same files, target and rollout) instead of queueing another: a running one is followed, a failed one is resumed when the target has not moved. Transient failures (lock timeouts, deadlocks, lost connections) retry inside the service with backoff, so the job only fails on a genuine SQL error (`sql:` in the message) or once the service gave up. The `source` recorded on the run is `github.com/<owner>/<repo>@<sha>`, which `godwit runs` and `godwit audit` show.

### Expand → contract in a pipeline

With `rollout: expand-contract` the apply (or the merge step in `apply-on-merge`) exits 0 while the run sits in `awaiting_contract`. On a pull request the contract phase is released by [`/godwit confirm`](#pull-request-confirm-the-contract-phase); everywhere else, confirm from the deploy pipeline once the new application version is out:

```yaml
      - run: godwit run confirm --latest --allow-none --target orders
        env:
          GODWIT_SERVER: https://godwit.internal
          GODWIT_TOKEN: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
```

`--allow-none` makes the step a no-op when nothing awaits (a deploy that shipped no migration). Without it the CLI fails with `target orders: no run awaiting contract`. The step waits: `run confirm` streams the contract phase it released and exits with the run, so the deploy fails when the swap fails instead of going green on a phase that was only queued. Add `--no-wait` when the pipeline watches the run somewhere else.

## ArgoCD

`deploy/argocd/` has two hook Jobs for an application whose migrations are shipped in a ConfigMap alongside the manifests.

**PreSync** (`presync-job.yaml`): `godwit migrate --target=orders --dir=/migrations --rollout=expand-contract` with the `orders-migrations` ConfigMap mounted at `/migrations`, `GODWIT_SERVER=http://godwit.godwit.svc:8474`, `GODWIT_TOKEN` from Secret `orders-godwit` key `token` (scope `pipeline`). `backoffLimit: 0` (the service retries, the Job must not), `activeDeadlineSeconds: 3600`, `hook-delete-policy: BeforeHookCreation`. Exit 0 on `succeeded` or `awaiting_contract` lets the sync proceed with the schema expanded; exit 1 stops the sync, the run's error is in the Job log and in `godwit run get`; exit 3 means the plan stored on the pull request is stale (or required and missing): the sync stops before any pod changes and the Job log carries the diff. The PreSync run binds to the plan the pull request stored: the ConfigMap holds the same `.up.sql`/`.down.sql` bodies as the repository, and the key is computed from those bodies, the target and the rollout, so it matches as long as the pull request planned with `target: orders` and `rollout: expand-contract`.

**PostSync** (`postsync-confirm.yaml`): `godwit run confirm --latest --allow-none --target=orders`, `activeDeadlineSeconds: 600`. After the new pods are healthy, the contract phase runs, and the Job waits for it: `run confirm` streams the phase and exits with the run, so a contract phase that fails fails the hook instead of leaving a green Job over a broken swap. If the sync fails between the hooks, the run stays `awaiting_contract`; the old pods keep working against the expanded schema, and the next successful sync's PostSync confirms it, or an operator reverts it.

Replace `orders`, the Secret name and, for a reproducible hook, the image tag (`ghcr.io/samuelmolling/godwit:main` in the examples; `sha-<short commit>` is immutable); the ConfigMap must contain both `.up.sql` and `.down.sql` files (the CLI loads the directory and rejects a version with a missing side). Runs created by these Jobs carry `created_by = <token name>` and an empty `source` unless `--source` is added to the args.

## Expand → contract, end to end

1. Author the change as two migrations: an additive one (`CREATE`, `ADD COLUMN`, `CREATE INDEX CONCURRENTLY`) and a later destructive one (`DROP`, `RENAME`: H002/H003/H008); or as one `-- godwit:` directive, which godwit splits down the middle itself. The split is by statement: everything up to the first contract statement runs in expand, that statement and everything after it wait for contract ([concepts: rollout policies](concepts.md#rollout-policies)).
2. Pull request: `lint` (hazards acknowledged where intended), `plan` with a read token: the admitted plan is stored with an observation of the target.
3. `/godwit apply` on the pull request: `migrate --rollout expand-contract` binds to the stored plan (or refuses with exit 3 when the target moved) → run ends `awaiting_contract`, step exits 0, `godwit/applied` on the head stays **`pending`** ("expand applied; comment /godwit confirm to run the contract phase"); the outcome is posted on the pull request.
4. Deploy the application version that handles both shapes.
5. `/godwit confirm` on the pull request (or `run confirm --latest --allow-none` from the deploy pipeline) → the same run resumes with `phase = contract` and ends `succeeded`, `godwit/applied` turns `success` and the pull request becomes mergeable. Merge: `verify` finds every migration applied.
6. If step 4 fails: `/godwit revert` on the pull request, or `godwit revert <run-id>`, applies the down side of the expand phase (needs the destructive hazards in the down files acknowledged).

A run whose plan has no contract statements skips `awaiting_contract` and ends `succeeded` directly, so the same pipeline serves additive and destructive changes.
