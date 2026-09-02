# CI/CD

Two integrations ship in the repository: a composite GitHub Action (`action.yml` at the root) and ArgoCD hook Jobs (`deploy/argocd/`). Both are thin wrappers over the CLI; anything they do you can do with `godwit` in any runner. Complete workflows and `Application` manifests to copy are in [examples/](../examples/README.md).

The CLI outside GitHub comes from the image `ghcr.io/samuelmolling/godwit` (`main`, `sha-<short commit>`; built by `.github/workflows/publish.yml` on every merge) or, once a `v*` tag exists, from the GitHub release and `brew install SamuelMolling/tap/godwit` (`.github/workflows/release.yml`, GoReleaser).

## Exit codes

| Code | CLI | Meaning |
|---|---|---|
| 0 | all | success; for `migrate` and `revert`: the run reached `succeeded` or `awaiting_contract` |
| 1 | all | error: blocking lint findings, refusal at admission, run `failed` or `needs_attention`, connection or usage error |
| 2 | Action only | unknown `command` input |
| 3 | `migrate` | plan stale or required: re-plan on the pull request |

The Action's last step re-exits with the CLI status after the summary and comment are written, so a failing lint still posts its report.

## GitHub Action

```yaml
- uses: SamuelMolling/godwit@main
  with:
    command: lint | plan | migrate
```

It builds godwit from the checked-out action ref with `actions/setup-go` (`CGO_ENABLED=1`, needs `gcc`), so the first run in a job takes a minute; `go-version` pins the toolchain. The runner also needs `jq` and `gh` (present on GitHub-hosted runners).

### Action inputs and outputs

| Input | Default | Used by |
|---|---|---|
| `command` | required | `lint`, `plan`, `migrate` |
| `dir` | `dir` from `godwit.yaml`, else `migrations` | all |
| `base` | `origin/main` | lint: only migrations added since the ref are linted, files modified since it are `E003`; empty checks every file. The ref is fetched depth-1 when missing |
| `ack` | — | lint, migrate: comma-separated hazard codes |
| `server` | `server` from `godwit.yaml` or `GODWIT_SERVER` | migrate |
| `token` | — | migrate; exported as `GODWIT_TOKEN`, never passed on the command line |
| `target` | `target` from `godwit.yaml` | migrate |
| `rollout` | `godwit.yaml`, else `direct` | migrate |
| `dry-run` | `false` | migrate: `PlanRun` instead of `CreateRun`, markdown report, no run |
| `source` | `<host>/<owner>/<repo>@<sha>[:<dir>]` | migrate: provenance stored on the run (`cp_runs.source`) |
| `comment` | `true` | post the report as one sticky pull request comment |
| `github-token` | `${{ github.token }}` | for the comment (`pull-requests: write`) |
| `go-version` | `1.26` | build |

| Output | Meaning |
|---|---|
| `run-id` | id of the run created by `migrate` (empty for dry runs and refusals) |
| `blocking` | number of blocking lint findings |
| `summary-path` | file with the markdown report, also appended to the job summary |

The comment is posted only on `pull_request` events, and never for a real `migrate`. It is found and replaced by a hidden marker: `<!-- godwit:lint -->`, `<!-- godwit:plan -->` or `<!-- godwit:dry-run -->`, so lint, plan and dry run each keep one comment per pull request. A failed comment is a workflow warning, not a failure.

### Pull request gate

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
        with: { command: plan }
      - uses: SamuelMolling/godwit@main
        with:
          command: migrate
          dry-run: "true"
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_READ }}
```

`lint` needs no service. The dry run needs a token with the `read` scope only: it asks the service for the admitted plan against the real target (hazards, out-of-order check, scratch validation, which versions are already applied, which statements would be deferred to contract) and reports it. A refusal becomes a `## godwit dry run` comment with the reason and a failed step.

Acknowledging a hazard is a code change, visible in the workflow file or in the migration author's `--ack` list, never a click:

```yaml
      - uses: SamuelMolling/godwit@main
        with:
          command: lint
          ack: H003
```

### Merge step

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
          server: https://godwit.internal
          token: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
          rollout: expand-contract
      - run: echo "run ${{ steps.migrate.outputs.run-id }}"
```

The step streams `godwit migrate --json` events, writes `## godwit migrate` with the final state (and the error when there is one) to the job summary, and exits with the run. `concurrency` is optional: the service serialises runs per target itself, and a second run created while the first is `queued`/`running` just waits. The `source` recorded on the run is `github.com/<owner>/<repo>@<sha>`, which `godwit runs` and `godwit audit` show.

### Expand → contract in a pipeline

With `rollout: expand-contract` the merge step exits 0 while the run sits in `awaiting_contract`. Confirm from the deploy pipeline once the new application version is out:

```yaml
      - run: godwit run confirm --latest --allow-none --target orders
        env:
          GODWIT_SERVER: https://godwit.internal
          GODWIT_TOKEN: ${{ secrets.GODWIT_TOKEN_PIPELINE }}
```

`--allow-none` makes the step a no-op when nothing awaits (a deploy that shipped no migration). Without it the CLI fails with `target orders: no run awaiting contract`.

## ArgoCD

`deploy/argocd/` has two hook Jobs for an application whose migrations are shipped in a ConfigMap alongside the manifests.

**PreSync** (`presync-job.yaml`): `godwit migrate --target=orders --dir=/migrations --rollout=expand-contract` with the `orders-migrations` ConfigMap mounted at `/migrations`, `GODWIT_SERVER=http://godwit.godwit.svc:8474`, `GODWIT_TOKEN` from Secret `orders-godwit` key `token` (scope `pipeline`). `backoffLimit: 0` (the service retries, the Job must not), `activeDeadlineSeconds: 3600`, `hook-delete-policy: BeforeHookCreation`. Exit 0 on `succeeded` or `awaiting_contract` lets the sync proceed with the schema expanded; exit 1 stops the sync, the run's error is in the Job log and in `godwit run get`.

**PostSync** (`postsync-confirm.yaml`): `godwit run confirm --latest --allow-none --target=orders`, `activeDeadlineSeconds: 600`. After the new pods are healthy, the contract phase runs. If the sync fails between the hooks, the run stays `awaiting_contract`; the old pods keep working against the expanded schema, and the next successful sync's PostSync confirms it, or an operator reverts it.

Replace `orders`, the Secret name and, for a reproducible hook, the image tag (`ghcr.io/samuelmolling/godwit:main` in the examples; `sha-<short commit>` is immutable); the ConfigMap must contain both `.up.sql` and `.down.sql` files (the CLI loads the directory and rejects a version with a missing side). Runs created by these Jobs carry `created_by = <token name>` and an empty `source` unless `--source` is added to the args.

## Expand → contract, end to end

1. Author the change as two migrations: an additive one (`CREATE`, `ADD COLUMN`, `CREATE INDEX CONCURRENTLY`) and a later destructive one (`DROP`, `RENAME`: H002/H003/H008). The split is per migration: every plan up to the first one carrying a contract hazard runs in expand, that plan and everything after it wait for contract. A single file mixing both lands in contract whole.
2. Pull request: `lint` (hazards acknowledged where intended), `plan`, dry run with a read token.
3. Merge: `migrate --rollout expand-contract` → run ends `awaiting_contract`, step exits 0.
4. Deploy the application version that handles both shapes.
5. `run confirm --latest --allow-none` → contract phase runs, run ends `succeeded`.
6. If step 4 fails: `godwit revert <run-id>` applies the down side of the expand phase (needs the destructive hazards in the down files acknowledged).

A run whose plan has no contract statements skips `awaiting_contract` and ends `succeeded` directly, so the same pipeline serves additive and destructive changes.
