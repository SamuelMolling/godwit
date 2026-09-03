# CI/CD

Two integrations ship in the repository: a composite GitHub Action (`action.yml` at the root) and ArgoCD hook Jobs (`deploy/argocd/`). Both are thin wrappers over the CLI; anything they do you can do with `godwit` in any runner. Complete workflows and `Application` manifests to copy are in [examples/](../examples/README.md).

The CLI outside GitHub comes from the image `ghcr.io/samuelmolling/godwit` (`main`, `sha-<short commit>`; built by `.github/workflows/publish.yml` on every merge) or, once a `v*` tag exists, from the GitHub release and `brew install SamuelMolling/tap/godwit` (`.github/workflows/release.yml`, GoReleaser).

## Exit codes

| Code | CLI | Meaning |
|---|---|---|
| 0 | all | success; for `migrate`, `apply` and `revert`: the run reached `succeeded` or `awaiting_contract`; for `verify`: every migration is applied |
| 1 | all | error: blocking lint findings, refusal at admission, run `failed` or `needs_attention`, a migration `verify` found pending, a comment command from someone outside `allowed-associations`, connection or usage error |
| 2 | Action only | unknown `command` or `mode`, a command the mode refuses, or an `apply-on` that enables nothing |
| 3 | `migrate`, `apply` | plan stale or required: re-plan on the pull request |

The Action's last step re-exits with the CLI status after the summary, the comment and the status are written, so a failing lint still posts its report.

## GitHub Action

```yaml
- uses: SamuelMolling/godwit@main
  with:
    command: lint | plan | apply | verify | revert | migrate
```

It builds godwit from the checked-out action ref with `actions/setup-go` (`CGO_ENABLED=1`, needs `gcc`), so the first run in a job takes a minute; `go-version` pins the toolchain. The runner also needs `jq` and `gh` (present on GitHub-hosted runners).

### Two modes

The default, `mode: apply-on-pr`, is the Atlantis model: the pull request plans, `/godwit apply` on the pull request applies, and the merge only verifies. `main` never carries a migration the database does not have, because the merge is gated on the `godwit/applied` commit status that only a successful apply sets.

| Event | Command | What happens |
|---|---|---|
| `pull_request` | `lint`, `plan` | lint the new files; store the admitted plan on the service; sticky comments |
| `issue_comment` `/godwit apply`, or `pull_request_review` | `apply` | `migrate` from the pull request head, bound to the stored plan; status `godwit/applied` on the head commit |
| merge (`push`) | `verify` | `migrate --dry-run`: fails when a migration on `main` is not applied; never applies |
| `issue_comment` `/godwit revert` | `revert` | the down files of the run(s) the pull request applied; status back to failure |

In this mode `command: migrate` is refused (exit 2) unless `dry-run: "true"`. `mode: apply-on-merge` keeps the previous flow: `plan` on the pull request, `migrate` on push; `apply` and `revert` are refused there. Use it when nothing may touch the database before the merge (for example when the PreSync hook in [ArgoCD](#argocd) is the only thing allowed to apply).

### Action inputs and outputs

| Input | Default | Used by |
|---|---|---|
| `command` | required | `lint`, `plan`, `apply`, `verify`, `revert`, `migrate` |
| `mode` | `apply-on-pr` | `apply-on-pr` (apply and revert from the pull request, verify on push, migrate refused) or `apply-on-merge` (migrate on push, apply and revert refused) |
| `apply-on` | `comment` | apply: `comment` (a `/godwit apply` comment or review body), `approve` (an approved review), or `comment,approve` |
| `allowed-associations` | `OWNER,MEMBER,COLLABORATOR` | apply, revert: `author_association` values whose comment or review counts; anyone else is refused with exit 1 |
| `dir` | `dir` from `godwit.yaml`, else `migrations` | all but revert |
| `base` | `origin/main` | lint: only migrations added since the ref are linted, files modified since it are `E003`; empty checks every file. The ref is fetched depth-1 when missing |
| `ack` | — | lint, plan, apply, verify, migrate: comma-separated hazard codes; revert: the codes found in the down files (`H002` for `DROP TABLE`, `H009` for `DROP INDEX`, ...) |
| `server` | `server` from `godwit.yaml` or `GODWIT_SERVER` | plan, apply, verify, revert, migrate |
| `token` | — | plan and verify (`read`), apply, revert and migrate (`pipeline`); exported as `GODWIT_TOKEN`, never passed on the command line |
| `target` | `target` from `godwit.yaml` | plan, apply, verify, migrate; revert (optional, narrows the run search). With a target, `plan` runs on the service and stores the plan; without one it parses the files offline |
| `rollout` | `godwit.yaml`, else `direct` | plan, apply, migrate: part of the plan key, so all must agree |
| `dry-run` | `false` | migrate: `PlanRun` without persisting, markdown report, no run (`command: plan` is the persisting variant); allowed in both modes |
| `source` | `<host>/<owner>/<repo>@<pull request head sha, else sha>[:<dir>]` | plan, apply, verify, migrate: provenance stored on the plan (`cp_plans.source`) or run (`cp_runs.source`); revert finds the runs of a pull request by it |
| `comment` | `true` | post the lint, plan, dry-run, apply or revert report as one sticky pull request comment |
| `comment-on-push` | `true` | on `push`, post the migrate outcome, or a failed verify, on the pull request(s) the commit merged |
| `github-token` | `${{ github.token }}` | reads the pull request, posts the comments (`pull-requests: write`) and sets the status (`statuses: write`) |
| `go-version` | `1.26` | build |

| Output | Meaning |
|---|---|
| `run-id` | id of the run created by `apply` or `migrate`, or of the last revert run (empty for dry runs, verify and refusals) |
| `plan-id` | id of the plan stored by `plan` on the service, or bound by `apply` or `migrate` (empty offline and for implicit runs) |
| `plan-key` | key of the plan stored by `plan` (same files, target and rollout give the same key on every push) |
| `stale` | `true` when `apply` or `migrate` exited 3: the stored plan is stale or the target requires one |
| `pending` | number of migrations `verify` found not applied |
| `blocking` | number of blocking lint findings |
| `pr-number` | pull request `apply` or `revert` acted on (or the pull request of a `pull_request` event) |
| `head-sha` | head commit of that pull request: the one checked out, applied and carrying the status |
| `skipped` | `true` when the event carried no command for `apply` or `revert` (a comment that is not `/godwit apply`, a review that does not apply): nothing ran, the step succeeds |
| `summary-path` | file with the markdown report, also appended to the job summary |

Comments are sticky: each is found and replaced by a hidden marker, `<!-- godwit:lint -->`, `<!-- godwit:plan -->`, `<!-- godwit:dry-run -->`, `<!-- godwit:verify -->` or `<!-- godwit:migrate -->` (shared by `apply`, `revert` and `migrate`: one comment tells the story of the run), so each command keeps one comment per pull request. On `pull_request` events lint, plan and dry run post their report; `apply` and `revert` post on the pull request they were commanded from; a real `migrate` never posts there. On `push` events only `migrate` and a failed `verify` post, and they look the pull request(s) up from the commit (`GET /repos/{owner}/{repo}/commits/{sha}/pulls`): the outcome of the merge lands on the pull request that shipped it, with the run id and state, the SQL error, the `PlanStale` report or the list of migrations still pending. A push that merged no pull request posts nothing; a failed comment is a workflow warning, not a failure.

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
      - uses: SamuelMolling/godwit@main
        with: { command: lint }
      - uses: SamuelMolling/godwit@main
        with:
          command: plan
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_READ }}
          target: orders
```

`lint` needs no service. `plan` with a target needs a token with the `read` scope only: it asks the service for the admitted plan against the real target (hazards, out-of-order check, scratch validation, which versions are already applied, which statements would be deferred to contract), stores it with an observation of the target, and posts a `## godwit plan <id>` comment carrying the key, the observation and a `### Changes outside migrations` diff when the target already differs from what the last run left. That stored plan is what the apply binds to ([concepts: plans](concepts.md#plans)); a refusal becomes a `## godwit plan` comment with the reason and a failed step. Without a target the step parses the files offline as before. `dry-run: "true"` on `migrate` gives the same report without storing anything. On `pull_request` events the `source` records the pull request head, not the merge commit GitHub checks out.

Acknowledging a hazard is a code change, visible in the workflow file or in the migration author's `--ack` list, never a click:

```yaml
      - uses: SamuelMolling/godwit@main
        with:
          command: lint
          ack: H003
```

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
      - uses: SamuelMolling/godwit@main
        with:
          command: apply
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
          target: orders
```

Once the review is done, a collaborator comments `/godwit apply` (the whole comment, or one line of it; also accepted as the body of a review). With `apply-on: comment,approve` an approving review applies as well. The step:

1. Reads the event. A comment that is not the command, a review that does not apply, an edited comment or a comment on an issue end the step with `skipped=true` and exit 0. A command from an author whose `author_association` is not in `allowed-associations` is refused (exit 1); a fork's contributor is `CONTRIBUTOR` and cannot apply.
2. Reads the pull request through the API: it must be open, and the checked-out commit must be its head, so the job has to check out `refs/pull/<n>/head` (the default checkout of `issue_comment` is the default branch). If the head moved between the command and the checkout, the step refuses and asks for the command again.
3. Sets the commit status `godwit/applied` to `pending` on the head, then runs `godwit migrate` from the pull request files with the same `dir`, `target` and `rollout` as the plan step, so it binds to the stored plan ([concepts: plans](concepts.md#plans)) and refuses when the target moved since (exit 3, `stale=true`).
4. Posts the `## godwit apply` report on the pull request and sets the status: `success` ("applied by run …; merge when the review is done"), or `failure` with the reason (stale plan → re-plan then command again; SQL error → the run's error is in the comment). The status links to the comment.

The status is per commit, so a push after the apply leaves the new head without one. Branch protection on the base branch should require the `godwit/applied` status and **"Require branches to be up to date before merging"**: the first makes the apply the gate of the merge, the second makes GitHub re-run the pull request workflow (re-plan) when the base moves, so the plan stored last is the one computed on the exact set the pull request applies. The `source` recorded on the run is `github.com/<owner>/<repo>@<head sha>[:<dir>]`, which `godwit runs`, `godwit audit` and `revert` use.

The `apply` step also accepts `pull_request` and `pull_request_target` events (the head from the event; no command or association check, the workflow's own `if` is the gate), for teams that apply on every push to a labelled pull request.

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
      - uses: SamuelMolling/godwit@main
        with:
          command: verify
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_READ }}
          target: orders
```

`verify` runs `godwit migrate --dry-run --json` with a `read` token: the same admission as a plan (hazards, `ack`, scratch validation) plus the list of versions the target has. Exit 0 with `pending=0` when every migration on `main` is applied; exit 1 with the pending list, posted on the merged pull request, otherwise. It never applies: a migration that reached `main` unapplied is fixed by applying it from a new pull request (or by `mode: apply-on-merge`, below).

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
        uses: SamuelMolling/godwit@main
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

With `rollout: expand-contract` the apply (or the merge step in `apply-on-merge`) exits 0 while the run sits in `awaiting_contract`. Confirm from the deploy pipeline once the new application version is out:

```yaml
      - run: godwit run confirm --latest --allow-none --target orders
        env:
          GODWIT_SERVER: https://godwit.internal
          GODWIT_TOKEN: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
```

`--allow-none` makes the step a no-op when nothing awaits (a deploy that shipped no migration). Without it the CLI fails with `target orders: no run awaiting contract`.

## ArgoCD

`deploy/argocd/` has two hook Jobs for an application whose migrations are shipped in a ConfigMap alongside the manifests.

**PreSync** (`presync-job.yaml`): `godwit migrate --target=orders --dir=/migrations --rollout=expand-contract` with the `orders-migrations` ConfigMap mounted at `/migrations`, `GODWIT_SERVER=http://godwit.godwit.svc:8474`, `GODWIT_TOKEN` from Secret `orders-godwit` key `token` (scope `pipeline`). `backoffLimit: 0` (the service retries, the Job must not), `activeDeadlineSeconds: 3600`, `hook-delete-policy: BeforeHookCreation`. Exit 0 on `succeeded` or `awaiting_contract` lets the sync proceed with the schema expanded; exit 1 stops the sync, the run's error is in the Job log and in `godwit run get`; exit 3 means the plan stored on the pull request is stale (or required and missing): the sync stops before any pod changes and the Job log carries the diff. The PreSync run binds to the plan the pull request stored: the ConfigMap holds the same `.up.sql`/`.down.sql` bodies as the repository, and the key is computed from those bodies, the target and the rollout, so it matches as long as the pull request planned with `target: orders` and `rollout: expand-contract`.

**PostSync** (`postsync-confirm.yaml`): `godwit run confirm --latest --allow-none --target=orders`, `activeDeadlineSeconds: 600`. After the new pods are healthy, the contract phase runs. If the sync fails between the hooks, the run stays `awaiting_contract`; the old pods keep working against the expanded schema, and the next successful sync's PostSync confirms it, or an operator reverts it.

Replace `orders`, the Secret name and, for a reproducible hook, the image tag (`ghcr.io/samuelmolling/godwit:main` in the examples; `sha-<short commit>` is immutable); the ConfigMap must contain both `.up.sql` and `.down.sql` files (the CLI loads the directory and rejects a version with a missing side). Runs created by these Jobs carry `created_by = <token name>` and an empty `source` unless `--source` is added to the args.

## Expand → contract, end to end

1. Author the change as two migrations: an additive one (`CREATE`, `ADD COLUMN`, `CREATE INDEX CONCURRENTLY`) and a later destructive one (`DROP`, `RENAME`: H002/H003/H008). The split is per migration: every plan up to the first one carrying a contract hazard runs in expand, that plan and everything after it wait for contract. A single file mixing both lands in contract whole.
2. Pull request: `lint` (hazards acknowledged where intended), `plan` with a read token: the admitted plan is stored with an observation of the target.
3. `/godwit apply` on the pull request: `migrate --rollout expand-contract` binds to the stored plan (or refuses with exit 3 when the target moved) → run ends `awaiting_contract`, step exits 0, status `godwit/applied` on the head; the outcome is posted on the pull request. Merge: `verify` finds every migration applied.
4. Deploy the application version that handles both shapes.
5. `run confirm --latest --allow-none` → contract phase runs, run ends `succeeded`.
6. If step 4 fails: `godwit revert <run-id>` applies the down side of the expand phase (needs the destructive hazards in the down files acknowledged).

A run whose plan has no contract statements skips `awaiting_contract` and ends `succeeded` directly, so the same pipeline serves additive and destructive changes.
