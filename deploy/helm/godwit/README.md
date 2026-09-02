# godwit Helm chart

Runs `godwit serve` as a two-replica Deployment: the second replica is what turns the leased scheduler into crash safety, so keep `replicaCount` at 2 or more.

## Prerequisites

- A PostgreSQL database for the control-plane store (any version the service supports; it creates its own tables).
- An image. The chart defaults to `ghcr.io/samuelmolling/godwit:main`, published from every merge to `main` (also tagged `sha-<short commit>`); to run from your own registry, `docker build -t <registry>/godwit:<tag> .` at the repo root, push it, set `image.repository` / `image.tag`.
- A Secret with the credentials. The chart never creates it:

```bash
kubectl -n godwit create secret generic godwit \
  --from-literal=GODWIT_MASTER_KEY=$(openssl rand -hex 32) \
  --from-literal=GODWIT_TOKENS=ci:pipeline:orders-ci-token,ops:operator:ops-token,admin:admin:admin-token \
  --from-literal=GODWIT_STORE_DSN='postgres://godwit:secret@store.internal:5432/godwit_store'
```

`GODWIT_MASTER_KEY` encrypts the DSNs of `static` targets; losing it means re-registering those targets. `GODWIT_TOKENS` is the comma-separated list of `name:scope:secret` bearer tokens the API accepts (scopes `read`, `pipeline`, `operator`, `admin`, cumulative; `name:secret` and a bare secret are admin); the name is recorded as the actor on runs, logs, notifications and the audit log (a bare secret is named `anonymous`).

## Install

```bash
helm upgrade --install godwit deploy/helm/godwit -n godwit --create-namespace \
  --set image.tag=sha-$(git rev-parse --short HEAD)
```

The release prints the in-cluster URL and the first commands to run. Every value is documented in [values.yaml](values.yaml); [ci/full-values.yaml](ci/full-values.yaml) is a rendering with every optional block on, used by `make helm-lint`.

## What gets rendered

| Object | Notes |
|---|---|
| Deployment | `serve --listen --store-dsn=$(GODWIT_STORE_DSN) --drift-interval [--skip-validation]`, env from the Secret, `GODWIT_LOG_FORMAT` / `GODWIT_LOG_LEVEL` from `serve.logFormat` / `serve.logLevel`, readiness `/readyz`, liveness `/healthz`, non-root read-only container, soft pod anti-affinity by default |
| Service | ClusterIP on `service.port` → container port `serve.port` |
| ServiceAccount | `serviceAccount.annotations` for Vault Kubernetes auth or cloud workload identity; token mounted by default because the Vault provider reads it |
| PodDisruptionBudget | `minAvailable: 1` so a drain never takes both replicas |
| ServiceMonitor | off by default; scrapes `/metrics` through the Service |
| Ingress | off by default; the API is HTTP/2 (connect), so use a class that speaks h2c/gRPC to the backend |

## Credential providers

- `vault`: set `vault.addr`; with `vault.k8sRole` the service logs in with the Kubernetes auth method using its own ServiceAccount token, otherwise point `vault.tokenSecret` at a Secret holding `VAULT_TOKEN`.
- `kubernetes`: mount the target's Secret with `extraVolumes` / `extraVolumeMounts` and register the target with `--secret-path` pointing at the file.
- `static`: nothing to configure; the DSN is encrypted with the master key.

## Notifications

`notifications.webhookUrl` sets `GODWIT_WEBHOOK_URL`. For Slack, add `GODWIT_SLACK_TOKEN` to the Secret (`existingSecret.keys.slackToken` names the key) and set `notifications.slack.channel`; `notifications.slack.mode` picks `thread` or `edit` and `notifications.publicUrl` is the base of the "Open run" links.

Anything else the process should see (proxies) goes through `extraEnv` / `extraEnvFrom`.

## Upgrading

Bump `image.tag` (an immutable `sha-<short commit>` tag makes the rollout explicit; `main` with `pullPolicy: Always` follows the branch); the store schema migrates itself at start-up. Rolling update keeps one replica serving, and a run in flight on the replica being replaced is resumed by the other one from the journal once its lease expires.
