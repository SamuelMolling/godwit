# 0018 — The App answers where the author is looking

Shipped in #129: the acknowledgement and the refusals. The run report, the Check Runs and the link to live output are the next change, and this record says why they are not this one.

## The open question

The complaint that started this was one sentence: *he commented, the run happened, and nothing appeared on the pull request or in the checks — "nem um aviso"*.

Everything [0016](0016-the-app-is-bound-to-targets-by-the-server.md) and [0017](0017-the-repository-asks-for-a-plan-and-the-binding-permits-it.md) built answers a delivery with an HTTP status and a sentence in its body. That body goes to GitHub's delivery log, which is a page an organisation admin can find under the App's settings and which nobody reads. Every refusal this App can produce — an unbound repository, a commander without write, a `godwit.yaml` that does not parse, a listing too large to trust — lands there and nowhere else.

So the App is silent in both directions. It does not say *I have seen you* when a command arrives, and it does not say *here is why I did nothing* when it refuses. The first is the difference between waiting and wondering; the second is the difference between a bug report and a fix.

## What Atlantis actually does

Read from `main` and its published documentation rather than from memory, because the details matter and two of them are the opposite of what is usually assumed:

- **`--emoji-reaction`**, since v0.29.0: *"The emoji reaction to use for marking processed comments. Currently supported on Gitea, GitHub and GitLab. If not specified, Atlantis will not use an emoji reaction. Defaults to `""`."* It fires in the **webhook controller**, not the command runner — `server/controllers/events/events_controller.go`, under the comment *"It's a comment we're going to react to so add a reaction"*. Its position in the flow is exact and deliberate: **after** the comment has parsed into a known command and **after** `RepoAllowlistChecker.IsAllowlisted`, and **before** any permission check, before the pull request is fetched, and before the command runs. A failure is `logger.Warn` and nothing else. The GitHub implementation is one call to `POST /repos/{owner}/{repo}/issues/comments/{id}/reactions`.
- **Commit statuses, not Check Runs.** This is the one worth stating plainly: Atlantis sets `atlantis/plan` and `atlantis/apply` as **commit statuses**, and `--vcs-status-name` (default `atlantis`) renames them. Using the Checks API is [issue #936](https://github.com/runatlantis/atlantis/issues/936), open since February 2020 and unimplemented; the issue's own argument against it is that the Checks API requires a GitHub App, so every installation would have to register its own. Nothing about Check Runs in godwit is copied from Atlantis, and this record should not be read as if it were.
- **`--atlantis-url`**: *"Specify the URL that Atlantis is accessible from. Used in the Atlantis UI and in links from pull request comments."* This is the flag behind the `https://atlantis.<host>/jobs/<uuid>` links.
- **A new comment every run.** Atlantis does not edit a comment in place. `--hide-prev-plan-comments` (v0.19.0, *"not enabled by default"*) lists the pull request's comments, keeps the ones written by its own user whose first line contains the command name, and collapses each with the GraphQL `minimizeComment` mutation. So the previous report is hidden, never rewritten.

## The decisions

### godwit acknowledges the comment it read, and by default

**A comment godwit reads as a command gets an emoji reaction.** `--github-emoji-reaction`, and unlike Atlantis it **defaults to on** (`eyes`); `none` turns it off.

Atlantis defaults it off because the flag arrived at version 0.29 into an established tool whose users had already built their expectations around its silence, and a tool that suddenly starts marking comments is a tool that changed under them. godwit's App has no installed base to surprise and one recorded complaint that a command looked unread. A default that reproduces the complaint is the wrong default.

**It fires after the association narrows and before the permission lookup.** Atlantis reacts after its repository allowlist and before every permission check; godwit's binding is that allowlist, so the equivalent point is after the binding. godwit then moves it one step later, past the `author_association` filter, to keep the property 0016 wrote down — *a comment nobody could have commanded with spends no GitHub budget*. The association is a field of the signed payload and costs nothing to check, so nothing is lost by checking it first, and a stranger commenting `godwit apply` still causes no API call at all.

**A command that is going to be refused is acknowledged anyway.** That is the point of it. `godwit apply` from someone without write permission takes several API calls to refuse, and during those calls the comment should not look unread. The reaction means *read*, never *accepted* — the same meaning it has in Atlantis.

**A failed reaction is a warning in the log and nothing more.** It is an acknowledgement, not a guarantee; failing a delivery over it would trade a cosmetic problem for a real one.

### Every refusal is said on the pull request

**An outcome a person has to act on becomes a comment.** Concretely: `refused` and `stale`. Not `ignored` — silence is the correct answer to a pull request that changed nothing godwit plans, and 0017 argued that at length. Not `accepted` either: what an accepted command produces is a report about a run, and there is no run yet.

**Exactly one refusal stands, and it is the newest comment on the page.** The previous one is deleted and a new one posted, under the marker `<!-- godwit:refused -->` — the same marker, the same `## godwit <command> refused` heading, and the same delete-then-post mechanism `scripts/action-refuse.sh` uses on the Action's side since #128.

That is not the shape this record first reached for. Editing one standing comment in place is the tempting answer — a refusal *is* a condition rather than an event, and "this repository is bound to no target" holds from the first push until an operator fixes it. #128 got there first and got it right for a reason that beats the tidiness argument: **an edit lands wherever the old comment sits, usually far above the command it answers.** That is the very failure this record exists to correct, and it does not stop being that failure because the message happens to be a refusal. A deleted-and-reposted comment is still exactly one comment; it is simply one the author can see.

**The refusal comment is its own, never the report's.** A refusal must not overwrite a plan or apply report that is still the truth about the pull request, which is why it carries a marker of its own rather than reusing `<!-- godwit:plan -->`.

**And the check the command would have set turns red.** `godwit/plan` for a `plan`, `godwit/applied` for `apply`, `confirm` and `revert` — the same mapping `action-refuse.sh` uses for the commit status it sets, expressed as a Check Run because [0016](0016-the-app-is-bound-to-targets-by-the-server.md) chose Check Runs for the App and refused to set both. A reviewer who never scrolls the conversation still sees it.

**A refusal check is created already concluded**, which is what makes it safe to ship before the runs exist: there is no moment at which a check is open with nothing to close it. A comment whose command godwit could not parse at all sets no check, because godwit does not know which one the author meant; the comment is the whole answer there.

**A failed comment is a warning, like a failed reaction.** The delivery already decided; being unable to say so does not change what was decided, and returning `500` would make GitHub redeliver something that was correctly refused.

### What this costs, and it is not nothing

0016 could say an unbound repository costs nothing at all, and 0017 kept that by checking the binding against the store before any GitHub call. **This record spends that property deliberately**: an unbound repository now costs one installation-token mint and up to two API calls per delivery, because it gets a comment saying it is unbound.

That is exactly what 0016 asked for — *"the first delivery from an unbound repository must produce a refusal a human can act on rather than a dropped event"* — and it is the whole reason the fail-closed default is safe to ship: a default that silently does nothing is indistinguishable from a broken deployment. The cost is bounded by one comment per pull request rather than per delivery, and it makes the per-repository rate limiter 0016 deferred more relevant, not less.

### The comment strategy for run reports, and why the setting is not here

The Action posts **one sticky comment, rewritten in place**, found by `<!-- godwit:plan -->` and friends. That is what the complaint is really about: an edited comment keeps the timestamp and position of the moment it was first posted, so it does not resurface in the timeline, generates no notification, and shows a reader nothing but a small "edited" marker. On a pull request with other bots it sinks and is never seen again. The mechanism is structurally incapable of telling someone that something changed.

**The position: a new comment per report should be the default, with the previous one collapsed.** That is Atlantis's shape with its `--hide-prev-plan-comments` turned on, and the reasoning is that the report's job is to tell a waiting author what happened. A surface that cannot notify cannot do that job, and no amount of tuning fixes it, because it is a property of how GitHub orders and notifies comments rather than of how godwit writes them. The obvious cost of a new comment per push — a timeline that grows — is almost entirely paid off by collapsing the previous one, which GitHub renders as a single grey line.

`sticky` stays available for anyone who prefers a quiet timeline and knows what they are giving up.

**The setting is not implemented here, and that is on purpose.** There is nothing yet that posts a report: the App's entry point still only records what it would have run. A configuration key with no consumer is a promise, not a feature, and it would ship untested against the thing it configures. It lands with the report, in the change that also brings the Check Runs — where it will follow the same shape 0017 set for the trigger: a server default, a per-project override in `godwit.yaml`, and no operator permission required, because how godwit words itself is not a privilege.

### Why the *pending* Check Run is not here

A refusal's check is concluded the moment it is created. The check that goes **pending while work runs** and resolves when it finishes is a different thing, and it is not in this change.

**A check that starts has to be finished by something.** Today the App's whole lifecycle for an accepted command is: receive, decide, record, `202`. Nothing runs afterwards. A `godwit/plan` check created `in_progress` at that moment would never resolve, and a repository that had made it a required check would have every pull request permanently unmergeable — a worse failure than the silence it was meant to cure, and one that would land in production before anyone noticed.

So the pending lifecycle ships with the runs it describes. What that change inherits from here is settled: `godwit/plan` and `godwit/applied` as Check Runs rather than commit statuses (0016), `pending` mapping to `status: in_progress` with no conclusion rather than `conclusion: neutral` (0004's reasoning about a required check that must stay unsatisfied without claiming an error), and no commit statuses alongside them.

The link to live output needs nothing new either: `GODWIT_PUBLIC_URL` already exists and already builds exactly the link required — `internal/notify/slack.go` composes `<public url>/ui/runs/<run id>` for the Slack "Open run" button, and `internal/ui` already serves that page with live run progress. The check's `details_url` is that same string. That is wiring, and it belongs with the run that gives it an id.

## Rejected or deferred

| Thing | Verdict | Reason |
|---|---|---|
| Reacting before the association check, exactly as Atlantis does | refused | It would spend an installation token on any comment from any stranger. The association is a payload field and free; checking it first keeps 0016's budget property whole and delays the acknowledgement by no API call. |
| A second reaction carrying the outcome (`+1` / `-1`) | deferred | Two reactions on one comment is a vocabulary nobody has asked for, and the outcome belongs in the report. Revisit if the report proves too slow to be the answer. |
| Commenting on `ignored` deliveries | refused | 0017 settled that a pull request touching no project is silence. A comment saying "nothing to do" on every push is the noise that makes people stop reading. |
| One refusal comment rewritten in place rather than reposted | refused | It reproduces the complaint. An edit sits where the old comment sat, notifies nobody, and shows a reader an "edited" marker. #128 settled this for the Action first; the App follows it rather than growing a second answer. |
| A second refusal vocabulary for the App | refused | `<!-- godwit:refused -->`, the `## godwit <command> refused` heading and the `godwit/plan` / `godwit/applied` mapping all come from `scripts/action-refuse.sh`. Two paths saying the same thing differently is how the two integrations start to disagree. |
| Failing the delivery when the comment or reaction fails | refused | The decision was already taken correctly; a `500` would make GitHub redeliver it. Both are warnings. |
| Minimising previous comments with the GraphQL `minimizeComment` mutation | deferred | It belongs with the report strategy, and it is the one part of that design that needs a second API surface and a permission this App does not yet ask for. |

## Not verified

- **Which installation permission grants `POST /issues/comments/{id}/reactions`, and `DELETE /issues/comments/{id}`.** The App asks for `Pull requests: write` to comment; whether reactions on a pull request's comments are covered by that or by `Issues: write` was not confirmed against a live installation. If it is the latter, the manifest in [CI/CD](../ci-cd.md#registering-the-app) needs another line and reviewers should know why.
- **That `POST /issues/{n}/comments` is the right endpoint for a pull request conversation comment**, rather than the pull-request-specific review comment endpoint. It is the one the Action already uses through `gh`, so it is the same assumption already in production, not a new one.
- **The `minimizeComment` mutation's permission and behaviour** — deferred with the strategy, and untested either way.
