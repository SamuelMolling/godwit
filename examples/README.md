# Examples

Ready-to-copy pipelines. Every input and flag matches `action.yml` and the CLI on `main`; values marked with an inline comment are the ones to replace. The mechanics (exit codes, comment marker, expand → contract) are in [docs/ci-cd.md](../docs/ci-cd.md).

## GitHub Actions

Copy into `.github/workflows/` of the repository that holds the migrations.

| File | Trigger | What it does | Token scope |
|---|---|---|---|
| [pr-plan-and-apply.yml](github-actions/pr-plan-and-apply.yml) | pull request; `/godwit apply`, `/godwit confirm` or `/godwit revert` comment; review | job `check`: `lint` the migrations added since `origin/main`, `plan` against the live target stored on the service; job `apply`: `/godwit apply` (or an approving review with `apply-on: comment,approve`) applies the stored plan from the pull request head and sets the `godwit/applied` status, `/godwit confirm` runs the contract phase an `expand-contract` apply held back (until it does the status stays `pending`), `/godwit revert` runs the down files when the pull request is abandoned | `read` (plan), `pipeline` (apply, confirm, revert) |
| [push-verify.yml](github-actions/push-verify.yml) | push to `main` | job `verify`: `migrate --dry-run` proves every migration on `main` is applied, fails the push and comments on the merged pull request otherwise; job `contract`: `run confirm --latest --allow-none`, gated by a GitHub environment with required reviewers | `read` (verify), `pipeline` (contract) |
| [pr-dry-run.yml](github-actions/pr-dry-run.yml) | pull request | non-persisting variant of the plan step: `migrate --dry-run` against the live target shows the same admitted plan without storing it; sticky comment | `read` |
| [apply-on-merge.yml](github-actions/apply-on-merge.yml) | push to `main` | opt-in `mode: apply-on-merge`: job `expand`: `migrate --rollout expand-contract` bound to the plan the pull request stored, outcome posted on the merged pull request; job `contract` as above | `pipeline` |
| [pr-prisma-diff.yml](github-actions/pr-prisma-diff.yml) | pull request touching `prisma/schema.prisma` | job `diff`: `diff --prisma` renders the Prisma schema with the project's Prisma CLI, writes the migration from the live target to it and commits the pair to the pull request branch, then `lint` and `plan` on the result; pair with pr-plan-and-apply.yml for `/godwit apply` | `read` |

Branch protection for the default mode: require the `godwit/applied` status and "Require branches to be up to date before merging" on `main`; the status is set on the pull request head, so any push after the apply needs `/godwit apply` again.

`SamuelMolling/godwit` is pinned to a commit in every example, not `@main`: the apply job holds a `pipeline` token, and a moving ref means whoever can push to this repository runs DDL on your database at your next apply. Replace the sha with the commit you reviewed, and pin the other actions the same way. Who may command an apply — repository permission, and an approving review standing on the exact commit — is in [docs/ci-cd.md](../docs/ci-cd.md#who-may-command-an-apply).

The action builds godwit from source with cgo (`go-version` pins the toolchain), so the first step of a job takes about a minute.

## GORM schema package

[gorm/schema/main.go](gorm/schema/main.go) is what `godwit diff --gorm ./cmd/schema` runs: GORM's migrator in `DryRun` mode with a logger that prints the SQL, so the model structs render to DDL without a database. Copy the file into your own project (it carries its own `go.mod` only to stay out of godwit's build), replace `Order` with your models, and point `--gorm` or `schema_source.path` at the package.

## ArgoCD

[argocd/](argocd/README.md): an `Application` for the service (Helm chart in `deploy/helm/godwit`) and one for an application whose chart carries the PreSync / PostSync hook Jobs from [deploy/argocd](../deploy/argocd/README.md).
