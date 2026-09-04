# Deployment examples

godwit is one distroless binary with a leased, multi-replica design, so it runs anywhere a Linux process runs. [docs/deployment.md](../../docs/deployment.md) covers the part that is the same everywhere — what a target is, where its credentials come from, and the Helm chart under ArgoCD. These are the platforms it does not cover, each complete enough to copy and adapt, each saying what it assumes and what it leaves to you.

| | What it is | Verified |
|---|---|---|
| [Kubernetes with ingress-nginx](kubernetes-ingress-nginx/README.md) | chart values, the Ingress annotations that matter, TLS, and what each kind of client can actually reach | `helm template` renders; every object passes `kubectl apply --dry-run=client` |
| [AWS ECS](ecs/README.md) | Fargate task definition, Secrets Manager or SSM into the environment, an ALB target group on `/readyz`, two tasks for the lease, RDS for the store and a disposable one for scratch | untested against AWS; flags and endpoints checked against the code |
| [Docker Compose](docker-compose/README.md) | the smallest deployment that is still a deployment: two replicas, the store, scratch, and the secrets in a file | run end to end — `target add`, `plan`, `migrate`, `targets` |
| [A plain VM](systemd/README.md) | systemd unit, environment file, restarts, logs, and what a second machine buys you | untested on a Linux host; flags and endpoints checked against the code |

[`demo/`](../../demo/README.md) is not on this list on purpose. It builds the image from the working tree, hardcodes a master key and a token in the compose file, and ships a target database and a dev-mode Vault so that a `kill -9` can be watched. It is a walkthrough, not a deployment.

## What every one of them has to answer

**Two PostgreSQL servers.** The store is godwit's own control plane, small, and the thing to back up. The scratch server is where validation and `godwit diff` execute the SQL a caller submits, and it must hold nothing: a `read` token is enough to reach that execution. `serve` inspects the scratch role at start-up and refuses to run if it is a superuser, owns the store database, or holds `CREATEROLE`, `REPLICATION` or the file-access memberships ([security](../../docs/security.md#the-scratch-database)).

**Secrets stay out of the arguments.** `GODWIT_MASTER_KEY`, `GODWIT_TOKENS`, `GODWIT_STORE_DSN` and `GODWIT_SCRATCH_DSN` are environment variables and go wherever the platform puts secrets. Pass the store DSN as `GODWIT_STORE_DSN` rather than `--store-dsn`: an argument is visible in `docker inspect`, `DescribeTaskDefinition`, `kubectl get pod -o yaml` and `/proc/<pid>/cmdline`, and every platform here can set an environment variable. The CLI has the same pair for the DSNs it takes — `GODWIT_DSN` for the local commands, `GODWIT_TARGET_DSN` for `godwit target add`.

**Two health endpoints that are not interchangeable.** `/healthz` always answers `200 ok` and touches nothing — liveness. `/readyz` answers `200` only when the store responds to a ping within two seconds, and `503 store unavailable: …` otherwise — readiness, and the one a load balancer should ask. Both, and `/metrics`, are unauthenticated on the listener, so publish them deliberately or not at all.

**More than one replica, because of the lease.** A replica that dies mid-run loses its lease after `--lease-ttl` (30 s) and another replica claims the run and resumes it from the journal in the target database. Two is the floor. The holder is the process's own identity — `<name>/<16 hex characters>`, the name from `--holder` (or `GODWIT_HOLDER`, or the hostname) and the suffix drawn at start-up — so replicas that share a hostname still hold separate leases. Set `--holder` where the hostname says nothing useful (`host` networking, a cloned image, two processes on one box) to get a readable name in `cp_leases.holder` and the logs; it is a label, not the identity, and cannot be made to collide.

**Reaching it from a pipeline.** The listener is plaintext h2c and HTTP/1.1; TLS is always something in front. Connect's unary calls *and* its server streams work over HTTP/1.1, so `curl`, generated connect clients and the `godwit` CLI all go through an ordinary TLS proxy: `--server https://…` dials TLS, trusting the system root store, and negotiates HTTP/2 where the proxy offers it. The proxy needs no h2c or gRPC backend. A plaintext port is still a private-network-only affair — the bearer token is the whole authorisation.

**Graceful shutdown, bounded by `--shutdown-timeout`.** `SIGINT` and `SIGTERM` shut the listener down, stop the replica claiming, and then wait for the runs it already holds — their leases still beating, so nothing else may take them — before the process exits `0`. `--shutdown-timeout` (20 s) is the budget for all of it; a run that outlives it is cut and left to its lease, which is the crash the journal is designed for. Keep the budget under the platform's kill delay (`terminationGracePeriodSeconds`, `stopTimeout`, `TimeoutStopSec`), or the platform ends the process first and the drain buys nothing.

**No in-container health check.** The image is distroless: no shell, no `curl`. Docker, Compose and ECS container health checks all run a command inside the container, so there is nothing useful to run. Check the listener from outside.
