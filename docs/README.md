# godwit manual

Plain markdown, grouped by what you are doing. Everything here describes the code on `main`; when a flag, key or RPC is missing from these pages, the code wins and the page is wrong.

## Start

**You are deciding whether godwit is for you, and want to see it run.**

| Page | Read it when |
|---|---|
| [Getting started](start/getting-started.md) | You have a migrations directory and want a first run through the service, then a first CI step. |
| [Choosing an approach](start/approaches.md) | You have decided to use it and have to pick how it reaches your pull requests. |
| [Comparison](start/comparison.md) | You are choosing between godwit and Flyway, Liquibase or Atlas and want the honest list, including what godwit does not have. |

## Use

**You open pull requests against a repository godwit already watches.**

| Page | Read it when |
|---|---|
| [GitHub App](use/github-app.md) | The service itself answers your pull requests, and you want to know what it does and what you may ask it. |
| [GitHub Actions](use/github-actions.md) | The plan and the apply are steps in your own workflow. |
| [Standalone](use/standalone.md) | Your pipeline is neither — the CLI in any CI system, or an ArgoCD hook. |
| [Command reference](use/cli.md) | You want to know what a command is *for* — every `godwit` command in plain language, grouped by what you are doing, with a real example each. |

## Run

**You operate the service other people's pull requests reach.**

| Page | Read it when |
|---|---|
| [Deployment](run/deployment.md) | You are standing it up or keeping it up: registering targets, Vault end to end, the Helm values and ArgoCD hooks, then HA, the store, backups, retention, upgrades, metrics and alert rules, notifications, logging. |
| [Configuration](run/configuration.md) | You need the exact list of `godwit.yaml` keys, `serve` flags and environment variables, the token spec, or the flags and scope one command takes. |
| [Security](run/security.md) | Tokens and scopes, where the key lives and how to rotate it, credential providers, what is and is not logged, network exposure. |
| [Runbook](run/runbook.md) | Something is wrong: a run in `needs_attention`, a run stuck in `awaiting_contract`, lock timeouts, a lost replica, a refused validation, drift, a checksum mismatch. SQL to look at, command to run. |

## Internals

**You want to know why godwit behaves the way it does, or you are changing it.**

| Page | Read it when |
|---|---|
| [The journal](internals/journal.md) | What godwit writes into a target and what survives a crash: the statement model, the journal protocol, repeatables, checkpoints, timeouts, `search_path`. |
| [Runs](internals/runs.md) | What a run's states mean, how a lease moves one between replicas, how a rollout is split, what `--to` holds back, and what a revert undoes. |
| [Admission](internals/admission.md) | What a run must pass before it is queued: the hazard gate, the out-of-order guard, the scratch replay, directives and their expansion, and the plan it binds to. |
| [Drift and comparison](internals/drift.md) | What godwit compares: the live schema against its baseline, against a desired schema (`godwit diff`), and against what the control plane recorded. |
| [API](internals/api.md) | You call the connect endpoint directly: every RPC, the scope it needs, request and response shapes, curl examples. |
| [Testing](internals/testing.md) | You want to run the load or chaos rigs, or read what they measured. |
| [Decisions](decisions/README.md) | You want to know *why* — the question that was open, the evidence, what it costs, and what was refused. The pages above describe the code; these explain it. |

Related material outside `docs/`: [examples/README.md](../examples/README.md) (ready-to-copy GitHub Actions workflows and ArgoCD manifests), [examples/deploy/README.md](../examples/deploy/README.md) (the service on ingress-nginx, ECS, Docker Compose and a plain VM), [deploy/helm/godwit/README.md](../deploy/helm/godwit/README.md) (chart values), [deploy/argocd/README.md](../deploy/argocd/README.md) (hook Jobs), [demo/README.md](../demo/README.md) (the docker-compose walkthrough), [AGENTS.md](../AGENTS.md) (contributor rules).
