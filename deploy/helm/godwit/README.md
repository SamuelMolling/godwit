# godwit Helm chart

Runs `godwit serve` as a two-replica Deployment: the second replica is what turns the leased scheduler into crash safety, so keep `replicaCount` at 2 or more.

## Prerequisites

- A PostgreSQL database for the control-plane store (any version the service supports; it creates its own tables).
- A second PostgreSQL for the scratch databases validation and `godwit diff` execute submitted SQL on, with a role that owns nothing else. Skipping it leaves that execution on the store server as the store role, and the pods say so on every start ([security](../../../docs/security.md#the-scratch-database)).
- An image. The chart defaults to `ghcr.io/samuelmolling/godwit:main`, published from every merge to `main` (also tagged `sha-<short commit>`); to run from your own registry, `docker build -t <registry>/godwit:<tag> .` at the repo root, push it, set `image.repository` / `image.tag`.
- A Secret with the credentials. The chart never creates it, and reads it whole: every entry in it becomes an environment variable of that name, so what godwit reads from the Secret is configured there and in no value.

```bash
kubectl -n godwit create secret generic godwit \
  --from-literal=GODWIT_TOKENS=ci:pipeline:orders-ci-token,ops:operator:ops-token,admin:admin:admin-token \
  --from-literal=GODWIT_STORE_DSN='postgres://godwit:secret@store.internal:5432/godwit_store' \
  --from-literal=GODWIT_SCRATCH_DSN='postgres://godwit_scratch:secret@scratch.internal:5432/postgres'
```

```sql
-- on scratch.internal, once
CREATE ROLE godwit_scratch LOGIN PASSWORD 'secret'
  CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS;
```

The `GODWIT_SCRATCH_DSN` entry is what puts scratch there; leave it out and that execution stays on the store server. The pods refuse to start when the scratch role is a superuser or owns the store database.

**No master key is in that Secret, because only `static` targets need a key** — theirs is the one DSN godwit stores, and a `kubernetes` or `vault` target stores a path. To register a `static` target, add `--from-literal=GODWIT_MASTER_KEY=$(openssl rand -hex 32)` to the Secret above. Without it, `godwit target add --provider static` is refused with

```
invalid_argument: static provider needs a key: set GODWIT_MASTER_KEY, or GODWIT_KEY_PROVIDER with GODWIT_KMS_KEY
```

and a `static` target already in the store makes every pod log `static targets are sealed and no key is configured` naming it, with its runs refused. Rotating the key needs no re-registration: put the new key in the Secret, the old one under `GODWIT_MASTER_KEY_PREVIOUS` (which on its own makes the pods refuse to start, since it configures no key to seal with), roll, and every replica reseals what it finds at start-up. `serve.keyProvider.name: gcpkms` or `vault-transit` moves the key into a KMS instead — `GODWIT_MASTER_KEY` is then unread — wrapping a per-value data key so the DSN never leaves the process ([security](../../../docs/security.md#the-key-and-where-it-comes-from)).

`GODWIT_TOKENS` is the comma-separated list of `name:scope:secret` bearer tokens the API accepts (scopes `read`, `pipeline`, `operator`, `admin`, cumulative; a bare secret is admin, and a two-field `name:secret` entry is refused at start-up); the name is recorded as the actor on runs, logs, notifications and the audit log (a bare secret is named `anonymous`).

## Install

```bash
helm upgrade --install godwit deploy/helm/godwit -n godwit --create-namespace \
  --set image.tag=sha-$(git rev-parse --short HEAD)
```

The release prints the in-cluster URL and the first commands to run. Every value is documented in [values.yaml](values.yaml); the files in [ci/](ci/) are complete renderings for four platform shapes, and `make helm-lint` lints and templates every one of them.

| File | Shape |
|---|---|
| [ci/full-values.yaml](ci/full-values.yaml) | every optional block on at once, which no real deployment does |
| [ci/platform-ingress-values.yaml](ci/platform-ingress-values.yaml) | ingress-nginx, a `Secret` created by hand |
| [ci/platform-gitops-values.yaml](ci/platform-gitops-values.yaml) | Gateway API, a `Secret` produced by External Secrets, ArgoCD hooks |
| [ci/platform-internal-values.yaml](ci/platform-internal-values.yaml) | no ingress at all: Service-only access, a Vault Agent sidecar writing the credential file |

## What gets rendered

| Object | Notes |
|---|---|
| Deployment | `serve --listen --drift-interval --scratch-template [--skip-validation] [--ui --ui-scope --ui-anonymous-scope]` — the two DSNs arrive as `GODWIT_STORE_DSN` and `GODWIT_SCRATCH_DSN`, never as arguments — env from the Secret, `GODWIT_LOG_FORMAT` / `GODWIT_LOG_LEVEL` from `serve.logFormat` / `serve.logLevel`, readiness `/readyz`, liveness `/healthz`, a `startupProbe` on `/healthz` that gives the store migration five minutes before either of them applies, non-root read-only container, soft pod anti-affinity by default |
| Service | ClusterIP on `service.port` → container port `serve.port` |
| Service (webhook) | only with `serve.githubApp.enabled`; a **second** Service, `<release>-webhook`, ClusterIP on `serve.githubApp.service.port` → container port `serve.githubApp.port`, carrying that port and nothing else |
| ServiceAccount | `serviceAccount.annotations` for Vault Kubernetes auth or cloud workload identity; token mounted by default because the Vault provider reads it |
| PodDisruptionBudget | `minAvailable: 1` so a drain never takes both replicas |
| ServiceMonitor | off by default; scrapes `/metrics` through the Service |
| Ingress | off by default; an ordinary HTTP backend carries the whole API, and a gRPC backend would stop the same host serving `/ui` |
| HTTPRoute | off by default; the Gateway API alternative to the Ingress, for a cluster that routes that way |
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

## The GitHub App listener

`serve.githubApp.enabled` opens a second listener, on its own port, serving `/github/webhook` and 404 for everything else. It needs `GODWIT_GITHUB_APP_ID` and `GODWIT_GITHUB_WEBHOOK_SECRET` in the Secret, and the pods refuse to start without either — an unverified webhook endpoint is an open one. It is the only part of godwit that has to be reachable from the internet, and it is a separate listener precisely so that exposing it exposes nothing else.

The chart renders that listener its **own Service**, `<release>-webhook`, rather than a second port on the API's. Point the public route at that Service:

```yaml
# in the platform's repository, not here
backendRefs:
  - name: godwit-webhook
    port: 8475
```

A Service is the unit a route attaches to, and this one has no API port on it. So the failure that matters — a public hostname that reaches the API — is not a wrong port number away; it needs someone to name the other Service. `chart/ci/platform-github-app-values.yaml` shows the whole shape, including the route and a NetworkPolicy, and `scripts/helm-assert.sh` asserts the webhook Service never carries `serve.port`.

Off is off: with `serve.githubApp.enabled: false` (the default) the render has no `--github-webhook-addr`, no second container port and no webhook Service. The listener is not opened and closed to callers — it never binds.

The App's private key is mounted as a file (`serve.githubApp.privateKeyPath`, read with `--github-private-key-file`) rather than passed as environment. It is multi-line, and unlike a DSN it is the App's whole identity: it mints an installation token for every repository the App is installed on. A file keeps it out of the process environment, which a sidecar, a core dump and `kubectl exec -- env` all read. It is therefore the one Secret entry the chart has to be told about — `existingSecret.githubPrivateKey`, so the volume can project it — and naming it something a shell would accept as a variable fails the render, because the Secret reaches the process whole and the PEM would be back in the environment. The webhook secret and the app id are single-line and stay environment; the app id is not a secret at all.

Standing the App up the first time — creating it, its permissions, its events, and binding a repository to a target — is [CI/CD: registering the App](../../../docs/ci-cd.md#registering-the-app).

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

## Targets and credential stores are not chart values

**The chart registers neither, and has no key for either.** A target is a row in `cp_targets` and a credential store is a row in `cp_credential_stores`: control-plane data, written by `RegisterTarget` / `RegisterCredentialStore` under an `admin` token, audited in `cp_audit`, and referenced by every run and plan that follows. Declaring the same row in a values file gives it two owners, and the values file wins by force — `RegisterTarget` replaces the whole config map rather than patching it, so a sync would silently drop whatever anyone registered against the API. [Decision 0022](../../../docs/decisions/0022-control-plane-data-is-not-chart-configuration.md) has the reasoning.

Register them against the API instead, with an `admin` token that belongs to whatever registers ([deployment](../../../docs/deployment.md#registering-a-target)):

```bash
godwit credential-store add production \
  --vault-addr https://vault.production.internal:8200 --vault-k8s-role godwit

godwit target add orders --provider vault --credential-store production \
  --vault-path secret/data/orders/db \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db:5432/orders' \
  --lock-timeout 5s --require-plan
```

A pipeline that wants this reconciled runs the same commands from a Job of its own, in the repository that owns the target, with the full `target add` line — the upsert makes re-running it free, and keeping the line where the target is owned is what stops a shorter one dropping settings. `extraObjects` takes such a Job if you want it in this release; the chart does not model it, because the set of targets is not a property of the deployment.

Registration is not adoption. A database that already has a schema still needs `godwit target adopt` — `--version` or `--from-journal` — before its first plan ([deployment](../../../docs/deployment.md#adopting-an-existing-database)).

## Credential providers

- `vault`: every `vault` target names a credential store, registered with `godwit credential-store add`, and that store is the only thing that says which Vault the secret is read from — no chart value reaches any target, and a target whose store is missing refuses its runs. With `--vault-k8s-role` the service logs in at that Vault with the ServiceAccount token this chart projects at `/var/run/secrets/godwit/vault/token`, minted for the audience `godwit`; `--vault-token-env` names an environment variable of the service holding a token instead, whose name must begin with `VAULT_TOKEN` and which `extraEnv` / `extraEnvFrom` brings in from a Secret. The chart has no key here: the audience is a constant, and the one thing to configure is `audience="godwit"` on each Vault's Kubernetes auth role ([security](../../../docs/security.md#credential-stores)).
- **`serve.keyProvider.vaultAddr` and its siblings are not that.** They configure the `vault-transit` key provider, where godwit seals the DSNs of `static` targets; leave them empty unless `serve.keyProvider.name` is `vault-transit`.
- `kubernetes`: mount the target's Secret with `extraVolumes` / `extraVolumeMounts` and register the target with `--secret-path` pointing at the file.
- `static`: the only one that needs a key, and no key is in the Secret by default. Add `GODWIT_MASTER_KEY` to it (or set a `serve.keyProvider` of `gcpkms` / `vault-transit`) before registering one; the DSN is sealed with it.

## Notifications

`notifications.webhookUrl` sets `GODWIT_WEBHOOK_URL`. For Slack, add `GODWIT_SLACK_TOKEN` to the Secret and set `notifications.slack.channel`; `notifications.slack.mode` picks `thread` or `edit` and `notifications.publicUrl` is the base of the "Open run" links.

Anything else the process should see (proxies) goes through `extraEnv` / `extraEnvFrom`.

## Admission limits

`serve.limits` sets the pool size, the request and file sizes, the migration file count, the concurrent runs and the concurrent scratch-database calls, and the per-run wall clock. The defaults hold roughly an order of magnitude more than a real migration directory; [operations](../../../docs/operations.md#admission-limits) says which one to raise for which refusal, and `serve.limits.maxConcurrentDiffs` is the one to size the scratch server’s `max_connections` and disk against.

## Web UI

`serve.ui.enabled` adds `--ui` and `--ui-scope`, serving the operator web UI at `/ui` on the same port. With `GODWIT_TOKENS` set the UI is already behind basic auth: any token secret is a valid password and signs in as that token with its own scope, so pages offer only the actions that scope allows. A `GODWIT_UI_USER` / `GODWIT_UI_PASSWORD` pair in the Secret adds a shared identity on top, whose rights are `serve.ui.scope` (default `operator`; `read` makes it a viewer). Without tokens and without that pair, anyone who reaches the port acts as `ui:anonymous` with scope `read` — `serve.ui.scope` belongs to the identity that signed in, and is not handed to a visitor who signed in with nothing.

**`serve.ui.auth: false`** drops authentication from `/ui` entirely (`--ui-anonymous-scope`): no password is asked for whatever `GODWIT_TOKENS` holds, and every visitor is `ui:anonymous` with `serve.ui.anonymousScope` — `operator` by default, so the UI can do everything it offers; `read` serves a dashboard nobody can act from. Set it only when nothing but trusted operators can route to the port — an internal Service, a VPN, an authenticating proxy in front — because the network is then the entire boundary and the audit trail records `ui:anonymous` with no identity behind it. The pod logs `ui served without authentication` on every start, refuses to start with a `GODWIT_UI_USER` in the Secret alongside it — an identity nobody is ever asked for — and the API keeps its own tokens either way. `ci/platform-internal-values.yaml` renders this shape; [security](../../../docs/security.md#an-unauthenticated-ui) states the threat model.

## Scheduling and the pod

`podSecurityContext` and `securityContext` are passed through verbatim and default to a non-root, read-only, all-capabilities-dropped container that satisfies the restricted Pod Security Standard. `nodeSelector`, `tolerations`, `topologySpreadConstraints`, `affinity` / `podAntiAffinity`, `priorityClassName`, `revisionHistoryLimit` and `updateStrategy` cover placement and rollout; `initContainers` and `extraContainers` take sidecars, which is where a Vault Agent or a cloud SQL proxy goes, paired with `extraVolumes` / `extraVolumeMounts`. `commonLabels` lands on every rendered object and on the pod template, but never on the Deployment's selector, so setting it on a live release does not need a delete.

**There is no HPA and there will not be one.** godwit's replicas are lease holders, not a request-serving pool: a replica claims a run, holds its lease and finishes it from the journal. The concurrency knob is `serve.limits.maxConcurrentRuns` per replica, and CPU is a bad proxy for it — a run spends its life waiting on PostgreSQL. Worse, a scale-down evicts a replica that may be holding a lease, and the run then waits out `--lease-ttl` before another replica resumes it. Size `replicaCount` for availability (two is the floor) and raise the limits for throughput.

## Upgrading

Bump `image.tag` (an immutable `sha-<short commit>` tag makes the rollout explicit; `main` with `pullPolicy: Always` follows the branch); the store schema migrates itself at start-up. Rolling update keeps one replica serving, and a run in flight on the replica being replaced is resumed by the other one from the journal once its lease expires.
