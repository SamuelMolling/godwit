# godwit manual

Plain markdown, one file per concern. Everything here describes the code on `main`; when a flag, key or RPC is missing from these pages, the code wins and the page is wrong.

| Page | Read it when |
|---|---|
| [Getting started](getting-started.md) | You have a migrations directory and want a first run through the service, then a first CI step. |
| [Concepts](concepts.md) | You want to know what the journal protocol guarantees across a crash, what a run's states mean, how leases, hazards, validation, rollout policies, revert, drift and baseline work. |
| [Configuration](configuration.md) | You need the exact list of `godwit.yaml` keys, `serve` flags and environment variables, the token spec, or the CLI reference. Single source of truth. |
| [Operations](operations.md) | You run the service: HA, store sizing and privileges, backups before destructive runs, retention, upgrades, metrics and alert rules, notifications, logging. |
| [Runbook](runbook.md) | Something is wrong: a run in `needs_attention`, a run stuck in `awaiting_contract`, lock timeouts, a lost replica, a refused validation, drift, a checksum mismatch. SQL to look at, command to run. |
| [CI/CD](ci-cd.md) | You wire the GitHub Action or the ArgoCD hooks, and need the exit codes and the expand → contract flow. |
| [API](api.md) | You call the connect endpoint directly: every RPC, the scope it needs, request and response shapes, curl examples. |
| [Security](security.md) | Tokens and scopes, the master key and how to rotate it, credential providers, what is and is not logged, network exposure. |
| [Comparison](comparison.md) | You are choosing between godwit and Flyway, Liquibase or Atlas and want the honest list, including what godwit does not have. |
| [Decisions](decisions/README.md) | You want to know *why* — the question that was open, the evidence, what it costs, and what was refused. The pages above describe the code; these explain it. |

Suggested order for a first read: getting started → concepts → configuration. Operators add operations and the runbook; pipeline authors add CI/CD.

Related material outside `docs/`: [examples/README.md](../examples/README.md) (ready-to-copy GitHub Actions workflows and ArgoCD manifests), [deploy/helm/godwit/README.md](../deploy/helm/godwit/README.md) (chart values), [deploy/argocd/README.md](../deploy/argocd/README.md) (hook Jobs), [demo/README.md](../demo/README.md) (the docker-compose walkthrough), [AGENTS.md](../AGENTS.md) (contributor rules).
