# godwit Helm chart

Runs `godwit serve` as a two-replica Deployment: the second replica is what turns the leased scheduler into crash safety, so keep `replicaCount` at 2 or more.

## Prerequisites

- A PostgreSQL database for the control-plane store (any version the service supports; it creates its own tables).
- A second PostgreSQL for the scratch databases validation and `godwit diff` execute submitted SQL on, with a role that owns nothing else. Skipping it leaves that execution on the store server as the store role, and the pods say so on every start ([security](../../../docs/security.md#the-scratch-database)).
- An image. The chart defaults to `ghcr.io/samuelmolling/godwit:main`, published from every merge to `main` (also tagged `sha-<short commit>`); to run from your own registry, `docker build -t <registry>/godwit:<tag> .` at the repo root, push it, set `image.repository` / `image.tag`.
- A Secret with the credentials. The chart never creates it:

```bash
kubectl -n godwit create secret generic godwit \
  --from-literal=GODWIT_MASTER_KEY=$(openssl rand -hex 32) \
  --from-literal=GODWIT_TOKENS=ci:pipeline:orders-ci-token,ops:operator:ops-token,admin:admin:admin-token \
  --from-literal=GODWIT_STORE_DSN='postgres://godwit:secret@store.internal:5432/godwit_store' \
  --from-literal=GODWIT_SCRATCH_DSN='postgres://godwit_scratch:secret@scratch.internal:5432/postgres'
```

```sql
-- on scratch.internal, once
CREATE ROLE godwit_scratch LOGIN PASSWORD 'secret'
  CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS;
```

Then `--set serve.scratch.enabled=true`. The pods refuse to start when that role is a superuser or owns the store database.

`GODWIT_MASTER_KEY` seals the DSNs of `static` targets and nothing else, so a deployment whose targets all use the `kubernetes` or `vault` credential provider can leave `existingSecret.keys.masterKey` empty and the pods start without it. Rotating it needs no re-registration: put the new key in the Secret, the old one under `existingSecret.keys.masterKeyPrevious`, roll, and every replica reseals what it finds at start-up. `serve.keyProvider.name: gcpkms` or `vault-transit` moves the key into a KMS instead, wrapping a per-value data key so the DSN never leaves the process ([security](../../../docs/security.md#the-key-and-where-it-comes-from)). `GODWIT_TOKENS` is the comma-separated list of `name:scope:secret` bearer tokens the API accepts (scopes `read`, `pipeline`, `operator`, `admin`, cumulative; a bare secret is admin, and a two-field `name:secret` entry is refused at start-up); the name is recorded as the actor on runs, logs, notifications and the audit log (a bare secret is named `anonymous`).

## Install

```bash
helm upgrade --install godwit deploy/helm/godwit -n godwit --create-namespace \
  --set image.tag=sha-$(git rev-parse --short HEAD)
```

The release prints the in-cluster URL and the first commands to run. Every value is documented in [values.yaml](values.yaml); the files in [ci/](ci/) are complete renderings for four platform shapes, and `make helm-lint` lints and templates every one of them.

| File | Shape |
|---|---|
| [ci/full-values.yaml](ci/full-values.yaml) | every optional block on at once, which no real deployment does |
| [ci/platform-ingress-values.yaml](ci/platform-ingress-values.yaml) | ingress-nginx, a `Secret` created by hand, targets registered by an operator |
| [ci/platform-gitops-values.yaml](ci/platform-gitops-values.yaml) | Gateway API, a `Secret` produced by External Secrets, targets declared in git, ArgoCD hooks |
| [ci/platform-internal-values.yaml](ci/platform-internal-values.yaml) | no ingress at all: Service-only access, a Vault Agent sidecar writing the credential file |

## What gets rendered

| Object | Notes |
|---|---|
| Deployment | `serve --listen --drift-interval --scratch-template [--skip-validation] [--ui --ui-scope]` — the two DSNs arrive as `GODWIT_STORE_DSN` and `GODWIT_SCRATCH_DSN`, never as arguments — env from the Secret, `GODWIT_LOG_FORMAT` / `GODWIT_LOG_LEVEL` from `serve.logFormat` / `serve.logLevel`, readiness `/readyz`, liveness `/healthz`, non-root read-only container, soft pod anti-affinity by default |
| Service | ClusterIP on `service.port` → container port `serve.port` |
| ServiceAccount | `serviceAccount.annotations` for Vault Kubernetes auth or cloud workload identity; token mounted by default because the Vault provider reads it |
| PodDisruptionBudget | `minAvailable: 1` so a drain never takes both replicas |
| ServiceMonitor | off by default; scrapes `/metrics` through the Service |
| Ingress | off by default; an ordinary HTTP backend carries the whole API, and a gRPC backend would stop the same host serving `/ui` |
| HTTPRoute | off by default; the Gateway API alternative to the Ingress, for a cluster that routes that way |
| Job | off by default; one `godwit target add` per entry of `targets.list` |
| anything in `extraObjects` | raw manifests, templated with the release context |

## Routing

Two mutually exclusive blocks, both upstream-standard, neither naming an implementation. Turn on the one your cluster routes with, or neither.

`ingress.*` renders a `networking.k8s.io/v1` Ingress. `httpRoute.*` renders a Gateway API `HTTPRoute`: set `httpRoute.parentRefs` to the Gateway (or `ListenerSet`) it attaches to and `httpRoute.hostnames`, and the default rule sends everything to the Service. `httpRoute.rules` takes verbatim rules when you want more than that — a rule with no `backendRefs` still gets the godwit Service, so a path split is `matches` alone:

```yaml
httpRoute:
  enabled: true
  parentRefs:
    - { name: internal, namespace: gateway-system, sectionName: https }
  hostnames: [godwit.example.com]
  rules:
    - matches: [{ path: { type: PathPrefix, value: /ui } }]
```

The Gateway itself, its listeners, its certificate and any implementation-specific policy (kgateway `ListenerSet`, Istio `VirtualService`, Traefik middleware) are the platform's, not the chart's. Declare them in `extraObjects` if you want them in this release. `httpRoute.apiVersion` drops to `gateway.networking.k8s.io/v1beta1` for a cluster on pre-1.0 CRDs.

Either way the API is connect over HTTP/2: a browser reaching `/ui` is ordinary HTTP, but a CLI needs the gateway to speak h2c to the backend.

## extraObjects

Anything the chart deliberately does not model goes here as a raw manifest, rendered with the release context so `{{ include "godwit.fullname" . }}`, `{{ .Release.Namespace }}` and `{{ include "godwit.selectorLabels" . }}` resolve. Entries may be maps or strings; a string is the readable form when it carries templating.

This is how the Secret gets created on a platform where secrets arrive through an operator — the chart never creates it, and there is no point in the chart knowing about External Secrets, Vault Secrets Operator, SOPS or Sealed Secrets one at a time:

```yaml
extraObjects:
  - apiVersion: external-secrets.io/v1
    kind: ExternalSecret
    metadata:
      name: godwit
    spec:
      secretStoreRef: { name: vault, kind: ClusterSecretStore }
      target: { name: godwit }
      dataFrom:
        - extract: { key: secrets/godwit }
```

It is also where a NetworkPolicy belongs. The chart has no `networkPolicy` block on purpose: godwit's egress set is the union of every registered target's database, plus Vault, plus Slack and the webhook, so a chart-authored policy would either be a guess or be `to: []`. The ingress side is expressible in six lines with the selector labels the chart hands you — see [ci/platform-gitops-values.yaml](ci/platform-gitops-values.yaml).

## Declarative target registration

`targets.list` turns a target's `godwit target add` line into a values entry, and the chart renders one Job that runs them:

```yaml
targets:
  enabled: true
  tokenSecret:
    name: godwit-admin
    key: GODWIT_ADMIN_TOKEN
  list:
    - name: orders
      provider: vault
      vaultPath: secret/data/orders/db
      vaultTemplate: 'postgres://{{username}}:{{password}}@orders-db:5432/orders'
      lockTimeout: 5s
      requirePlan: true
```

`godwit target add` registers one target per invocation and the image is distroless, so there is no shell to loop in: every entry but the last is an init container and the last is the pod's container. They run in order, and a failure names the target that failed.

`RegisterTarget` is an upsert that replaces the whole config map rather than patching it, which cuts both ways. It is what makes re-running the Job on every sync safe — that is the point of the Job, not a hazard. It is also what makes this list *authoritative*: a setting somebody added by hand with a shorter `target add` is gone at the next sync. Register a target from this list or from somewhere else, never both.

The Job needs an `admin` token, the only scope `RegisterTarget` accepts. Keep it in a Secret of its own rather than reusing an entry of `GODWIT_TOKENS` that humans also hold. A `static` target's DSN goes in `dsnSecret` (passed as `GODWIT_TARGET_DSN`, never as an argument) rather than `dsn`, which would put the credential in the pod spec.

By default the Job is a Helm `post-install,post-upgrade` hook. `targets.helmHook: false` drops those annotations for a deployment tool that drives the Job itself; `targets.annotations` adds its own (`argocd.argoproj.io/hook: Sync`, `hook-delete-policy: BeforeHookCreation`). The service may still be rolling out when the Job starts — `targets.backoffLimit` covers that, and the registration does not touch the target database, so a retry costs nothing.

Registration is not adoption. A database that already has a schema still needs `godwit target baseline` or `godwit target reconcile` before its first plan ([deployment](../../../docs/deployment.md#adopting-an-existing-database)); the chart does not do that for you, because getting it wrong writes history.

## Credential providers

- `vault`: set `vault.addr`; with `vault.k8sRole` the service logs in with the Kubernetes auth method using its own ServiceAccount token, otherwise point `vault.tokenSecret` at a Secret holding `VAULT_TOKEN`.
- `kubernetes`: mount the target's Secret with `extraVolumes` / `extraVolumeMounts` and register the target with `--secret-path` pointing at the file.
- `static`: nothing to configure beyond a key provider; the DSN is sealed with it.

## Notifications

`notifications.webhookUrl` sets `GODWIT_WEBHOOK_URL`. For Slack, add `GODWIT_SLACK_TOKEN` to the Secret (`existingSecret.keys.slackToken` names the key) and set `notifications.slack.channel`; `notifications.slack.mode` picks `thread` or `edit` and `notifications.publicUrl` is the base of the "Open run" links.

Anything else the process should see (proxies) goes through `extraEnv` / `extraEnvFrom`.

## Admission limits

`serve.limits` sets the pool size, the request and file sizes, the migration file count, the concurrent runs and the concurrent scratch-database calls, and the per-run wall clock. The defaults hold roughly an order of magnitude more than a real migration directory; [operations](../../../docs/operations.md#admission-limits) says which one to raise for which refusal, and `serve.limits.maxConcurrentDiffs` is the one to size the scratch server’s `max_connections` and disk against.

## Web UI

`serve.ui.enabled` adds `--ui` and `--ui-scope`, serving the operator web UI at `/ui` on the same port. With `GODWIT_TOKENS` set the UI is already behind basic auth: any token secret is a valid password and signs in as that token with its own scope, so pages offer only the actions that scope allows. `serve.ui.basicAuth` adds a shared identity on top — put `GODWIT_UI_USER` / `GODWIT_UI_PASSWORD` in the Secret (`existingSecret.keys.uiUser` / `uiPassword` name the keys) — whose rights are `serve.ui.scope` (default `operator`; `read` makes it a viewer). Without tokens and without that pair, anyone who reaches the port acts as `ui:anonymous` with scope `read` — `serve.ui.scope` belongs to the identity that signed in, and is not handed to a visitor who signed in with nothing.

## Scheduling and the pod

`podSecurityContext` and `securityContext` are passed through verbatim and default to a non-root, read-only, all-capabilities-dropped container that satisfies the restricted Pod Security Standard. `nodeSelector`, `tolerations`, `topologySpreadConstraints`, `affinity` / `podAntiAffinity`, `priorityClassName`, `revisionHistoryLimit` and `updateStrategy` cover placement and rollout; `initContainers` and `extraContainers` take sidecars, which is where a Vault Agent or a cloud SQL proxy goes, paired with `extraVolumes` / `extraVolumeMounts`. `commonLabels` lands on every rendered object and on the pod template, but never on the Deployment's selector, so setting it on a live release does not need a delete.

**There is no HPA and there will not be one.** godwit's replicas are lease holders, not a request-serving pool: a replica claims a run, holds its lease and finishes it from the journal. The concurrency knob is `serve.limits.maxConcurrentRuns` per replica, and CPU is a bad proxy for it — a run spends its life waiting on PostgreSQL. Worse, a scale-down evicts a replica that may be holding a lease, and the run then waits out `--lease-ttl` before another replica resumes it. Size `replicaCount` for availability (two is the floor) and raise the limits for throughput.

## Upgrading

Bump `image.tag` (an immutable `sha-<short commit>` tag makes the rollout explicit; `main` with `pullPolicy: Always` follows the branch); the store schema migrates itself at start-up. Rolling update keeps one replica serving, and a run in flight on the replica being replaced is resumed by the other one from the journal once its lease expires.
