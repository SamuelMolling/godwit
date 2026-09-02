# Examples

Ready-to-copy pipelines. Every input and flag matches `action.yml` and the CLI on `main`; values marked with an inline comment are the ones to replace. The mechanics (exit codes, comment marker, expand → contract) are in [docs/ci-cd.md](../docs/ci-cd.md).

## GitHub Actions

Copy into `.github/workflows/` of the repository that holds the migrations.

| File | Trigger | What it does | Token scope |
|---|---|---|---|
| [pr-lint-and-plan.yml](github-actions/pr-lint-and-plan.yml) | pull request | `lint` the migrations added since `origin/main`; `plan` against the live target, stored on the service as the plan the merge will apply; each posts a sticky comment | `read` (plan) |
| [pr-dry-run.yml](github-actions/pr-dry-run.yml) | pull request | non-persisting variant: `migrate --dry-run` against the live target shows the same admitted plan without storing it; sticky comment | `read` |
| [deploy-migrate.yml](github-actions/deploy-migrate.yml) | push to `main` | job `expand`: `migrate --rollout expand-contract` bound to the plan the pull request stored, outcome posted on the merged pull request; job `contract`: `run confirm --latest --allow-none`, gated by a GitHub environment with required reviewers | `pipeline` |

The action builds godwit from source with cgo (`go-version` pins the toolchain), so the first step of a job takes about a minute.

## ArgoCD

[argocd/](argocd/README.md): an `Application` for the service (Helm chart in `deploy/helm/godwit`) and one for an application whose chart carries the PreSync / PostSync hook Jobs from [deploy/argocd](../deploy/argocd/README.md).
