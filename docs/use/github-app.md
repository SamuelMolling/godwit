# Run migrations with the GitHub App

The App route puts godwit in the pull request without putting it in your CI: GitHub posts deliveries to the
godwit service, and the service reads the migrations, plans them, applies them and answers on the pull
request itself. No runner, no workflow file, no token to hand a job — the credentials live in the service and
nothing is copied into the repository.

It is one App per godwit service, installed on as many repositories as you like. Each repository then only
needs a `godwit.yaml`, and an operator to bind it to a target.

## What must exist first

- A godwit service GitHub can reach over HTTPS, with a target registered on it
  (`godwit target add app --provider static --dsn …`).
- The App's webhook listener turned on. It is a listener of its own, not a path on the API:

```bash
godwit serve \
  --store-dsn postgres://godwit@db/godwit_store \
  --listen :8474 \
  --github-webhook-addr :8475
```

`--github-webhook-addr` (or `GODWIT_GITHUB_WEBHOOK_ADDR`) serves `/github/webhook` and answers 404 everywhere
else. Leaving it empty leaves the App off. The first three of these are required, and start-up fails without
them:

| Variable | What it is |
|---|---|
| `GODWIT_GITHUB_WEBHOOK_SECRET` | the webhook secret you set on the App; every delivery is verified against it before anything else is read |
| `GODWIT_GITHUB_APP_ID` | the App ID from its settings page |
| `GODWIT_GITHUB_PRIVATE_KEY` or `GODWIT_GITHUB_PRIVATE_KEY_FILE` | the App's private key, PEM (PKCS#1 or PKCS#8). Setting both is refused. The file form has a flag, `--github-private-key-file`; the inline form deliberately has none, so the key cannot reach the process arguments |
| `GODWIT_GITHUB_API_URL` | only for GitHub Enterprise Server; defaults to `https://api.github.com` |

`GODWIT_PUBLIC_URL` is worth setting too: it is what godwit links its plans and runs at from the pull request.

## Creating the App

GitHub → Settings → Developer settings → GitHub Apps → New GitHub App.

- **Webhook URL**: `https://<the host you published :8475 at>/github/webhook`
- **Webhook secret**: the same string as `GODWIT_GITHUB_WEBHOOK_SECRET`. godwit will not start without one, and
  answers `401` to any delivery whose `X-Hub-Signature-256` does not match the body it received.
- **Content type**: `application/json`. The receiver parses the delivery body as the GitHub JSON payload; a
  form-encoded delivery is answered `400`.
- Uncheck **Webhook → Active** only if you want the App off; there is no other switch.

Then generate a private key, note the App ID, and install the App on the repositories that hold migrations.
The installation is what godwit mints a token against, scoped to the one repository the delivery came from.

## Permissions

Repository permissions, all of them read-only except the three that write back to the pull request. godwit
never pushes, so it needs no write access to **Contents**, and it needs no **Webhooks** permission — the
deliveries come to the App's own URL.

| Permission | Access | What godwit does with it | What breaks without it |
|---|---|---|---|
| **Metadata** | Read | reads the repository permission of the person who commanded (`GET /repos/{owner}/{repo}/collaborators/{login}/permission`), and of the reviewer whose approval an apply is standing on | nothing can be authorised. Every commanded delivery ends in a `500` godwit logs as `webhook delivery failed`, and GitHub shows it as a failed delivery. GitHub grants this on every App, so in practice you cannot get it wrong |
| **Contents** | Read | reads `godwit.yaml` at the pull request head, lists the migration directory, and downloads each migration body | no plan and no apply. The pull request gets a refusal naming the file godwit could not read — and because a repository that may not be read looks the same as one that has no file, the refusal can read as *"carries no `godwit.yaml`"* when the truth is that the permission is missing |
| **Pull requests** | Read | the head SHA, state, merged flag and file count (`GET /pulls/{n}`); which files changed, to decide which bound projects the pull request touches (`/pulls/{n}/files`); the reviews, to find the standing approval `apply` and `confirm` require (`/pulls/{n}/reviews`) | every delivery fails with `500` before anything runs, and nothing is posted on the pull request |
| **Pull requests** *or* **Issues** | Write | posts the plan and run reports on the pull request, and deletes the report it posted last under the same marker so exactly one stands (`POST`/`GET /issues/{n}/comments`, `DELETE /issues/comments/{id}`). GitHub accepts either permission for these | the report never reaches the pull request. A command that got as far as opening a check still says so *in the check*, ending with `godwit could not post this on the pull request, so it stands only here: …`. A refusal, which is decided before any check exists, is posted nowhere at all — it survives only as a `could not post the refusal on the pull request` line in the service log |
| **Issues** | Write | the 👀 reaction godwit puts on the comment it read as a command. A reaction on a pull request comment is an *issue comment* reaction, and GitHub accepts **no Pull requests alternative** for it — this is the one place where reacting and commenting do not draw on the same permission | the command still runs, but silently: the comment is never acknowledged and the only trace is `could not react to the commanding comment` in the service log. If you would rather not grant Issues write, run with `--github-emoji-reaction none` and godwit will not attempt the reaction |
| **Checks** | Write | opens a check run `in_progress` on the head commit before it starts and concludes it with the report (`POST /check-runs`, `PATCH /check-runs/{id}`) | no `godwit/plan` or `godwit/applied` check appears, so branch protection has nothing to require and refusals leave no mark on the commit. The command itself still runs and still comments; godwit logs `could not open the check` |

The check is named `godwit/plan` for a plan and `godwit/applied` for `apply`, `confirm` and `revert`. When one
pull request plans against more than one target, the target is appended: `godwit/applied (payments)`.

## Events

Subscribe to exactly three. godwit ignores every other delivery with `godwit does not act on <event> deliveries`.

| Event | What godwit does with it |
|---|---|
| **Pull request** | on `opened`, `synchronize`, `reopened` and `ready_for_review`, plans by itself. Every other action is ignored |
| **Issue comment** | on `created` only: the comment is read as a command. An edit is not — *a command is what someone posted, not what a comment now says* |
| **Pull request review** | on `submitted`: the review body is read as a command, the same way a comment is |

## `godwit.yaml` in the repository

The App reads this file at the pull request head, in the directory the repository is bound at. It is the same
file the CLI reads, but the App only looks at these keys:

```yaml
target: app                 # required; the target on the service this project migrates
dir: db/migrations          # default: migrations. Relative to this file, and it may not leave its directory
rollout: expand-contract    # default rollout for this project: direct or expand-contract
allow_out_of_order: false   # accept a migration whose version sorts below one already applied
plan:
  format: schema            # schema (default) or statements
autoplan:
  enabled: true
  when_modified:
    - "db/seeds/**/*.sql"
```

A file with no `target` is skipped with *"names no target, so godwit has nothing to plan it against"*. Any key
the CLI understands but the App does not (`server`, `lock_timeout`, `statement_timeout`, `schema_source`) is
accepted and ignored here; unknown keys are a parse error and the project is skipped with the parser's message.

**`when_modified` widens, it never replaces.** godwit always watches `<dir>/**/*.sql` and `godwit.yaml`, and
`when_modified` adds to that list — a project cannot configure its own migrations out of being planned.
`autoplan.enabled: false` turns off only the plan godwit starts by itself on a push; `godwit plan` in a comment
still works.

## Binding the repository to a target

Nothing in the repository can grant itself a target. An operator binds them on the service:

```bash
godwit target add app --github-repo myorg/api
```

`--github-repo` takes `owner/repo` or `owner/repo:dir`, is repeatable, and **replaces the whole list** when
passed. The `:dir` form binds one subdirectory of a monorepo: `--github-repo myorg/platform:services/billing`
means godwit reads `services/billing/godwit.yaml` and only considers files under `services/billing/`. A
repository bound with no `:dir` may use any directory in it.

An unbound repository gets nothing — not an apply and not a plan:

> repository myorg/api is bound to no godwit target, so it gets nothing here — not an apply and not a plan;
> ask a godwit operator to bind it (`godwit target add <target> --github-repo myorg/api`)

A repository bound to target `app` whose `godwit.yaml` names target `billing` is refused the same way a
repository bound to nothing is, and with the same message: telling the two apart would hand a repository the
list of targets it holds no token for.

## The commands

The command must be the **whole comment**, backticks stripped. Anything above or below it on another line and
godwit stays quiet — quoting a command in a review, or writing "to deploy, comment: godwit apply", does not
fire one. `/godwit apply` works as an alias.

| Comment | What it does |
|---|---|
| `godwit plan` | re-plans the pull request head against the live target and stores the plan |
| `godwit plan --rollout expand-contract` | plans under a rollout other than the project's. `direct` and `expand-contract` are the only two values |
| `godwit apply` | creates the run, bound to the stored plan for this set |
| `godwit apply --ack H001,H003` | applies with those hazards acknowledged. Without the ack the run is refused at admission and the codes are named in the report |
| `godwit confirm` | releases the contract phase of the run this pull request left in `awaiting_contract` |
| `godwit revert` | undoes what a run of this pull request applied |
| `godwit revert --ack H001` | reverts with those hazards acknowledged |

`--allow-data-loss` and `--force` are **refused from a comment**. Both remove a gate — the first the one
that refuses a revert dropping a table or column still holding rows, the second the one that refuses a run
that is not the newest un-reverted one on its target — and a comment is not where a gate is removed. Take
those reverts on the CLI or the UI, with a token ([decision 0025](../decisions/0025-a-revert-from-a-comment-is-held-to-the-same-people-as-the-apply.md)).

Every command also takes a commit SHA as its first argument — `godwit apply 4f2a9c1` — which refuses if the
pull request has moved on since:

> godwit apply names 4f2a9c1 but pull request #42 is at 9b31f07: the head moved after the comment

A flag a command does not take is refused with its own usage, not ignored:
`godwit confirm --ack H001` answers *"godwit confirm does not take --ack (want 'godwit confirm' or
'godwit confirm <sha>')"*.

## Who may command

Three things are checked, in this order, before anything runs:

1. **Author association.** `OWNER`, `MEMBER` and `COLLABORATOR` by default; narrow it with
   `--github-allowed-associations`. `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, `FIRST_TIMER`, `MANNEQUIN` and
   `NONE` are refused outright as values — anyone who opened a pull request carries one, so they are not access
   to anything. This check runs before godwit spends a single API call, so an idle comment costs nothing.
2. **Repository permission.** The commander must hold `write` or `admin`. An association is a label GitHub puts
   on a comment; the permission is the thing that means something.
3. **A standing approval**, for `apply`, `confirm` and `revert`. GitHub must report an approving review by
   someone who *also* holds write or admin. Each reviewer's latest review counts, so a later
   `CHANGES_REQUESTED` or a dismissal supersedes that reviewer's earlier approval.

`plan` needs no approval. `revert` does: it is the one command that destroys, and the forward path needing
two people while the path that undoes needs one is the wrong way round. In the ordinary case the approval
that permitted the apply is still standing and nothing more is asked for. What the flags would remove is
refused outright rather than approved — see the note under the command table, and
[decision 0025](../decisions/0025-a-revert-from-a-comment-is-held-to-the-same-people-as-the-apply.md).

Beyond that:

- **A fork never reaches a target.** A pull request whose head is in another repository is refused for every
  command, and is not even planned automatically.
- `plan`, `apply` and `confirm` need the pull request open. `revert` does not, because a pull request applied
  and then closed unmerged is exactly when one is wanted; it refuses once the pull request is merged — the
  migrations belong to the base branch then, and reverting them is a new pull request.
- A review command is anchored to the commit it was submitted on: if the head moved after the review, the
  command would run commits nobody reviewed, so it is refused.
- A delivery older than `--github-webhook-max-age` (one hour by default) is refused as a replay.
- If the head moves between godwit accepting a command and carrying it out, godwit drops it and leaves it to
  the delivery that moved the head. Nothing is posted.

A command that reaches the service through a webhook holds at most the `pipeline` scope, so no comment on any
pull request can reach the RPCs that register a target or a credential store.

## What you see on the pull request

- **One comment per report, replaced in place.** The plan report lives under a `<!-- godwit:plan -->` marker
  and the run report under `<!-- godwit:migrate -->`; posting a new one deletes the old one first, so the
  thread never fills with stale plans. With two or more projects in one pull request, the marker carries the
  target so neighbouring reports do not delete each other.
- **A check run per command**, opened before the work starts with *"godwit is reading the migrations of
  `db/migrations` at 4f2a9c1"* and concluded with the report. A plan that found hazards concludes
  `action_required` rather than `failure` — it is a thing to look at, not a thing that went wrong.
- **The run reports itself later.** An apply returns as soon as the run is queued; a poller then posts the
  outcome under the same marker when the run finishes. A run that ends in `awaiting_contract` concludes its
  check as *"expand applied; comment godwit confirm to run the contract phase"*.

## Limits worth knowing

GitHub lists at most 3000 files for a pull request and at most 1000 entries for a directory, and says nothing
when it truncates either. godwit refuses rather than guesses:

- A pull request over the file cap is refused for every command — *"godwit cannot tell which projects this pull
  request touches and will not guess"*. Land the migrations in a pull request of their own.
- A migration directory over the entry cap is refused with the count GitHub answered. That is the point at
  which a checkpoint is overdue.
- A pull request carrying more reviews than godwit reads is refused for `apply` and `confirm`: GitHub lists the
  oldest first, so the reviews that *withdraw* an approval are exactly the ones a short read would miss.

## What a delivery is answered with

This is what the App's page shows in *Recent Deliveries*, in the order godwit decides it — the signature is
checked before anything else about the request, so a malformed request with a bad signature is a `401` and
says nothing more.

| Answer | When |
|---|---|
| `413`, empty | the body is over `--github-webhook-max-bytes` |
| `400`, empty | the body could not be read |
| `401`, empty | the signature is missing, malformed or wrong. Nothing but the byte count is learned from such a request |
| `405` | the signature verified, but the request is not a `POST` |
| `400` | no `X-GitHub-Delivery`, or a body that is not the GitHub JSON payload |
| `202 accepted` | verified, authorised, recorded — the command itself runs behind the answer |
| `202` with a reason | ignored (an event or action godwit does not act on, a comment that names nothing, a pull request no bound project plans), refused (unbound, unauthorised, stale, a fork, a listing godwit could not read the whole of), or a duplicate delivery id |
| `500` | the store or GitHub could not be reached, or the worker queue had no room. Nothing was recorded, so GitHub's redelivery is a fresh attempt |

**The command runs after the delivery is answered.** GitHub wants a webhook answered in seconds and a plan
builds scratch databases, so the delivery is recorded, answered `202`, and carried out by `--github-workers`
workers (2 by default) behind it. The queue is in memory: a replica that dies between the `202` and the plan
loses that command, the check stays open, and the way out is to comment `godwit plan` again — nothing ran.

Every delivery increments `godwit_webhook_deliveries_total{event,result}`.
