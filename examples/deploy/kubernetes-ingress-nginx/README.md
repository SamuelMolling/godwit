# Kubernetes with ingress-nginx

The chart in [deploy/helm/godwit](../../../deploy/helm/godwit) supports an Ingress and nobody has shown one end to end: which annotations `ingress-nginx` needs, what to publish and what to keep off it, where TLS goes, and which clients can then actually reach the API. [deployment.md](../../../docs/deployment.md) is the reference for what a target is and where its credentials come from; this page is the part in front of the service, which that page leaves to you.

**Assumes:** a cluster with `ingress-nginx` installed and an `IngressClass` named `nginx`, cert-manager with a `ClusterIssuer` named `letsencrypt-prod`, the Prometheus Operator CRDs (for `serviceMonitor.enabled`), and a DNS record for `godwit.example.com` pointing at the ingress controller.

**Leaves to you:** where the two PostgreSQL servers come from (a managed service, CloudNativePG, a StatefulSet), and the credential provider for your targets. Registering the targets is the chart's `targets.list`, or the `godwit target add` calls [deployment.md](../../../docs/deployment.md#registering-a-target) describes.

Files here: [`values.yaml`](values.yaml) (the chart values, rendered and validated) and [`ingress-grpc.yaml`](ingress-grpc.yaml) (a second Ingress for HTTP/2 clients — read [Reaching the API](#reaching-the-api) before you apply it).

## The two databases

godwit needs a **store** — its own control plane, which it migrates itself — and a **scratch** server, where validation and `Diff` execute the SQL a caller submits. They must be two servers, not two databases on one: a `read` token is enough to reach that execution, so the scratch role must be unable to see the store ([security](../../../docs/security.md#the-scratch-database)).

On the store server:

```sql
CREATE ROLE godwit LOGIN PASSWORD '...' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE DATABASE godwit_store OWNER godwit;
REVOKE CONNECT ON DATABASE godwit_store FROM PUBLIC;
```

On the scratch server:

```sql
CREATE ROLE godwit_scratch LOGIN PASSWORD '...'
  CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS;
```

`serve` inspects the scratch role at start-up and **refuses to start** if it is a superuser, owns the store database, or holds `CREATEROLE` / `REPLICATION` / the file-access roles. Size the scratch server against `--max-concurrent-diffs` (4 by default): each admitted `Diff`, `PlanRun`, `CreateRun`, `RevertRun` or `Checkpoint` builds four to five databases on it.

## The Secret

The chart never creates it, and the Deployment references `tokens` and `storeDSN` unconditionally — a Secret missing either leaves the pod in `CreateContainerConfigError`. (`extraObjects` is where the Secret goes when an operator produces it; the chart's own values file has an `ExternalSecret` sketch.)

```bash
kubectl create namespace godwit
kubectl -n godwit create secret generic godwit \
  --from-literal=GODWIT_MASTER_KEY=$(openssl rand -hex 32) \
  --from-literal=GODWIT_TOKENS="ci:pipeline:$(openssl rand -hex 16),ops:operator:$(openssl rand -hex 16),register:admin:$(openssl rand -hex 16)" \
  --from-literal=GODWIT_STORE_DSN='postgres://godwit:...@store.internal:5432/godwit_store?sslmode=require' \
  --from-literal=GODWIT_SCRATCH_DSN='postgres://godwit_scratch:...@scratch.internal:5432/postgres?sslmode=require'
```

`GODWIT_MASTER_KEY` is only needed by `static` targets: when every target uses the `kubernetes` or `vault` provider, leave the literal out and set `existingSecret.keys.masterKey: ""` — the Deployment guards that key with `with`, so an empty value drops the reference. `GODWIT_TOKENS` is read once at start-up, so adding an application's token is a pod roll.

## Install

```bash
helm upgrade --install godwit deploy/helm/godwit -n godwit -f values.yaml
kubectl -n godwit rollout status deploy/godwit
kubectl -n godwit logs deploy/godwit | grep -E 'listening|not isolated|no tokens'
```

A clean start logs `store migrated` and `listening` and nothing else. `scratch database is not isolated` means `serve.scratch.enabled` is off or the Secret's `GODWIT_SCRATCH_DSN` is empty; `no tokens configured` means every caller is an anonymous admin.

## What the Ingress publishes

Two prefixes, and deliberately no more:

| Path | Who reaches it |
|---|---|
| `/ui` | a browser; basic auth is any bearer token's secret, or the `GODWIT_UI_USER` / `GODWIT_UI_PASSWORD` pair |
| `/godwit.v1.GodwitService` | the connect API — `curl` and generated connect clients, over HTTP/1.1 |

`/metrics`, `/healthz` and `/readyz` are **unauthenticated on the listener** and are left off the Ingress on purpose. `/metrics` names every target and its run counts; the ServiceMonitor scrapes it through the Service, inside the cluster, where it belongs.

## The annotations that matter

| Annotation | Why |
|---|---|
| `proxy-body-size: 32m` | `--max-request-bytes` is 32 MiB and ingress-nginx defaults to `1m`. A migration set larger than that is a `413` from nginx, before any godwit limit is consulted, with no godwit log line to explain it. Raise both together if you raise `--max-migrations`. |
| `proxy-read-timeout: 900` | `PlanRun`, `CreateRun`, `Diff` and `Checkpoint` build scratch databases and replay the target's whole recorded history before they answer. Nothing is written to the socket while that runs, and `--max-concurrent-diffs` can hold a call in its queue for a further 30 seconds. The 60-second default cuts exactly the calls a large directory makes slow. |
| `proxy-buffering: off` | `WatchRun` is a server stream, and `migrate`, `revert` and `run confirm` are clients of it. With buffering on, nginx holds the frames and the caller sees nothing until the run ends. |
| `ssl-redirect: true` | The listener is plaintext; TLS exists only in front of it. |

`proxy-read-timeout` is an *inactivity* timeout, and `WatchRun` emits a frame every 500 ms for the whole run, so it is not what bounds a long migration — a 24-hour run streams fine under a 900-second read timeout. It bounds the silent unary calls above.

## Reaching the API

The listener speaks **plaintext h2c and HTTP/1.1**; there is no TLS in the binary. What a client can use depends on how it dials:

| Client | Over the TLS Ingress | In-cluster (`http://godwit.godwit.svc:8474`) |
|---|---|---|
| a browser, on `/ui` | yes | — |
| `curl`, connect JSON over HTTP/1.1 | yes | yes |
| a generated connect client with an ordinary `http.Client` | yes | yes |
| the `godwit` CLI | yes | yes |

The CLI picks its transport from the scheme: `https://` is dialled over TLS, trusting the system root store, and negotiates HTTP/2 by ALPN where the proxy offers it; `http://` is dialled as cleartext HTTP/2 (h2c), which is what the in-cluster Service speaks. The plain HTTP backend of the Ingress above is enough for both — connect's unary calls and its one server stream (`WatchRun`) work over HTTP/1.1, so no annotation is needed to carry the API.

For a private CA, the CLI has no flag: put the CA in the runner's system trust store (`update-ca-certificates`, or `SSL_CERT_FILE` on Linux).

Reaching the plaintext listener instead is still the simplest thing in-cluster:

- an ArgoCD hook Job or a self-hosted runner **in the cluster**, with `GODWIT_SERVER=http://godwit.godwit.svc:8474` — this is the shape [deploy/argocd](../../../deploy/argocd/README.md) already uses;
- `kubectl -n godwit port-forward svc/godwit 8474:8474` for a human at a terminal;
- a private L4 route (a `Service` of type `LoadBalancer` on an internal address, or the ingress controller's `tcp-services` map) reachable only from your own network. Do not put the plaintext port on the public internet: the bearer token would cross it in the clear.

[`ingress-grpc.yaml`](ingress-grpc.yaml) is the HTTP/2 form — `backend-protocol: GRPC` makes nginx `grpc_pass` to the h2c listener — for gRPC clients, which unlike connect ones cannot fall back to HTTP/1.1. Nothing here needs it. `grpc_pass` applies to the whole nginx server block, so it cannot share a host with `/ui`: a browser reaching an HTML page through it gets a gRPC response frame. That is why it is a second hostname rather than another path on the first, and why the chart's single `ingress.*` block cannot express both at once. `kubectl apply -f` it, or paste it into `extraObjects` to keep it inside the release.

## The UI, and `--ui-origin`

`serve.ui.origins` is not optional once an Ingress publishes `/ui`. It is both the allowlist of origins a form post may come from **and** the allowlist of `Host` values the UI answers on at all; a request for any other host is refused with `403 unknown host`, whatever its method. Set it to exactly the scheme and host the browser uses, including a non-default port if there is one — an origin of `http://localhost:8474` reached on `:18474` is a `403`, which is the check working.

Left empty, the UI compares the browser's `Origin` against the request's `Host`. ingress-nginx passes the browser's `Host` through by default, so that also works — until someone puts a proxy in front that rewrites it. Naming the origin costs one line and removes the question.

## More than one replica

`replicaCount: 2` is the floor, not a preference: the crash-safety story is that a replica which dies mid-run loses its lease after `--lease-ttl` and *another replica* claims the run and resumes it from the journal in the target. The values here also set `podAntiAffinity: hard` so a single node loss cannot take both, and keep the chart's `podDisruptionBudget.minAvailable: 1`. On a cluster with fewer schedulable nodes than replicas, `hard` leaves a pod `Pending`; the chart's default `soft` is the one to keep there.

Two things follow from the lease being keyed on `cp_leases.holder` (matched whole by `Heartbeat`):

- the holder is `<name>/<16 hex characters>`: the pod name, and a suffix drawn when the process started. Nothing here needs setting — a `Deployment` already gives each pod a unique name — and the suffix is why a pod that restarts under the same name cannot take its own in-flight run back before the lease expires. That wait is the window in which the backend it left in the target, still holding that target's advisory lock, dies.
- `SIGTERM` is handled: the pod stops serving, stops claiming, and waits for the run it already holds until `--shutdown-timeout` (20 s), then exits `0`. The chart's `terminationGracePeriodSeconds: 30` is what makes that budget real and must stay above it. A run that outlives the drain is cut and stays `running` until its lease expires, and the surviving replica takes it over — the same path a `SIGKILL` or a lost node takes.

## The pipeline

`godwit lint` and `godwit plan` on a pull request need only `read` and a route to the API. A GitHub-hosted runner reaches the TLS Ingress with the CLI or the [composite action](../../../docs/ci-cd.md) — `GODWIT_SERVER=https://godwit.example.com` — as long as the Ingress' certificate chains to a public CA. From a self-hosted runner in the cluster, or from the ArgoCD PreSync hook, the CLI works unchanged against `http://godwit.godwit.svc:8474`.

## Verified

`helm template godwit deploy/helm/godwit -n godwit -f values.yaml` renders, and every object it produces plus `ingress-grpc.yaml` passes `kubectl apply --dry-run=client`. The flags in the rendered `args` were read back against `internal/cli/serve.go`. The runtime facts above — the health endpoints and their codes, `/metrics` answering unauthenticated, the connect API over HTTP/1.1 and `403 unknown host` from a mismatched `--ui-origin` — were measured on the [Docker Compose](../docker-compose/README.md) stack, which runs the same binary with the same flags. The CLI over TLS and the `SIGTERM` drain were measured separately, against a real TLS endpoint and a real store. The Ingress itself was not applied to a live ingress-nginx.
