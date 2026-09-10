# 0017 — The repository asks to be planned, the binding says which target it may be planned against

Shipped in #127. It decides *when* the App looks at a project and *which* project it is; it still does not fetch the migrations or write a Check Run.

## The open question

With the Action, a consuming repository decides when godwit runs by writing a `paths:` filter in its workflow:

```yaml
on:
  pull_request:
    paths: ["db/migrations/**"]
```

[0016](0016-the-app-is-bound-to-targets-by-the-server.md) removed the workflow. Nothing then decides: every `synchronize` on every bound repository would plan, so a pull request touching only JavaScript would replay DDL on a scratch database and post a report about nothing. The `paths:` filter has to move to the server, and the moment it does it stops being a repository's private business — the server is now spending its own installation's API budget and its own scratch databases on it.

Atlantis has answered this exact question, and the answer is not the obvious one.

## What Atlantis actually does

Read rather than remembered, from `main` at the time of writing:

- **`autoplan: {enabled, when_modified}` on each project in `atlantis.yaml`.** `when_modified` is a list of `.dockerignore` globs, **relative to the project's `dir`** and joined onto the project dir before matching (`server/events/project_finder.go`: *"Prepend project dir to when modified patterns because the patterns are relative to the project dirs but our list of modified files is relative to the repo root"*). `enabled` defaults to `true`; `when_modified` defaults to `["**/*.tf*", "**/*.tofu", "**/*.tofu.json", "**/terragrunt.hcl", "**/.terraform.lock.hcl"]` (`server/core/config/raw/autoplan.go`). The defaults apply per field, so `autoplan: {enabled: false}` still carries the default globs — which matters, because a manually commanded plan still uses them.
- **A custom `when_modified` replaces the defaults entirely.** The docs say so, and they say it is the most common self-inflicted wound: a project that lists `../modules/**/*.tf` and nothing else stops planning when its own `.tf` files change.
- **`when_modified` is *not* server-gated.** `allowed_overrides` in the server-side `repos.yaml` covers `apply_requirements`, `workflow`, `plan_requirements`, `import_requirements`, `delete_source_branch_on_merge`, `repo_locking`, `repo_locks`, `custom_policy_check` and `silence_pr_comments`; `ValidateRepoCfg` in `server/core/config/valid/global_cfg.go` checks exactly those fields, and `Autoplan` never appears. `MergeProjectCfg` copies the repo's `when_modified` through unconditionally. **Any repository that can put an `atlantis.yaml` in a pull request sets its own trigger, with no operator opt-in.**
- **Every matching project is planned, with no cap** — only `--parallel-pool-size` (15) bounds concurrency, and only when parallel plans are on.
- **A pull request matching nothing is silent on the autoplan path** and *not* silent on the comment path. `runAutoplan` returns early with `0/0` commit statuses and never calls the comment writer; a commented `atlantis plan` renders the zero-project template and posts the literal `Ran Plan for 0 projects:`. `--silence-no-projects` suppresses the latter.
- **Changing `atlantis.yaml` itself triggers nothing.** It matches neither the default `when_modified` nor the default `--autoplan-file-list`, and there is no special case for it. That is source-derived; no document states it.

The lesson is the one that is easy to get backwards. Atlantis does not gate the trigger, because **the trigger is not the privilege**. What `allowed_overrides` protects is what a plan is allowed to *do* — which workflow runs, what an apply requires. When it happens is the repository's business.

## The decisions

### A project is a bound directory, and the binding is the whole permission

0016 put a repository allowlist on the target: `github_repositories`, entries `owner/repo` or `owner/repo:dir`. That `dir` is now given its precise meaning: **it is the directory holding `godwit.yaml`** — the project root — not the migration directory, which `dir:` inside that file names relative to it. A bare `owner/repo` is a project at the repository root.

So the two mechanisms compose exactly as Atlantis's do, with the same split:

| | Atlantis | godwit |
|---|---|---|
| Repository asks | `atlantis.yaml`: projects, `when_modified` | `godwit.yaml`: `target`, `dir`, `autoplan.when_modified` |
| Server permits | `repos.yaml`: `allowed_overrides`, workflows | the binding: which targets this repository may reach, from which root |
| Not gated | when a plan happens | when a plan happens |
| Gated | what a plan may do | which database a plan may touch |

**A repository declares its own trigger and needs no operator permission for it**, for Atlantis's reason. A repository that widens `when_modified` to `**` gets more plans of *its own* target and cannot reach another: the target its `godwit.yaml` names is checked against the binding on every delivery, which is where `bindings.grant` — written in 0016 with nothing calling it — finally runs.

**A repository cannot declare a project.** This is the sharp difference from Atlantis, where `atlantis.yaml` lists the projects. Here the roots come from the binding and nowhere else, so `godwit.yaml` in a directory nobody bound is a file the server never reads.

### The default trigger is the migrations and the file that says where they go

`when_modified` defaults to `["<dir>/**/*.sql", "godwit.yaml"]`, relative to the project root, with `<dir>` read from the same file (`migrations` when it says nothing). It is the direct analogue of `**/*.tf*`: a change to `db/migrations/20260101_a.up.sql` plans, a change to `src/app.js` does not, and neither does a `README.md` sitting beside the migrations.

**`godwit.yaml` is in the default, where `atlantis.yaml` is not in Atlantis's.** Atlantis could not put it there — its config is at the repository root, outside every project's `dir`, so a project-relative default cannot name it. godwit's is *at* the project root, and it carries `target` and `rollout`: changing it changes what an apply would do, so it deserves a re-plan.

### `when_modified` widens the default, it does not replace it

This is the one place the shape is deliberately not Atlantis's, and the reason is the wound Atlantis documents. A godwit project that stops planning when its own migrations change is not expressing a preference, it is broken — the plan exists to describe those files. So a project's patterns are **added** to the default. The only way to stop a project planning is `autoplan.enabled: false`, which says so.

### Patterns stay inside the project root

A `when_modified` pattern that begins with `/` or contains `..` is refused, and so is a `dir:` that does. Atlantis allows and documents `../modules/**/*.tf`, because a Terraform project genuinely depends on modules that live outside it. A migration directory does not: it is SQL, and it is under the file that names it.

Containment buys something concrete. The server can decide, from the changed-path list alone, that a pull request touched nothing under a project's root — and then skip that project without reading its `godwit.yaml` at all. A monorepo with twenty bound projects costs one Contents call for the one project a pull request touched, not twenty. Allowing `../` would make that skip unsound and put the cost back.

It also avoids the footgun Atlantis has here: `../` from a repository-root project resolves above the repository, matches nothing, and is reported nowhere. godwit refuses it in words instead.

### Several projects, and none

**Several bound projects touched: all of them are planned**, as in Atlantis, with no cap. The bound set is the cap, and an operator sets it. They are ordered by target name so a report reads the same twice.

**None touched, from a pull request: silence.** No plan, no comment, nothing — the same choice Atlantis makes in `runAutoplan`, and the whole point of having a trigger at all.

**None touched, from a comment: a refusal.** A person wrote `godwit apply` and is owed an answer; Atlantis posts `Ran Plan for 0 projects:` for the same reason. A project that is skipped for a reason — no `godwit.yaml`, one that does not parse, one naming a target the binding does not carry — reports that reason on both paths, because it is a mistake somebody has to fix rather than a pull request that simply changed nothing.

**`enabled: false` stops the automatic plan, not a commanded one.** Atlantis's `when_modified` "will continue to work for manually run plans even when autoplan is disabled", and the same reading applies: *auto*plan is the thing being disabled.

## What this costs

- **An autoplan is no longer free.** 0016 could say a `pull_request` event spent no GitHub call at all; it now costs one call for the changed files and one Contents call per project the pull request touched. The binding check still runs first, against the store, so an unbound repository still costs nothing.
- **`godwit.yaml` is now parsed in the server process, from an arbitrary head sha**, which 0016 listed as a consequence to live with and this record makes real. It is bounded — 64 KiB, one file per touched project — and the parse is the same strict one the CLI uses, unknown keys included.
- **A repository can make its own project plan more often than it needs to.** That is the trade Atlantis makes and the reason is the same: the cost lands on the repository that asked for it, and the binding is what stops it landing anywhere else.
- **`dir` in a binding entry changed meaning** from the migration directory to the project root. 0016 is not merged, so nothing in the wild carries the old reading, but the two records must be read together.

### A listing godwit cannot read the whole of decides nothing

`GET /pulls/{n}/files` answers with **at most 3000 files**, paginates to that limit and then simply stops. There is no flag, no error and no last page marker: a caller that follows the `Link` header to exhaustion gets a short list that looks complete.

Read naively, that is the failure this whole design exists to avoid. A pull request with 3200 files, one of them a migration, would list the first 3000, match no project, and be answered with silence. The author would see godwit say nothing, the reviewer would merge believing no migration was involved, and the database would be behind the branch with nothing anywhere recording it. Rarity is not a defence: the outcome is a wrong answer that looks like a right one.

**So godwit checks whether the listing is whole, and refuses when it is not.** Two signals, either of which is enough:

- the read stopped at the cap, which is a listing godwit truncated itself;
- fewer entries came back than `changed_files` on the pull request — GitHub's own count of what it changed, which arrives in the signed `pull_request` payload for free and from `GET /pulls/{n}` on the comment path.

The refusal names both numbers and says what to do. It applies to every command, not only the automatic plan: an `apply` decided from a partial listing would apply the projects godwit happened to see and silently skip the ones it did not, which is the same wrong answer wearing a different hat.

*Rejected: planning every bound project when the listing is partial.* It preserves the invariant for `plan`, which is read-only, and breaks it for `apply` — a `godwit apply` would then run migrations against every target the repository is bound to on the strength of no evidence at all. An asymmetry between the read path and the write path is exactly the kind of rule that gets applied to the wrong one later.

*Rejected: planning the projects the partial listing did match.* A positive match from a partial list is sound on its own, but the projects it did **not** match are still undecided, so the answer is right about some targets and silently wrong about others. Partial knowledge is what is being refused; acting on part of it is not a smaller version of the same thing.

*Deferred: falling back to a tree comparison.* `GET /git/trees/{sha}?recursive=1` carries an explicit `truncated` flag and holds 100,000 entries, so diffing the merge base against the head would answer correctly where the file listing cannot. It is the better outcome and it is not taken here, because it is four more calls of new code on a path that fires for perhaps one pull request in ten thousand, resting on three API behaviours nothing in this repository can exercise. An untested fallback on a path nobody walks is a liability pretending to be a fix; a refusal that is provably correct is worth more until there is a reason to spend that complexity.

**The review listing has the same shape and the opposite danger.** `GET /pulls/{n}/reviews` has no documented cap, and godwit follows its pages to exhaustion — but an unbounded read is its own problem, and GitHub returns reviews **oldest first**, so anything a truncation drops is the newest: precisely the `CHANGES_REQUESTED` and `DISMISSED` entries that withdraw an approval. Where a short file listing under-reports and fails closed on its own, a short review listing would fail *open*. godwit therefore stops at 3000 reviews and refuses the approval check rather than deciding it from a prefix of the record.

## Where the App stops, and why `godwit diff` is not here

"I changed a Go model — how does godwit know to write a migration?" is a different question with a different answer, and it does not belong to the App.

Deriving a desired schema from an ORM model means **running the repository's own toolchain**: compiling a Go package, executing `npx prisma`, running `manage.py`. [0003](0003-orm-schema-sources.md) made that execution deliberate and put it in a checkout. A central App would have to build and run arbitrary code from any repository in its installation, inside the process that holds every target's credentials — the exact surface 0016 exists to remove. There is no version of that which is safe enough to be worth it.

So:

- **`godwit diff` and `lint`'s `E005` stay Action-only.** A team that generates migrations from Prisma or GORM keeps a workflow for that step. It needs no `pipeline` token: `diff` and `lint` are `read`.
- **A schema-source change is not a trigger.** `prisma/schema.prisma` moving is not something the App can plan, so it does not plan it. A project may name it in `when_modified` if it wants the migrations re-planned when it moves, and that is a choice, not the default.
- **The App's job starts at the committed migration.** Once `godwit diff` has written `20260101000000_add_column.up.sql` and the author has pushed it, the App is what plans it, applies it and records it.

The two halves meet in the pull request, which is where they were always going to.

## Rejected or deferred

| Thing | Verdict | Reason |
|---|---|---|
| `when_modified` replacing the default, as in Atlantis | refused | A project that stops planning its own migrations is broken, not configured. Widening only. |
| `../` patterns, as Atlantis documents for shared modules | refused | Migrations do not live outside the file that names them, and containment is what lets the server skip an untouched project without reading it. |
| A server-side `allowed_overrides` for the trigger | refused | Atlantis does not gate `when_modified` either, and for the right reason: the trigger says when godwit looks, the binding says what it may touch. Gating both would be one mechanism too many. |
| A repository declaring its own projects | refused | Roots come from the binding. This is the whole fail-closed default of 0016 and it does not get an exception here. |
| A cap on projects planned per pull request | deferred | Atlantis has none, and the binding already bounds it. Revisit with a real monorepo. |
| `!` negation in `when_modified` | deferred | `.dockerignore` has it and Atlantis supports it. Nothing has asked, and order-sensitive negation is a thing to get wrong. |
| A pattern matching a parent directory matching everything under it (Atlantis's `MatchesOrParentMatches`) | refused | Surprising. `db/migrations` matches that path and nothing else; `db/migrations/**` is how you say the rest. |
| Autodiscovering projects from changed paths, as Atlantis does without an `atlantis.yaml` | refused | It would mean planning a directory nobody bound. |

## Not verified

- **Atlantis's behaviour is quoted from `main` and its published docs, not run.** The two claims most worth re-checking if this record is ever leaned on: that `allowed_overrides` does not cover `autoplan` (read in `global_cfg.go`, and the docs' own table is out of date on that field's contents), and that a `pull_request` matching no project posts nothing (read in `plan_command_runner.go`).
- **The Contents API's shape for a small file.** The reader asks for the JSON form, expects `type: file` and `encoding: base64`, and refuses anything else — including the `encoding: none` GitHub returns for a blob over 1 MiB, which a 64 KiB cap should make unreachable. Not exercised against a live repository.
- **What a rename costs.** `previous_filename` is treated as changed alongside `filename`, so moving a migration out of a directory triggers the project it left. That is what the API documents; not tested live.
- **That `changed_files` on a pull request is the true count** and does not itself saturate on a very large pull request. It is the second of the two truncation signals; the cap check stands alone if it turns out to be unreliable.
- **That `GET /pulls/{n}/reviews` really is uncapped.** godwit bounds it at 3000 itself and refuses past that, so the assumption cannot produce a wrong answer either way — only an unnecessary refusal on a pull request with three thousand reviews.
