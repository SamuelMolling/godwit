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

**Secrets, and the one flag that has no variable.** `GODWIT_MASTER_KEY`, `GODWIT_TOKENS` and `GODWIT_SCRATCH_DSN` are environment variables and go wherever the platform puts secrets. `--store-dsn` is a required *flag* with no variable behind it: the Helm chart writes `--store-dsn=$(GODWIT_STORE_DSN)` and lets Kubernetes expand it, and no other platform here can do that. The portable answer is a password-less DSN on the command line plus `PGPASSWORD` in the environment — pgx completes the DSN from the libpq environment, which keeps the password out of `docker inspect`, `DescribeTaskDefinition` and `/proc/<pid>/cmdline`.

**Two health endpoints that are not interchangeable.** `/healthz` always answers `200 ok` and touches nothing — liveness. `/readyz` answers `200` only when the store responds to a ping within two seconds, and `503 store unavailable: …` otherwise — readiness, and the one a load balancer should ask. Both, and `/metrics`, are unauthenticated on the listener, so publish them deliberately or not at all.

**More than one replica, because of the lease.** A replica that dies mid-run loses its lease after `--lease-ttl` (30 s) and another replica claims the run and resumes it from the journal in the target database. Two is the floor. The holder name is the machine's hostname and `Heartbeat` matches on `(run_id, holder)` alone, so **replicas must not share a hostname** — free under Kubernetes, ECS `awsvpc` and Compose, not free with ECS `host` networking or cloned VM images.

**Reaching it from a pipeline.** The listener is plaintext h2c and HTTP/1.1; TLS is always something in front. Connect's unary calls *and* its server streams work over HTTP/1.1, so `curl` and generated connect clients go through any ordinary TLS proxy. The `godwit` CLI does not: `internal/cli/client.go` builds an `http2.Transport` whose `DialTLSContext` opens a plain TCP connection, and `x/net/http2` uses that hook for `https://` URLs too, so `--server https://…` dials port 443 in cleartext and fails. Until that changes, anything running the CLI needs a private route to the plaintext port — in-cluster, in-VPC, or a tunnel — and the token must never cross a public network in the clear.

**No graceful shutdown.** `serve` installs no signal handler, so the `srv.Shutdown` path in `server.Run` never fires and the process ends the moment it is signalled, cutting in-flight streams. Nothing is left dirty — that is the crash the journal is designed for, and the surviving replica takes the run over after the lease TTL — but it means `terminationGracePeriodSeconds`, `stopTimeout` and ELB deregistration delays buy nothing. As PID 1 in a container the exit code is `2` rather than `143`, because Go cannot die from an unhandled signal as PID 1.

**No in-container health check.** The image is distroless: no shell, no `curl`. Docker, Compose and ECS container health checks all run a command inside the container, so there is nothing useful to run. Check the listener from outside.
