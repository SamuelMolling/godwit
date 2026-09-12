# CI/CD

Three integrations ship in the repository: a composite GitHub Action (`action.yml` at the root), a GitHub App the service receives webhooks for ([below](#github-app)), and ArgoCD hook Jobs (`deploy/argocd/`). Both are thin wrappers over the CLI; anything they do you can do with `godwit` in any runner. Complete workflows and `Application` manifests to copy are in [examples/](../examples/README.md).

The CLI outside GitHub comes from the image `ghcr.io/samuelmolling/godwit` (`main`, `sha-<short commit>`; built by `.github/workflows/publish.yml` on every merge) or, once a `v*` tag exists, from the GitHub release and `brew install SamuelMolling/tap/godwit` (`.github/workflows/release.yml`, GoReleaser).

## Exit codes

| Code | CLI | Meaning |
|---|---|---|
| 0 | all | success; for `migrate`, `apply` and `revert`: the run reached `succeeded` or `awaiting_contract`; for `confirm`: the contract phase ran; for `verify`: every migration is applied |
| 1 | all | error: blocking lint findings, refusal at admission, run `failed` or `needs_attention`, a migration `verify` found pending, no run of the pull request awaiting its contract phase, a refused command ([who may command an apply](#who-may-command-an-apply)), a fork trying to apply, connection or usage error |
| 2 | Action only | unknown `command` or `mode`, a command the mode refuses, an `apply-on` that enables nothing, an `allowed-associations` or `require-approval` that is not a valid value, or an applying command on `pull_request_target` |
| 3 | `migrate`, `apply` | plan stale or required: re-plan on the pull request |

The Action's last step re-exits with the CLI status after the summary, the comment and the status are written, so a failing lint still posts its report. Exit 1 and exit 2 in the event-reading step happen before anything runs, and both [answer on the pull request](#a-refusal-answers-on-the-pull-request) rather than only in the job log.

## The merge signal

**godwit already gates the merge, and it does it with a commit status.** Every applying command sets
`godwit/applied` on the pull request head; make it a required status check on the base branch and GitHub will
not let the pull request merge until the apply has landed. There is nothing else to enable and no godwit
setting to turn on — the Action sets the status whenever it holds `statuses: write`.

On the base branch's protection rule, require:

- **`godwit/applied`** — the apply is the gate of the merge.
- **"Require branches to be up to date before merging"** — when the base moves, GitHub re-runs the pull request
  workflow, so the plan stored last is the one computed on the exact set the pull request applies.

Optionally also **`godwit/plan`**, which carries the plan's verdict and stays `pending` while a hazard is
unacknowledged ([what each state means](#pull-request-lint-and-plan)). It is a separate choice: `godwit/applied`
is the check that gates the merge on the apply.

What `godwit/applied` says, and when:

| State | Description | Set by |
|---|---|---|
| `pending` | `applying <sha> from pull request #<n>` | the apply, confirm, revert or migrate step, before it runs anything |
| `success` | `applied by run <id>; merge when the review is done` | an apply that reached `succeeded` |
| `pending` | `expand applied; comment godwit confirm to run the contract phase` | an apply that reached `awaiting_contract` — half a migration is on the database, so the pull request stays unmergeable |
| `success` | `contract applied by run <id>; merge when the review is done` | `godwit confirm` |
| `failure` | `plan stale or missing: re-plan on the pull request, then godwit apply again` | an apply the target moved underneath (exit 3) |
| `failure` | `apply failed (run <id>); see the pull request comment` | a run that stopped |
| `failure` | `reverted by run <id>; comment godwit apply to apply again` | `godwit revert` |
| `failure` | the refusal's own text, cut at 140 characters | a command godwit refused before running it ([a refusal answers on the pull request](#a-refusal-answers-on-the-pull-request)) |

The status is **per commit**. A push after the apply leaves the new head without one, so the pull request
becomes unmergeable again and the apply has to be commanded on the new head — which is the point: the commit
that merges is the commit that was applied.

**Auto-merge is GitHub's, and it composes with this.** With "Allow auto-merge" enabled on the repository, a
reviewer presses *Enable auto-merge* and GitHub merges the pull request the moment every required check is
green and the reviews are in — `godwit/applied` among them. Nothing about that needs godwit.

### Why godwit does not merge the pull request for you

Atlantis merges after `atlantis apply` (`--automerge`, or `automerge: true` in a repository's `atlantis.yaml`).
godwit deliberately has no equivalent, for reasons that are about godwit's shape rather than about Atlantis:

- **A godwit apply is not always the end of the migration.** An `expand-contract` apply *ends* at
  `awaiting_contract` on purpose, with the application deploy and `godwit confirm` still to come. An automerge
  that fired on a successful apply would merge before the contract phase; one that waited for `succeeded` would
  do nothing at all on the rollout godwit exists to make safe.
- **"All of it applied" is not a question godwit can answer from one command.** Atlantis's automerge requires
  every project in the pull request to be `applied`, and when only some are it returns silently — no comment, no
  failed status, just a line in the server log. A pull request that touches two targets, or two directories,
  would land godwit in the same place, and a merge signal that is silent when it declines is worse than none.
- **GitHub already does it, and it does it better.** Auto-merge waits on *every* required check and on the
  reviews, not just on the one command that happened to run last. It is per pull request, a reviewer opts into
  it, and it is visible in the UI. godwit's job is to make `godwit/applied` tell the truth; deciding to merge is
  the repository's.

If you want the merge automated, enable auto-merge and require `godwit/applied`. If you want it automated
*without* branch protection, you are asking for a migration to reach `main` without the check that says it was
applied, and godwit will not build that.


## GitHub Action

```yaml
- uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
  with:
    command: lint | plan | apply | confirm | verify | revert | migrate | diff
```

It builds godwit from the checked-out action ref with `actions/setup-go` (`CGO_ENABLED=1`, needs `gcc`), so the first run in a job takes a minute; `go-version` pins the toolchain. The runner also needs `jq` and `gh` (present on GitHub-hosted runners).

**Pin the sha, not a branch.** The examples and the snippets on this page use a commit because a job running `command: apply` holds a `pipeline` token: `@main` means whoever obtains one push to this repository executes DDL on every consumer's production database at their next apply, with no version to roll back to. Replace the sha with the godwit commit you reviewed, and pin every other action in those workflows the same way — `actions/checkout@v4` is a tag its owner can move.

### Two modes

The default, `mode: apply-on-pr`, is the Atlantis model: the pull request plans, `godwit apply` on the pull request applies, and the merge only verifies. `main` never carries a migration the database does not have, because the merge is gated on the `godwit/applied` commit status that only a successful apply sets.

| Event | Command | What happens |
|---|---|---|
| `pull_request` | `lint`, `plan` | lint the new files; store the admitted plan on the service; sticky comments; status `godwit/plan` on the head commit |
| `issue_comment` `godwit plan` | `plan` | re-plan the pull request head and store it, without pushing ([re-planning from a comment](#re-planning-from-a-comment)) |
| `issue_comment` `godwit apply`, or `pull_request_review` | `apply` | `migrate` from the pull request head, bound to the stored plan; status `godwit/applied` on the head commit |
| `issue_comment` `godwit confirm` | `confirm` | the contract phase of the run the pull request left in `awaiting_contract`; the status goes from `pending` to `success` |
| merge (`push`) | `verify` | `migrate --dry-run`: fails when a migration on `main` is not applied; never applies |
| `issue_comment` `godwit revert` | `revert` | undoes what the run(s) of the pull request applied, newest first; the dry-run plan goes in the comment before anything is queued; status back to failure |

In this mode `command: migrate` is refused (exit 2) unless `dry-run: "true"`. `mode: apply-on-merge` keeps the previous flow: `plan` on the pull request, `migrate` on push; `apply`, `confirm` and `revert` are refused there (confirm the contract phase from the deploy pipeline with `godwit run confirm --latest --allow-none --target <t>`, [below](#expand--contract-in-a-pipeline)). Use it when nothing may touch the database before the merge (for example when the PreSync hook in [ArgoCD](#argocd) is the only thing allowed to apply).

### Action inputs and outputs

| Input | Default | Used by |
|---|---|---|
| `command` | required | `lint`, `plan`, `apply`, `confirm`, `verify`, `revert`, `migrate`, `diff` |
| `mode` | `apply-on-pr` | `apply-on-pr` (apply, confirm and revert from the pull request, verify on push, migrate refused) or `apply-on-merge` (migrate on push, apply, confirm and revert refused) |
| `apply-on` | `comment` | apply: `comment` (a `godwit apply` comment or review body), `approve` (an approved review), or `comment,approve`. `confirm` is always commanded by a `godwit confirm` comment or review body |
| `allowed-associations` | `OWNER,MEMBER,COLLABORATOR` | plan, apply, confirm, revert: `author_association` values whose comment or review counts. It **narrows**, it does not authorise ([below](#who-may-command-an-apply)); only `OWNER`, `MEMBER` and `COLLABORATOR` are accepted, anything else is a configuration error (exit 2) |
| `require-approval` | `true` | apply, confirm: GitHub must report the pull request as approved, by a reviewer who holds write or admin permission. Whether a push withdraws that approval is the repository's setting, not godwit's ([below](#who-may-command-an-apply)). `false` drops the requirement |
| `dir` | `dir` from `godwit.yaml`, else `migrations` | all but revert; diff writes the generated pair there |
| `base` | `origin/main` | lint: only migrations added since the ref are linted, files modified since it are `E003`; empty checks every file. The ref is fetched depth-1 when missing |
| `ack` | — | lint, plan, apply, verify, migrate: comma-separated hazard codes; revert: the codes found in the down files (`H002` for `DROP TABLE`, `H009` for `DROP INDEX`, ...). A `--ack` on the command comment adds its codes to these for that run ([flags on a command comment](#flags-on-a-command-comment)) |
| `allow-data-loss` | `false` | revert: run a plan that drops a table or column still holding rows. godwit refuses it by default and names the objects and their row counts in the comment. `godwit revert --allow-data-loss` sets it for that run |
| `force` | `false` | revert: undo a run that is not the newest un-reverted one on its target. `godwit revert --force` sets it for that run |
| `server` | `server` from `godwit.yaml` or `GODWIT_SERVER` | plan, apply, verify, revert, migrate, diff |
| `token` | — | plan, verify and diff (`read`), apply, confirm, revert and migrate (`pipeline`); exported as `GODWIT_TOKEN`, never passed on the command line |
| `target` | `target` from `godwit.yaml` | plan, apply, verify, migrate, diff; confirm and revert (optional, narrows the run search). With a target, `plan` runs on the service and stores the plan; without one it parses the files offline |
| `rollout` | `godwit.yaml`, else `direct` | plan, apply, migrate: part of the plan key, so all must agree. `godwit plan --rollout expand-contract` on a comment sets it for that re-plan |
| `dry-run` | `false` | migrate: `PlanRun` without persisting, markdown report, no run (`command: plan` is the persisting variant); allowed in both modes. diff: report the migration without writing the files |
| `schema` | — | diff: file holding the whole desired database as DDL |
| `prisma` | — | diff: `schema.prisma` rendered to DDL by the Prisma CLI, which the checkout must provide (`npm ci`); exclusive with `schema` |
| `prisma-bin` | `npx prisma`, or `GODWIT_PRISMA_BIN` | diff: command line that runs the Prisma CLI, e.g. `node_modules/.bin/prisma --config prisma.config.ts` |
| `name` | — | diff: migration name, snake_case; the files are `<timestamp>_<name>.{up,down}.sql` in `dir` (required unless `dry-run`) |
| `source` | `<host>/<owner>/<repo>@<pull request head sha, else sha>[:<dir>]` | plan, apply, verify, migrate: provenance stored on the plan (`cp_plans.source`) or run (`cp_runs.source`); revert finds the runs of a pull request by it |
| `comment` | `true` | post the lint, plan, diff, dry-run, apply or revert report — or a [refused command's reason](#a-refusal-answers-on-the-pull-request) — as one sticky pull request comment |
| `comment-on-push` | `true` | on `push`, post the migrate outcome, or a failed verify, on the pull request(s) the commit merged |
| `github-token` | `${{ github.token }}` | reads the pull request, its reviews and the commander's repository permission, posts the comments (`pull-requests: write`) and sets the status (`statuses: write`) |
| `go-version` | `1.26` | build |

| Output | Meaning |
|---|---|
| `run-id` | id of the run created by `apply` or `migrate`, resumed by `confirm`, or of the last revert run (empty for dry runs, verify and refusals) |
| `plan-id` | id of the plan stored by `plan` on the service, or bound by `apply`, `confirm` or `migrate` (empty offline and for implicit runs) |
| `plan-key` | key of the plan stored by `plan` (same files, target and rollout give the same key on every push) |
| `plan-verdict` | the verdict `plan` (or `migrate` with `dry-run`) put in the `godwit/plan` status: `nothing to apply`, `2 to apply, 1 hazard to acknowledge`, `offline plan; no target was consulted` (empty when the plan was refused) |
| `plan-hazards` | number of hazards on what the planned run would execute; empty for an offline plan, which gates nothing |
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

### A refusal answers on the pull request

A command godwit refuses before it runs anything — a commander without write permission, an `author_association` outside `allowed-associations`, no approving review standing on the head, a `godwit apply <sha>` the head has moved past, a flag the command does not take, a workflow whose `mode` or `apply-on` is not a valid value — is answered where it was given: a `## godwit <command> refused` comment carrying the reason, and the commit status that command would have set (`godwit/plan` for `plan` and a `dry-run` `migrate`, `godwit/applied` for `apply`, `confirm`, `revert` and `migrate`) turned `failure` with the same text, cut at GitHub's 140 characters and linking to the comment. `lint`, `verify` and `diff` set no status, refused or not, so a refusal there is the comment and a red job.

That comment is **its own**, marked `<!-- godwit:refused -->`, and not the sticky one: a refusal must never overwrite a plan or apply report that is still the truth about the pull request. It is posted rather than edited, because an edit lands wherever the old comment sits — usually far above the command it is answering — and the previous refusal comment is deleted first, so exactly one stands at a time and it is the newest thing on the page. Nothing else clears it; the status is the live signal, and a later apply turns `godwit/applied` green.

**Silence stays silence.** A comment that commanded nothing — prose around the command, a pasted log, a comment on an issue, an edited comment, a review that neither approves nor commands — is not a refusal: it ends `skipped=true`, exit 0, and posts nothing at all. And a refusal on an event that names no pull request (a `push`, a payload carrying no number) has nowhere to answer: the reason stays in the workflow log and the step says so with a warning. So does a refusal the token cannot post — a `pull_request` run from a fork gets a read-only `GITHUB_TOKEN`, and a caller's workflow without `pull-requests: write` or `statuses: write` gets a warning instead of a comment or a status.

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

`lint` needs no service, unless a `schema_source` block is declared and `server`/`target` are given: it then also checks that the committed migrations still express the ORM schema (`E005`, [below](#pull-request-a-migration-from-the-prisma-schema)). `plan` with a target needs a token with the `read` scope only: it asks the service for the admitted plan against the real target (hazards, out-of-order check, scratch validation, which versions are already applied, which statements would be deferred to contract), stores it with an observation of the target — the step adds `--save` when a target is given, because a plan an apply binds to has to be a plan somebody asked to keep — and posts a `## godwit plan` comment that opens with what will happen to the database, lists the migrations the run would apply (`+`) or revert (`-`) in a `diff` fence the forge paints green and red, and then describes what each one does to the schema — one block per table, index, sequence, enum, view, function or procedure, with the hazard each change carries on the line that causes it and its recipe in a collapsed block — closing on a `Plan: N to add, N to change, N to destroy.` line. Inside those fences the `+`, `-` and `~` markers sit in column 0 with the indentation after them, which is the only placement GitHub paints. `plan.format: statements` in `godwit.yaml` ([configuration](configuration.md#plan)) makes the comment list the SQL statement by statement instead, which is also what it falls back to whole when there is no schema delta to describe (no target, `skip-validation`, a service with no validator); then the closing line is `Plan: N to apply, N to revert, N hazard(s) to acknowledge`. Either way the changes outside migrations fold into a collapsed block under it, and so does `how this will run`: transactions, locks, the pause between an expand/contract rollout's two halves, and what a failure part way through leaves behind. With nothing pending the first line is the target's current state (`Nothing to apply. orders is at <version> (N migrations).`) and there is no block explaining what the target already has; a bookkeeping table left behind by another migration tool is surfaced as a note. The plan id and key ride in HTML comments (`<!-- godwit-plan-id: … -->`, `<!-- godwit-plan-key: … -->`) so the step can read them without putting a UUID and a 64-character hash in front of a reviewer, and the verdict and the hazard count leave the same way (`<!-- godwit-plan-verdict: … -->`, `<!-- godwit-plan-hazards: N -->`): the step fills the commit status below from those, never by re-deriving a verdict from the prose. That stored plan is what the apply binds to ([concepts: plans](concepts.md#plans)); a refusal becomes a `## godwit plan` comment with the reason and a failed step. Without a target the step parses the files offline as before. `dry-run: "true"` on `migrate` gives the same report without storing anything. On `pull_request` events the `source` records the pull request head, not the merge commit GitHub checks out.

The verdict also lands where people actually look, the checks list at the top of the pull request: `plan` (and `migrate` with `dry-run: "true"`) sets the commit status **`godwit/plan`** on the head, whose description is the verdict and nothing else, because GitHub cuts a description at 140 characters — `nothing to apply`, `2 to apply, 1 hazard to acknowledge`, `offline plan; no target was consulted`. It links to the plan comment, or to the workflow run when `comment: "false"`. The state says what is left to decide:

| State | When |
|---|---|
| `success` | the plan stands and what it would apply carries no hazard (`nothing to apply`, `2 to apply`), or it was an offline plan, which decides nothing about a database |
| `pending` | the plan stands, and what it would apply carries a hazard acknowledged in `ack` (`2 to apply, 1 hazard to acknowledge`). The plan did not fail — the hazard is information — but it is not a green light either |
| `failure` | the plan was refused and nothing was stored: an unacknowledged hazard, a version out of order, a validation failure, an unreachable service. The reason is the `## godwit plan` comment and the step log |

`godwit/plan` is a second context beside [`godwit/applied`](#pull-request-apply), not a replacement: one carries what the pull request would do to the database, the other whether it was done. A repository that makes `godwit/plan` a required check is choosing that a migration carrying a hazard cannot merge on the plan alone, and there is no click that turns it green: acknowledging the code in `ack` moves it from `failure` to `pending`, and only rewriting the migration — take the recipe printed beside the statement — makes it `success`, because an `--ack` on the apply comment never re-plans. Leave it out of the required set if you would rather acknowledge a hazard and move on; `godwit/applied` is still the check that gates the merge on the apply.

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

### Re-planning from a comment

A `pull_request` event re-plans on every push, which covers the usual case: the plan on the pull request is the plan of its head. `godwit plan` as a comment re-plans that same head without one — when the target moved under the stored plan and the migrations did not, when the plan was refused for a hazard that has since been accepted through the `ack` input, or to plan the same files under a different rollout:

```yaml
      - uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with:
          command: plan
          dir: db/migrations
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_READ }}
          target: orders
```

**A re-plan from a comment changes what a later `apply` does.** `apply` binds by plan key, not by plan id, so the newest stored plan with that key is the one it runs; the comment therefore reaches past the pull request's own content into what the next apply will execute. It is commanded like `apply` ([what counts as commanding](#what-counts-as-commanding)) and authorised like it — `allowed-associations` and write or admin permission on the repository ([who may command an apply](#who-may-command-an-apply)) — and it is refused when the pull request is closed, when the checked-out commit is not the head, or when a `godwit plan <sha>` names a commit the head has moved past. It does **not** need an approving review: `require-approval` anchors the apply to a reviewed commit, and a plan applies nothing. `--rollout` is part of the plan key, so `godwit plan --rollout expand-contract` stores the plan under a different key from the one an `apply` step left on the default rollout looks for; the two have to agree, as they already do between the `plan` and `apply` steps of a workflow.

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
    if: github.event_name == 'pull_request_review' || (github.event.issue.pull_request && contains(github.event.comment.body, 'godwit '))
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

Once the review is done, a collaborator comments `godwit apply` — the **whole comment**, nothing else in it; also accepted as the body of a review. With `apply-on: comment,approve` an approving review applies as well. `/godwit apply`, which earlier versions of this page documented, is still accepted for every command; `godwit apply` is the form to write and the one the reports and statuses print. The step:

1. Reads the event. The command has to be the entire comment ([what counts as commanding](#what-counts-as-commanding)), so prose around it — a log pasted above it, a sentence explaining it to a teammate, a thank-you below it — means nothing fires. It may name the commit the commenter was reading (`godwit apply <sha>`), and then that sha must be the head. A comment that is not the command, a review that does not apply, an edited comment or a comment on an issue end the step with `skipped=true` and exit 0. The command takes the flags that command has ([below](#flags-on-a-command-comment)).
2. Authorises the commander — `allowed-associations`, then the real permission lookup and the approval described [below](#who-may-command-an-apply). Every refusal exits 1, says which check failed, and [answers on the pull request](#a-refusal-answers-on-the-pull-request).
3. Reads the pull request through the API: it must be open, and the checked-out commit must be its head, so the job has to check out `refs/pull/<n>/head` (the default checkout of `issue_comment` is the default branch). This catches a mis-configured checkout. It does **not** catch a push that raced the job: both sides of that comparison move together, which is what `godwit apply <sha>` and the `review.commit_id` check are for.
4. Sets the commit status `godwit/applied` to `pending` on the head, then runs `godwit migrate` from the pull request files with the same `dir`, `target` and `rollout` as the plan step, so it binds to the stored plan ([concepts: plans](concepts.md#plans)) and refuses when the target moved since (exit 3, `stale=true`).
5. Posts the `## godwit apply` report on the pull request and sets the status: `success` ("applied by run …; merge when the review is done"), `pending` when the run stopped at `awaiting_contract` ("expand applied; comment godwit confirm to run the contract phase", output `phase=awaiting-contract`), or `failure` with the reason (stale plan → re-plan then command again; SQL error → the run's error is in the comment). The status links to the comment, and gates the merge ([the merge signal](#the-merge-signal)).

The report is [`godwit run report`](cli.md#godwit-run-report) rendered as markdown, so it says what the apply did in the vocabulary the plan comment above it used: the migrations that reached the target's history and what they changed in it, the statements that ran and how long the run took, what a held `expand-contract` run left for `godwit confirm`, and, when the run stopped, the statement it stopped at with its SQL and the database's error. Set `GODWIT_PUBLIC_URL` in the workflow's environment and the report links the run and its plan to their pages in the UI:

```yaml
env:
  GODWIT_PUBLIC_URL: https://godwit.internal
```

It is the same setting the service reads for Slack's "Open run" button, and there is no Action input for it: the report is rendered by the CLI, which reads it from its own environment. Unset, the report names the run and the plan and links neither.

The status is per commit, so a push after the apply leaves the new head without one ([the merge signal](#the-merge-signal)). The `source` recorded on the run is `github.com/<owner>/<repo>@<head sha>[:<dir>]`, which `godwit runs`, `godwit audit` and `revert` use — and which the report turns into the commit link.

### What counts as commanding

**The command must be the entire comment.** A comment is trimmed of surrounding whitespace, any backticks wrapping the whole of it are stripped, and what is left must be the command and its arguments — nothing before it, nothing after it, no second line. Anything else names nothing: the step ends `skipped=true` and exits 0.

| Comment | |
|---|---|
| `godwit apply` | commands |
| `` `godwit apply` `` , ```` ```godwit apply``` ```` | commands — GitHub's copy and quote-reply wrap a command in backticks |
| `godwit apply <sha> --ack H001` | commands; this rule is about surrounding content, not arguments |
| `To deploy, comment:` ⏎ `godwit apply` | silence |
| `godwit apply` ⏎ `thanks!` | silence |
| a pasted log or a fenced block containing `godwit apply` | silence |
| `please godwit apply` | silence |

This is [Atlantis's rule](https://github.com/runatlantis/atlantis/blob/main/server/events/comment_parser.go) — its parser ignores any comment with a second non-empty line — and godwit takes it for the same reason. Without the `/` prefix, `godwit apply` on its own line is exactly how someone explains the tool to a teammate, and the person explaining is usually someone whose permission would let the apply through. Requiring the whole comment costs the commander nothing and takes that footgun away. It also removes the need for a fenced-code-block rule: a real fenced block spans lines, so it is already silence.

**A review body obeys the same rule.** The parser is the same one, deliberately: two grammars is what this page used to have and it is not worth having again for a convenience. Approving *and* saying something is a normal thing to do, but it already has its own path — `apply-on: approve` makes the approving review itself the trigger and never reads the body ([who may command an apply](#who-may-command-an-apply)), so prose and an apply coexist there. When the body is the command, it has to be only the command. The failure mode of getting this wrong is inaction: nothing applies, and the reviewer comments again.

### Flags on a command comment

A commanded line may carry the flags its command takes, after the optional sha:

| Command | Flags |
|---|---|
| `godwit plan` | `--rollout direct`, `--rollout expand-contract` |
| `godwit apply` | `--ack H001`, `--ack H001,H003` (or `--ack=H001`) |
| `godwit confirm` | none |
| `godwit revert` | `--ack …`, `--allow-data-loss`, `--force` |

`--ack` on a comment is the escape hatch for a hazard someone has decided to accept on this one run — the main path stays editing the migration to use the recipe the report prints beside the statement, and the `ack` input stays the place for a code the repository always accepts. The comment's codes are **added** to the `ack` input; `--allow-data-loss` and `--force` set the corresponding input for that run.

A comment that *is* the command and then carries something the command does not take — an unknown flag, a flag belonging to another command, a hazard code that is not one, `--ack` with nothing after it — **fails the job** with the reason, [on the pull request](#a-refusal-answers-on-the-pull-request). It does not quietly apply without what was asked for. That is not the same as a comment which never commanded at all — one with prose around it, or one that names no command — and those still end `skipped=true` and exit 0, even when the text inside them would have been a refusal on its own.

A caller's workflow that filters events before the Action runs has to let the flags through: a job that matches the comment with an exact `== 'godwit apply'` on the first line refuses `godwit apply --ack H001` before godwit ever sees it. Match loosely (`contains(..., 'godwit apply')`) instead, and leave the parsing and the refusals to the Action. `contains` rather than `startsWith` is also what lets `/godwit apply` — the form earlier versions of this page documented, still accepted — reach the Action.

The `apply` step also accepts `pull_request` events, for teams that apply on every push to a labelled pull request. There is no comment to authorise there, so the guards that remain are the approval (`require-approval`, on by default: GitHub must report the pull request as approved) and a refusal when the pull request comes from a fork — GitHub withholds the secrets from a fork's `pull_request` run anyway, and a stated refusal beats a confusing authentication failure. The workflow's own `if` is the rest of the gate.

**`pull_request_target` is refused for `apply`, `confirm`, `revert` and a real `migrate` (exit 2), and refused outright for a pull request opened from a fork (exit 1).** That event runs in the base repository with its secrets and a write token *for code the fork controls*: a workflow that reached `apply` from it would hand `secrets.GODWIT_TOKEN_PIPELINE` and production DDL to anyone who can open a pull request. Earlier versions of this page recommended exactly that shape. They were wrong; use `pull_request`, or command the apply from a comment.

### Who may command an apply

`author_association` is a **relationship, not a permission**: `MEMBER` is returned for every member of the organisation that owns the repository, including members with no access to this repository at all. `allowed-associations` therefore only narrows; three checks authorise, in this order, and each refusal names itself:

1. **`allowed-associations`** — the commenter's `author_association` must be in the list. `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, `MANNEQUIN` and `NONE` are rejected as *configuration* (exit 2): anyone who opened a pull request carries one of them, so listing one would authorise the world.
2. **Repository permission** — `GET /repos/{owner}/{repo}/collaborators/{login}/permission` must return `admin` or `write`, for the commander and, when an approval is required, for the approver too. The `github-token` must be able to read that; if the call fails, the command is refused rather than allowed.
3. **GitHub reports the pull request as approved** (`require-approval`, default `true`) — `GET /pulls/{n}/reviews`, the latest review of each reviewer, and at least one of those must be `APPROVED`. That is the whole rule. godwit does not decide for itself whether an approval still counts; it asks GitHub and takes the answer, so a pull request the reviewer's page shows as green is never one godwit refuses as unapproved.

**Whether a push withdraws the approval is the repository's setting, not godwit's.** GitHub's branch protection has *Dismiss stale pull request approvals when new commits are pushed*: with it on, a push that changes the diff makes GitHub itself rewrite the review's state to `DISMISSED`, the pull request stops being approved, and godwit refuses for free — no re-implementation, and one source of truth for the whole team, including the merge button. Enable it (or *Require approval of the most recent reviewable push*) if the team wants approvals to expire on a push. Leave it off and they do not, and godwit does not second-guess that either. Note that godwit's rule is stricter than GitHub's own review requirement on one point it does not have to infer: a reviewer's later `CHANGES_REQUESTED` supersedes their earlier `APPROVED`, because that is the state GitHub reports for them.

An approving review that itself triggers the apply (`apply-on: approve`) is still checked against `review.commit_id`. That is a check on the **command**, not on the approval: the review event is what fired this job, and if a push landed between submitting it and the job running, the commit the reviewer pressed the button on is not the commit that would be applied. It is the same guard as `godwit apply <sha>` in a comment.

Two costs, stated plainly. `require-approval: true` needs a **second person** — GitHub does not let a pull request's author approve it, so a solo maintainer must approve from another account or set `require-approval: "false"`. And the permission lookup is **one extra API call** per command (two when an approval is checked), needing a `github-token` that may read collaborators.

### Pull request: confirm the contract phase

An `expand-contract` apply whose plan holds statements back ends in `awaiting_contract`: the expand phase is on the database, the contract phase is not, and the migration is only half done. The step exits 0, so `godwit/applied` must **not** say `success` there — it stays `pending` with "expand applied; comment godwit confirm to run the contract phase". Branch protection keeps the pull request unmergeable until the contract phase runs, which is the point: `main` never carries a migration the database has only half of.

```yaml
      - name: Run the contract phase held by the apply
        if: contains(github.event.comment.body, 'godwit confirm')
        uses: SamuelMolling/godwit@f4d803c9aae750b85ee35c75cabb990ea98d2eb6
        with:
          command: confirm
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_PIPELINE }}   # pipeline scope, same as apply
          target: orders
```

Once the application version that reads both shapes is deployed, a collaborator comments `godwit confirm`. The step reads the event under the same rules as `apply` ([who may command an apply](#who-may-command-an-apply), open pull request, the checked-out commit must be the head), sets `godwit/applied` to `pending` ("confirming the contract phase of …"), then:

1. Lists the commits of the pull request and the runs of the target, and takes the newest run whose `source` is `<repo>@<one of those commits>` and whose state is `awaiting_contract` — the same provenance match `revert` uses, so it can only release what this pull request applied.
2. Calls `ConfirmRollout` on it and streams the run to its end. It is the **same run id**, resumed at the statement it stopped at with `phase = contract`: no second plan, no second bind, nothing re-executed ([concepts: rollout policies](concepts.md#rollout-policies)).
3. Posts the `## godwit confirm` report and sets `godwit/applied` to `success` ("contract applied by run …; merge when the review is done"), or `failure` with the run's error. Output `phase` is `contract`.

No `dir` and no `rollout`: the contract phase runs the statements the plan already froze, so the files are never re-read. When no run of the pull request is awaiting its contract phase the step fails (exit 1) with "no run of pull request #N is awaiting its contract phase" and **leaves the status alone** — a green apply is not turned red by a command that did nothing.

If the head moved while the run was awaiting its contract phase, the head guard refuses the command (the status on the new head was never set by that apply, and a new apply is refused while the target has a run awaiting contract). Confirm it from the CLI instead — `godwit run confirm --latest --target orders` — and re-plan on the new head.

### Pull request: revert

When a pull request is closed without merging after it applied, `godwit revert` runs `command: revert`: it lists the commits of the pull request, finds every run whose `source` is `<repo>@<one of those commits>` and is `succeeded`, `awaiting_contract`, `failed` or `needs_attention`, and calls `godwit revert <id>` for each, newest first, stopping at the first failure. The down files pass the hazard gate, so `ack` must carry their codes (`H002`, `H009`, ...). A merged pull request is refused: its migrations belong to the base branch now, revert them from a new pull request. The report is posted on the pull request and the status goes back to `failure` ("reverted by run …"), so the pull request cannot be merged until it applies again. The original run ends `reverted`, the plan it was bound to is retired when the next apply binds ([concepts: revert](concepts.md#revert)); a re-plan stores a fresh plan and `godwit apply` binds to that one.

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

A repository whose migration directory does not exist yet verifies green and reports *no migration yet*, which is what the first merge of these workflows is — an `issue_comment` workflow only runs from the default branch, so the wiring has to land before the first migration ([when `--dir` holds nothing](cli.md#when---dir-holds-nothing)). The same directory missing once the target has migrations applied fails instead, naming what the target has.

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

With `rollout: expand-contract` the apply (or the merge step in `apply-on-merge`) exits 0 while the run sits in `awaiting_contract`. On a pull request the contract phase is released by [`godwit confirm`](#pull-request-confirm-the-contract-phase); everywhere else, confirm from the deploy pipeline once the new application version is out:

```yaml
      - run: godwit run confirm --latest --allow-none --target orders
        env:
          GODWIT_SERVER: https://godwit.internal
          GODWIT_TOKEN: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
```

`--allow-none` makes the step a no-op when nothing awaits (a deploy that shipped no migration). Without it the CLI fails with `target orders: no run awaiting contract`. The step waits: `run confirm` streams the contract phase it released and exits with the run, so the deploy fails when the swap fails instead of going green on a phase that was only queued. Add `--no-wait` when the pipeline watches the run somewhere else.

## GitHub App

The Action needs a runner that can reach godwit, and a workflow in every consuming repository. The App inverts that: the service receives the pull request events itself, so a consumer configures a webhook and nothing else. [Decision 0016](decisions/0016-the-app-is-bound-to-targets-by-the-server.md) has the reasoning, what an attacker gains from the webhook secret, and what is deliberately not built.

**All four commands run.** A delivery is verified, de-duplicated, authorised and resolved to the projects the pull request touches; godwit then reads those projects' migration directories at the pull request's head over the Contents API and carries the command out — `plan` stores the plan and reports it, `apply` creates the run, `confirm` releases a held contract phase, `revert` undoes what the pull request applied. A consuming repository needs `godwit.yaml` and nothing else: no workflow, no action pin, no token, no secret. [Decision 0019](decisions/0019-the-app-reads-the-migrations-at-the-head.md) is where the files come from and what bounds them; [0020](decisions/0020-a-run-is-bound-to-the-pull-request-that-asked-for-it.md) is how a run that outlives the delivery reports back.

### Registering the App

One App per godwit deployment, registered by the operator — never a shared, publicly listed one, because a shared App means one webhook secret across unrelated fleets.

1. **New GitHub App**, under the organisation that owns the repositories. Webhook URL is `https://<host><path>` where the path is `/github/webhook`; set a webhook secret and keep it wherever the master key lives.
2. **Repository permissions**: `Metadata: read` (the collaborator permission lookup), `Pull requests: **write**` (the pull request and its reviews, the emoji reaction on a command, and the comment saying why godwit did nothing), `Contents: read` (the project file, and the migration files once the App fetches them), `Checks: write` (the report, once the App writes one). Nothing needs `write` on contents, and the App never asks for it: a service that can write to the repository can write the migration it is about to apply.
3. **Subscribe to** `Issue comment`, `Pull request` and `Pull request review`. Anything else is answered `202` and dropped.
4. **Generate a private key** and install the App on the repositories that will use it — *Only select repositories*, not all of them. Installing it grants nothing on its own — see the binding below.
5. Point `--github-webhook-addr` at a port only the tunnel or ingress reaches, and give the service `GODWIT_GITHUB_APP_ID`, `GODWIT_GITHUB_WEBHOOK_SECRET` and the key ([configuration](configuration.md#github-app)). Without `--github-webhook-addr` the listener does not exist.

**Publish that listener on a hostname of its own.** It is the only part of godwit that has to be reachable from GitHub, and it is a second listener rather than a path on the API's so that publishing it publishes nothing else — a route that reaches `/github/webhook` must not be able to reach `/godwit.v1.GodwitService` by changing its path. The Helm chart renders the App its own Service, `<release>-webhook`, carrying the webhook port and nothing else: point the public route at that Service by name and a wrong port number cannot reach the API ([chart README](../deploy/helm/godwit/README.md#the-github-app-listener)).

**The private key goes in as a file or as environment**, `--github-private-key-file` or `GODWIT_GITHUB_PRIVATE_KEY`, and never as a flag holding the PEM: an argument is readable from `/proc/<pid>/cmdline` on the node and from the pod spec, so godwit has no such flag. Both at once fails `serve` rather than one quietly winning. The file is the stronger of the two: it is the App's whole identity, it mints an installation token for every repository the App is installed on, and a file keeps it out of the environment, which a sidecar, a core dump and `kubectl exec -- env` all read. The chart's default takes the PEM from the Secret as `GODWIT_GITHUB_PRIVATE_KEY`, which is where a Secret written for godwit's environment already has it; naming an entry in `existingSecret.githubPrivateKey` projects it at `serve.githubApp.privateKeyPath` as a file instead.

With the chart, steps 5 and 6 are:

```yaml
# GODWIT_GITHUB_APP_ID and GODWIT_GITHUB_WEBHOOK_SECRET go in the Secret; the pods refuse to
# start without either. Empty githubPrivateKey, the default, reads the PEM from GODWIT_GITHUB_PRIVATE_KEY
# instead of mounting the entry named here.
existingSecret:
  githubPrivateKey: ""
serve:
  githubApp:
    enabled: true
notifications:
  publicUrl: https://godwit.example.internal   # or the App's links point nowhere
```

`serve.githubApp.enabled: false` is the default, and off means the listener never binds: no `--github-webhook-addr`, no second container port and no webhook Service in the rendered manifests.

### Binding a repository to a target

**Installing the App grants nothing.** A repository bound to no target gets no apply and no plan, and the refusal is in GitHub's delivery log and the service's own. An operator binds it with the target:

```bash
godwit target add orders --provider vault --credential-store production --vault-path database/creds/orders \
  --github-repo acme/orders \
  --github-repo acme/monorepo:services/orders
```

A GitOps deployment runs that same line from a Job rather than by hand — the binding is part of the target's row, so it is registered the way the rest of the row is, and never declared in the chart's values ([decision 0022](decisions/0022-control-plane-data-is-not-chart-configuration.md)).

Each entry is `owner/repo`, whose project is the repository root, or `owner/repo:dir`, where `dir` is the directory holding that project's `godwit.yaml`. It is **not** the migration directory — `dir:` inside that file names those, relative to it. `godwit targets` prints the bindings in its `GITHUB` column, `godwit target show` alongside the rest of the registration. `--github-repo` replaces the whole list when you pass it, so pass every repository at once; a `target add` that omits it leaves the bindings alone, and `--github-repo=""` unbinds them all.

`godwit.yaml` still names the target, and is still where a repository says what it wants. It is now a request: the server reads the name from the head sha and looks it up in the binding. A name the binding does not carry is refused in words that do not say whether that target exists, because a webhook caller has no `ListTargets` and a refusal should not become one.

### What makes a pull request worth planning

The Action leaves this to the workflow's `paths:` filter. With no workflow, the App decides it from the changed files, the way [Atlantis's `autoplan.when_modified`](https://www.runatlantis.io/docs/repo-level-atlantis-yaml.html#autoplan) does — [decision 0017](decisions/0017-the-repository-asks-for-a-plan-and-the-binding-permits-it.md) has the comparison and what godwit does differently.

A project's default trigger is **its own migrations, and the file that says where they go**:

| Changed file | |
|---|---|
| `db/migrations/20260101000000_add_col.up.sql` | plans |
| `godwit.yaml` | plans — it names the target and the rollout |
| `db/migrations/README.md` | nothing |
| `db/seeds/reference.sql` | nothing — `.sql` outside the migration directory is not a migration |
| `src/app.js` | nothing |

A project widens that with `autoplan` in its own `godwit.yaml`:

```yaml
dir: db/migrations
target: orders
autoplan:
  enabled: true                     # false stops the automatic plan; a godwit comment still works
  when_modified: ["prisma/**"]      # added to the default, never replacing it
```

Patterns are globs relative to the project's directory — `*` does not cross `/`, `**` does. They may not begin with `/` or contain `..`: a project's trigger stays inside the project, which is also what lets the server skip a project a pull request did not touch without reading its file at all. **`when_modified` adds to the default rather than replacing it**, so a project cannot stop planning the migrations it owns except by saying `enabled: false`. Atlantis replaces, and its own docs call the resulting mistake the common one.

Nothing here needs an operator's permission, and nothing here grants any: the trigger says when godwit looks at a project, and the [binding](#binding-a-repository-to-a-target) says which database that project may reach. A repository that widens its own trigger gets more plans of its own target.

**A pull request touching several bound projects plans all of them**, ordered by target name. **One touching none is silence** — no plan, no comment, nothing, which is the point of having a trigger. A `godwit apply` comment on such a pull request is refused instead, because a person asked and is owed an answer.

**A migration directory godwit cannot see the whole of is refused too.** The Contents API answers at most 1000 entries for one directory and does not say when it truncated, so a directory that comes back exactly full is refused rather than planned as the smaller set it looks like. That caps a project the App can plan at 500 migrations, below the 2000 `--max-migrations` admits; the refusal names the count. The limits themselves — `--max-files`, `--max-file-bytes`, `--max-migrations` — are decided from the listing's own names and sizes, before any body is fetched, so a directory over one of them costs a single request.

**A pull request godwit cannot see the whole of is refused, never called empty.** `GET /pulls/{n}/files` answers with at most 3000 files and does not say when it truncated, so godwit compares what it listed against the count the pull request itself reports and stops believing a listing that falls short of it. Concluding "nothing to plan" there would report a migration as absent when it is in the pull request and the reviewer would merge on it, so instead every command is refused with what was listed, what was expected, and the advice to split the pull request or land the migrations in one of their own. The same rule refuses an apply whose review list hit its cap: GitHub lists reviews oldest first, so a truncated read drops exactly the dismissals that withdraw an approval.

### What the App does not do: `godwit diff`

"I changed my Go model — how does godwit know to write a migration?" is not the App's question, and it will not become one.

Deriving a desired schema from an ORM means running the repository's own toolchain: compiling a Go package, running `npx prisma`, running `manage.py`. A central App would be building and executing arbitrary code from any repository in its installation, in the process that holds every target's credential — the surface the App exists to remove. So `godwit diff` and `lint`'s `E005` stay [Action-only](#github-action), in a job that already has a checkout and needs only a `read` token. A schema-source change is therefore not a trigger either; a project that wants a re-plan when its schema moves can name it in `when_modified`.

The App's job starts at the committed migration. `godwit diff` writes the file, the author pushes it, and the App plans it.

### Who may command through the App

The same three checks as [the Action](#who-may-command-an-apply), run server-side against the same endpoints with an installation token narrowed to the one repository the signed payload named:

1. **`author_association`** must be in `--github-allowed-associations` (default `OWNER,MEMBER,COLLABORATOR`). Naming `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, `MANNEQUIN` or `NONE` fails `serve` at start-up rather than at the first comment.
2. **Repository permission** — `admin` or `write` for the commander, and for the approver. A failed lookup refuses.
3. **An approving review GitHub still reports**, for `apply` and `confirm`: the latest review per reviewer, one of them `APPROVED`, and that approver's own permission checked too. There is no `require-approval: false` on this path. As on the Action's, godwit takes GitHub's answer rather than re-deriving it from commit shas — [the amendment to decision 0007](decisions/0007-the-action-authorises-with-permission-and-approval.md#amendment--the-platform-says-whether-a-pull-request-is-approved) says why, and *Dismiss stale pull request approvals when new commits are pushed* is the branch-protection setting that makes a push withdraw one.

A command's grammar is the one on this page: [what counts as commanding](#what-counts-as-commanding) and its [flags](#flags-on-a-command-comment) are the same parser. A comment that names no command is silence, and the App posts nothing about it — the Action's green `skipped=true` tick has no equivalent here.

Beyond those: the pull request must be open (`plan`, `apply`, `confirm`) and unmerged (`revert`); the head must be in the repository the delivery named, so a fork's pull request is refused whatever the command; and a comment or review older than `--github-webhook-max-age` (default one hour) is refused before any of the above, so a redelivered command from yesterday cannot apply today.

### What godwit says back

**A comment godwit reads as a command is marked with an emoji** — `eyes` by default, `--github-emoji-reaction none` turns it off. It is added after the `author_association` filter and before the permission lookup, so a command that takes several API calls to refuse never looks unread, and a comment from someone who could never command godwit costs no API call at all. The reaction means **read**, not accepted; a `godwit apply` that is then refused still carries it. Atlantis has the same thing behind `--emoji-reaction`, defaulted off; godwit defaults it on ([decision 0018](decisions/0018-the-app-answers-where-the-author-is-looking.md) argues why).

**A refusal is said on the pull request, not only in the delivery log.** Anything a person has to act on — an unbound repository, a commander without write, a `godwit.yaml` that does not parse or names a target the binding does not carry, a pull request too large for godwit to see the whole of, a command that arrived too late or carries a flag it does not take — becomes a `## godwit <command> refused` comment carrying the reason, and turns the check that command would have set (`godwit/plan`, or `godwit/applied` for `apply`, `confirm` and `revert`) red with the same text.

It is the [same answer the Action gives](#a-refusal-answers-on-the-pull-request), in the same words: the same `<!-- godwit:refused -->` marker, the previous refusal deleted so exactly one stands and it is the newest thing on the page, and a comment of its own so a refusal never overwrites a report that is still true. The App expresses the signal as a Check Run where the Action sets a commit status — a repository uses one integration or the other, and [0016](decisions/0016-the-app-is-bound-to-targets-by-the-server.md) refused to have both surfaces from one path. A refusal check is concluded as it is created, so nothing is ever left spinning; a comment godwit could not parse into a command sets no check, since godwit does not know which one was meant.

**An accepted `plan` answers with the report and a check.** Every outcome the command reaches is commented, the empty one included: a plan with nothing to apply says so on the pull request rather than leaving the answer behind a check nobody opens. The report is one sticky comment carrying the `<!-- godwit:plan -->` marker — the Action's, so that if both ever ran on one pull request exactly one report stands — and it is the same markdown the CLI writes, plan id, verdict and hazard count in HTML comments included. The `godwit/plan` check opens `in_progress` when the worker picks the command up and is concluded when the plan is: `success`, `action_required` when a hazard on what the apply would run is unacknowledged, `failure` when godwit refused. Its `details_url` is the plan's page in the UI when `GODWIT_PUBLIC_URL` is set. A pull request touching several bound projects gets one comment carrying every project's report and a check each, named `godwit/plan (<target>)` so they can be told apart; with one project the check is plain `godwit/plan`, which is what a required check names.

A pull request that changed nothing godwit plans still gets nothing. Failing to react, to comment or to set the check is a warning in the service log and never fails the delivery; a report GitHub would not take as a comment is said in the check that does conclude, which carries the posting error under the report, so the missing comment is diagnosable from the pull request rather than only from the service log — a `Pull requests: write` the installation never accepted is the usual reason.

**The command runs after the delivery is answered.** GitHub wants a webhook answered in seconds and a plan builds scratch databases, so the delivery is recorded and answered `202` and the command is carried out by `--github-workers` workers (default 2) behind it. Two consequences worth knowing. The queue is in memory: a replica that dies between the `202` and the plan loses that command, the check stays open, and the recovery is to comment `godwit plan` again — nothing ran. And a queue with no room fails the delivery (`500`) rather than dropping the command, so GitHub's redelivery is a fresh attempt.

**A head that moved is left alone.** If the pull request's head is no longer the one the command was accepted at, godwit abandons it without comment: the push that moved it arrived as its own delivery and is being planned under that one.

### What a run answers with

`plan` finishes inside the command. `apply`, `confirm` and `revert` do not: they create or release a run, and the scheduler executes it — on a lease, possibly on another replica. So the command answers immediately with the run id, leaves its `godwit/applied` check open, and the run reports itself when it settles.

That binding is a row in the store rather than state in one process ([0020](decisions/0020-a-run-is-bound-to-the-pull-request-that-asked-for-it.md)): every replica polls for runs whose outcome the pull request has not been told, claims one under a lease so exactly one replica reports it, and posts the report `godwit run report <id>` renders, with the check concluded from the run's state — `success`, `action_required` for an expand phase holding its contract half, `failure` for a run that stopped. An expand-contract run therefore reports twice, once holding and once when `godwit confirm` releases it.

**`revert` undoes the newest run of the pull request on that target**, not every one of them. The Action loops over all of them oldest first, which refuses on the second without `--force`; the two agree in every case the Action actually handles. There is no `--dry-run` comment before the revert either: `RevertRun` already refuses a plan that would drop a table or column still holding rows unless `godwit revert --allow-data-loss` says so.

**A pull request touching several bound projects gets one comment and one check per project**, both carrying the target: `godwit/plan (orders)` and `<!-- godwit:plan:orders -->`. With one project — the common case — both keep the Action's plain names, which is what a required check and a sticky comment are matched on.

The window that is not covered: a replica dying between opening the check and recording the binding leaves a run that applies with nothing to report it, and a check that spins. The audit entry names the delivery and `godwit runs --target <t>` shows the run.

### What a delivery is answered with

| Answer | When |
|---|---|
| `401`, empty | the signature is missing, malformed or wrong. Nothing but the byte count is learned from such a request |
| `413`, empty | the body is over `--github-webhook-max-bytes` |
| `400` | not a `POST`, no `X-GitHub-Delivery`, or a body that is not JSON |
| `202 accepted` | verified, authorised, recorded |
| `202` with a reason | ignored (an event or action godwit does not act on, a comment that names nothing, a pull request no bound project plans), refused (unbound, unauthorised, stale, a fork, a listing godwit could not read the whole of), or a duplicate delivery id |
| `500` | the store or GitHub could not be reached. Nothing was recorded, so GitHub's redelivery is a fresh attempt |

Every delivery increments `godwit_webhook_deliveries_total{event,result}`.

## ArgoCD

`deploy/argocd/` has two hook Jobs for an application whose migrations are shipped in a ConfigMap alongside the manifests.

**PreSync** (`presync-job.yaml`): `godwit migrate --target=orders --dir=/migrations --rollout=expand-contract` with the `orders-migrations` ConfigMap mounted at `/migrations`, `GODWIT_SERVER=http://godwit.godwit.svc:8474`, `GODWIT_TOKEN` from Secret `orders-godwit` key `token` (scope `pipeline`). `backoffLimit: 0` (the service retries, the Job must not), `activeDeadlineSeconds: 3600`, `hook-delete-policy: BeforeHookCreation`. Exit 0 on `succeeded` or `awaiting_contract` lets the sync proceed with the schema expanded; exit 1 stops the sync, the run's error is in the Job log and in `godwit run get`; exit 3 means the plan stored on the pull request is stale (or required and missing): the sync stops before any pod changes and the Job log carries the diff. The PreSync run binds to the plan the pull request stored: the ConfigMap holds the same `.up.sql`/`.down.sql` bodies as the repository, and the key is computed from those bodies, the target and the rollout, so it matches as long as the pull request planned with `target: orders` and `rollout: expand-contract`.

**PostSync** (`postsync-confirm.yaml`): `godwit run confirm --latest --allow-none --target=orders`, `activeDeadlineSeconds: 600`. After the new pods are healthy, the contract phase runs, and the Job waits for it: `run confirm` streams the phase and exits with the run, so a contract phase that fails fails the hook instead of leaving a green Job over a broken swap. If the sync fails between the hooks, the run stays `awaiting_contract`; the old pods keep working against the expanded schema, and the next successful sync's PostSync confirms it, or an operator reverts it.

Replace `orders`, the Secret name and, for a reproducible hook, the image tag (`ghcr.io/samuelmolling/godwit:main` in the examples; `sha-<short commit>` is immutable); the ConfigMap must contain both `.up.sql` and `.down.sql` files (the CLI loads the directory and rejects a version with a missing side). Runs created by these Jobs carry `created_by = <token name>` and an empty `source` unless `--source` is added to the args.

## Expand → contract, end to end

1. Author the change as two migrations: an additive one (`CREATE`, `ADD COLUMN`, `CREATE INDEX CONCURRENTLY`) and a later destructive one (`DROP`, `RENAME`: H002/H003/H008); or as one `-- godwit:` directive, which godwit splits down the middle itself. The split is by statement: everything up to the first contract statement runs in expand, that statement and everything after it wait for contract ([concepts: rollout policies](concepts.md#rollout-policies)).
2. Pull request: `lint` (hazards acknowledged where intended), `plan` with a read token: the admitted plan is stored with an observation of the target.
3. `godwit apply` on the pull request: `migrate --rollout expand-contract` binds to the stored plan (or refuses with exit 3 when the target moved) → run ends `awaiting_contract`, step exits 0, `godwit/applied` on the head stays **`pending`** ("expand applied; comment godwit confirm to run the contract phase"); the outcome is posted on the pull request.
4. Deploy the application version that handles both shapes.
5. `godwit confirm` on the pull request (or `run confirm --latest --allow-none` from the deploy pipeline) → the same run resumes with `phase = contract` and ends `succeeded`, `godwit/applied` turns `success` and the pull request becomes mergeable. Merge: `verify` finds every migration applied.
6. If step 4 fails: `godwit revert` on the pull request, or `godwit revert <run-id>`, applies the down side of the expand phase (needs the destructive hazards in the down files acknowledged).

A run whose plan has no contract statements skips `awaiting_contract` and ends `succeeded` directly, so the same pipeline serves additive and destructive changes.
