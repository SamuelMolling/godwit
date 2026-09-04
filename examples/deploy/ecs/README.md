# AWS ECS

A task definition, a service with two tasks, secrets from Secrets Manager, and an ALB in front. **Untested:** nothing on this page was run against AWS. Every flag, environment variable, path and port was checked against `internal/cli/serve.go`, `internal/api` and `internal/server`, and the runtime behaviour it relies on was measured on the [Docker Compose](../docker-compose/README.md) stack, which runs the same binary; the AWS resources and their arguments were not.

**Assumes:** a VPC with private subnets, an ECS cluster, an ALB, and a route to the internet for the image pull (`ghcr.io` has no VPC endpoint, so private subnets need a NAT gateway).

**Leaves to you:** Terraform or CloudFormation around all of it, the certificate on the ALB listener, and the target databases.

Files: [`taskdef.json`](taskdef.json) and [`service.json`](service.json).

## The two databases

Two RDS instances, sized very differently.

**Store** — `db.t4g.small`, Multi-AZ if you care about the control plane, automated backups on, encrypted, private. It holds runs, plans, the audit trail and the file bodies of every run; it is small and it is the thing to back up. Each task opens `--store-max-conns` (20) connections to it, so two tasks are 40 plus a few.

```sql
CREATE ROLE godwit LOGIN PASSWORD '...' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE DATABASE godwit_store OWNER godwit;
REVOKE CONNECT ON DATABASE godwit_store FROM PUBLIC;
```

**Scratch** — `db.t4g.micro`, `--backup-retention-period 0`, no deletion protection, `--skip-final-snapshot`, private, in one AZ. Cattle: every database on it is created and dropped inside the call that made it, and if you delete the instance the only thing to recreate is the role. It exists because `Diff` needs a `read` token and executes the DDL its caller submits, so that execution must reach nothing ([security](../../../docs/security.md#the-scratch-database)).

```sql
CREATE ROLE godwit_scratch LOGIN PASSWORD '...'
  CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS;
```

**Do not point `--scratch-dsn` at the RDS master user.** It carries `CREATEROLE`, which `serve`'s start-up probe treats as fatal — "has CREATEROLE, so submitted DDL can grant itself the memberships above" — and the task will not start. Create the role above with the master user and use that.

Size the scratch instance against `--max-concurrent-diffs` (4): each admitted `Diff`, `PlanRun`, `CreateRun`, `RevertRun` or `Checkpoint` builds four to five databases on it, and the pool that makes them is `max(4, 2 × --max-concurrent-diffs)` connections per task. `db.t4g.micro` has room for that; a directory with a thousand migrations replayed on every admission is CPU, so watch it there rather than in connections.

## Secrets

ECS injects secrets as **environment variables**, and that is the only place they can go — the image is distroless, so there is no shell and no entrypoint script to read a file. Four values:

| Variable | What |
|---|---|
| `PGPASSWORD` | the store password — see below |
| `GODWIT_SCRATCH_DSN` | the whole scratch DSN, password included |
| `GODWIT_MASTER_KEY` | 64 hex characters; needed only if any target uses the `static` provider |
| `GODWIT_TOKENS` | `name:scope:secret` entries, comma-separated |

One Secrets Manager secret holding a JSON object is the fewest moving parts; each `secrets[].valueFrom` names a key in it with the `:key::` suffix, as in [`taskdef.json`](taskdef.json):

```bash
aws secretsmanager create-secret --name godwit --secret-string '{
  "store_password": "...",
  "scratch_dsn": "postgres://godwit_scratch:...@godwit-scratch.abcdefghijkl.us-east-1.rds.amazonaws.com:5432/postgres?sslmode=require",
  "master_key": "'"$(openssl rand -hex 32)"'",
  "tokens": "ci:pipeline:...,ops:operator:...,register:admin:..."
}'
```

SSM Parameter Store is the cheaper alternative: one `SecureString` per value, and `valueFrom` becomes the parameter ARN (`arn:aws:ssm:us-east-1:123456789012:parameter/godwit/master-key`) with no key suffix. Either way the **execution role**, not the task role, is what reads them:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": "secretsmanager:GetSecretValue",
     "Resource": "arn:aws:secretsmanager:us-east-1:123456789012:secret:godwit-*"},
    {"Effect": "Allow", "Action": "kms:Decrypt",
     "Resource": "arn:aws:kms:us-east-1:123456789012:key/abcd-...",
     "Condition": {"StringEquals": {"kms:ViaService": "secretsmanager.us-east-1.amazonaws.com"}}}
  ]
}
```

Attach `AmazonECSTaskExecutionRolePolicy` alongside it for the image pull and CloudWatch Logs, and create the `/ecs/godwit` log group first — that policy grants `logs:CreateLogStream` and `logs:PutLogEvents` but not `logs:CreateLogGroup`, so `"awslogs-create-group": "true"` would need a statement of its own. The **task role** needs nothing for a plain deployment; it is where `roles/cloudkms`-style permissions would go if you used a KMS key provider, which on AWS today means neither of the two godwit ships (`gcpkms` and `vault-transit`), so a Secrets Manager `GODWIT_MASTER_KEY` is the AWS answer.

### Why the store password is a separate `PGPASSWORD`

`--store-dsn` is a required **flag** with no environment variable behind it, and ECS does **not** expand `$VARIABLE` in `command` — the array is passed to the binary as argv, with no shell in between. So the Helm chart's trick (`--store-dsn=$(GODWIT_STORE_DSN)`, which Kubernetes expands) has no equivalent here: whatever you write in `command` is what the process gets, and it is visible to anyone who can call `DescribeTaskDefinition`.

The way out is to put a password-less DSN in `command` and the password in the environment:

```
"--store-dsn=postgres://godwit@godwit-store...rds.amazonaws.com:5432/godwit_store?sslmode=require"
```

pgx falls back to the libpq environment for anything the DSN leaves out, so `PGPASSWORD` from Secrets Manager completes it. This was verified on the Compose stack: the store migrated with no password in the DSN. `--scratch-dsn` needs no such thing — it reads `GODWIT_SCRATCH_DSN` directly.

`PGPASSWORD` is process-wide, so it is also the fallback password for any *target* DSN that has none. Give every target DSN its own password, or use a credential provider.

**`sslmode=verify-full` needs a file the image does not have.** The distroless base carries the public CA bundle, and RDS certificates are signed by a private Amazon CA that is not in it. Either accept `sslmode=require` (encrypted, unauthenticated server), or build a two-line image on top of the published one and point `PGSSLROOTCERT` at the bundle:

```dockerfile
FROM ghcr.io/samuelmolling/godwit:sha-1a2b3c4
COPY global-bundle.pem /etc/ssl/rds/global-bundle.pem
```

```json
{"name": "PGSSLROOTCERT", "value": "/etc/ssl/rds/global-bundle.pem"}
```

## The load balancer and its health check

Two endpoints, and they are not interchangeable:

| Path | Answers | Use it for |
|---|---|---|
| `/healthz` | always `200 ok`, touching nothing | liveness — is the process alive |
| `/readyz` | `200` when the store answers a ping within two seconds, `503 store unavailable: …` otherwise | readiness — should this task get traffic |

The target group health check is a readiness check, so it is **`/readyz`**:

```bash
aws elbv2 create-target-group \
  --name godwit --vpc-id vpc-0123 --target-type ip \
  --protocol HTTP --port 8474 --protocol-version HTTP1 \
  --health-check-protocol HTTP --health-check-path /readyz \
  --health-check-interval-seconds 15 --health-check-timeout-seconds 5 \
  --healthy-threshold-count 2 --unhealthy-threshold-count 3 --matcher HttpCode=200
```

Know what you are choosing: ECS replaces a task the target group calls unhealthy, so a store outage long enough to fail three checks will cycle every task, and the tasks will come back no healthier. If you would rather ride out a store outage with tasks up and returning errors — and alert on `godwit_*` metrics instead — put `/healthz` on the target group. Neither endpoint is authenticated; neither is `/metrics`, which is a reason to keep the ALB internal or to route only `/ui` and `/godwit.v1.GodwitService` on a public one.

`--protocol-version HTTP1` is deliberate. Connect's unary calls and its server streams (`WatchRun`, and therefore `migrate`, `revert` and `run confirm`) work over HTTP/1.1; only bidirectional streams need HTTP/2, and godwit has none. HTTP/1.1 to the targets keeps the ALB health check, the browser UI and `curl` on one target group.

Two listener attributes matter:

- **`idle_timeout.timeout_seconds`** — 60 by default, 4000 maximum. `WatchRun` sends a frame every 500 ms, so a 24-hour run never goes idle; what does is a silent unary call. `PlanRun`, `CreateRun`, `Diff` and `Checkpoint` build scratch databases and replay the target's whole recorded history before answering a byte, and `--max-concurrent-diffs` can hold a call in a queue for a further 30 seconds. Set it to 900 or so; the ALB answers `504` when it fires.
- **`deregistration_delay.timeout_seconds`** buys nothing here. `serve` installs no signal handler, so a task being replaced exits the moment it gets `SIGTERM` and its in-flight streams die with it, whatever the ALB is waiting for.

## The service

Two tasks, because the lease needs somewhere to go: a task that dies mid-run loses its lease after `--lease-ttl` (30 s) and the other one claims the run and resumes it from the journal in the target database. With one task the run waits for the task to come back.

```bash
aws ecs register-task-definition --cli-input-json file://taskdef.json
aws ecs create-service --cli-input-json file://service.json
```

`minimumHealthyPercent: 100` with `maximumPercent: 200` starts the replacements before stopping the old tasks, so the lease always has a taker; the deployment circuit breaker rolls back a task definition that cannot pass `/readyz`. `availabilityZoneRebalancing` needs a 2024-or-later CLI; drop it on an older one and Fargate still spreads the two tasks across the subnets' AZs.

Each task's lease holder is `<name>/<16 hex characters>` — its hostname under `awsvpc`, where every task has its own ENI, plus a suffix drawn at start-up that keeps two tasks apart whatever their hostnames say. Nothing here needs setting; under `network_mode: host` the name is worth setting anyway, see below.

`linuxParameters.initProcessEnabled: true` is worth the line. Without it godwit is PID 1, and Go's runtime cannot die from an unhandled `SIGTERM` as PID 1, so it falls back to `exit(2)` — every scale-in and every deployment leaves a stopped task with exit code 2 in the console. With an init process in front, `SIGTERM` kills it normally and the exit code is 143. Both were measured; neither loses data, because an interrupted run is resumed from the journal.

That also makes **Fargate Spot** reasonable for this workload: a two-minute `SIGTERM` warning godwit ignores costs one lease TTL, and the surviving task finishes the run.

## Fargate or EC2

The task definition here is Fargate. What changes on EC2:

| | Fargate | EC2 |
|---|---|---|
| `requiresCompatibilities` | `["FARGATE"]` | `["EC2"]` |
| task-level `cpu` / `memory` | required | optional; use container `memoryReservation` for a soft limit |
| `networkMode` | must be `awsvpc` | `awsvpc`, `bridge` or `host` |
| target group `--target-type` | `ip` | `ip` with `awsvpc`, `instance` with `bridge` and a dynamic host port (`"hostPort": 0`) |
| `executionRoleArn` | required — it pulls the image, writes the logs and reads the secrets | still what resolves `secrets`; the instance role can cover the pull |
| `linuxParameters` | only `initProcessEnabled` | also `tmpfs`, `devices`, `sharedMemorySize`, `maxSwap`, `swappiness` |
| placement | automatic across the subnets' AZs | add `placementStrategy` spread on `attribute:ecs.availability-zone` and then `instanceId` |

**`host` network mode gives every container the instance's hostname**, so two godwit tasks on one instance report the same name. The lease is safe regardless — the holder carries a suffix drawn per process — but `cp_leases.holder` and the logs then name a machine rather than a task. Pass `--holder` (or `GODWIT_HOLDER`) something that distinguishes them, `ECS_TASK_ID` or the port, if you want to read those columns.

Nothing else in the container definition changes: `readonlyRootFilesystem: true` holds on both (godwit writes nothing to its filesystem), and there is no ECS **container** health check on either, because `healthCheck.command` runs inside the container and distroless has no shell. `["CMD", "/godwit", "version"]` would run, but it proves only that the binary starts — the listener is what you want checked, and the target group checks it.

## The pipeline

`godwit lint`, `plan` and `migrate` need a route to the API, and the CLI cannot use a TLS one: its transport dials cleartext even for `https://` URLs ([the detail](../kubernetes-ingress-nginx/README.md#reaching-the-api)). On AWS that means the CLI runs **inside the VPC** — a self-hosted GitHub runner, or CodeBuild with `vpcConfig` — against an internal ALB or the service's own address, over plain HTTP. From GitHub-hosted runners, reach the public ALB with `curl` against the connect JSON API instead, which works over HTTP/1.1 and TLS like any other HTTP call ([api.md](../../../docs/api.md) has one per RPC).

Whatever the route, the bearer token is the whole authorisation, so do not put the plaintext listener anywhere the token would cross the public internet.
