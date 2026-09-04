# A plain VM

One binary, one unit file, one environment file. **Untested:** the unit was not run on a Linux host. The flags, environment variables and endpoints in it were checked against `internal/cli/serve.go`, `internal/api` and `internal/server`, and the runtime behaviour was measured on the [Docker Compose](../docker-compose/README.md) stack; the systemd directives were not exercised.

**Assumes:** a systemd Linux host — 247 or later for the `ProtectProc` and `ProcSubset` lines, 240 for `Type=exec`; drop those three directives on anything older — and two PostgreSQL servers it can reach.

**Leaves to you:** the reverse proxy that terminates TLS, the monitoring that polls `/readyz`, and the target databases.

Files: [`godwit.service`](godwit.service) and [`godwit.env.example`](godwit.env.example).

## The binary

`go install github.com/SamuelMolling/godwit/cmd/godwit@main` needs Go 1.26 and a C toolchain, because the planner links libpg_query. On a host where you would rather not have either, lift the published binary out of the image — it is one file and it is the whole program:

```bash
id=$(docker create ghcr.io/samuelmolling/godwit:sha-1a2b3c4)
docker cp "$id:/godwit" /usr/local/bin/godwit
docker rm "$id"
chmod 0755 /usr/local/bin/godwit
/usr/local/bin/godwit version
```

Two things have to match. The architecture: the image is published for `linux/amd64` and `linux/arm64`. And **glibc**: cgo makes it a dynamically linked binary, and the published one asks for `libc.so.6` at `GLIBC_2.34` or newer (checked with `file` and `strings` on the extracted file). Debian 12, Ubuntu 22.04 and RHEL 9 clear that; Ubuntu 20.04 and Debian 11 do not, and musl hosts such as Alpine cannot run it at all. Build on the host, or run the container, when the host is older than that.

## The two databases

Same shape as everywhere else — a **store** godwit owns and migrates itself, and a **scratch** server where validation and `godwit diff` execute the SQL callers submit. Two servers, not two databases, because a `read` token is enough to reach that execution ([security](../../../docs/security.md#the-scratch-database)).

```sql
-- store server
CREATE ROLE godwit LOGIN PASSWORD '...' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
CREATE DATABASE godwit_store OWNER godwit;
REVOKE CONNECT ON DATABASE godwit_store FROM PUBLIC;

-- scratch server
CREATE ROLE godwit_scratch LOGIN PASSWORD '...'
  CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS;
```

`serve` inspects the scratch role at start-up and refuses to run if it is a superuser, owns the store database, or holds `CREATEROLE`, `REPLICATION` or the file-access memberships. Running both PostgreSQL servers on the same VM as godwit is possible and defeats the point of the second one; if disk is the reason, give scratch its own small host.

## The user, the files, the modes

```bash
useradd --system --no-create-home --shell /usr/sbin/nologin godwit
install -d -o root -g godwit -m 0750 /etc/godwit
install -o root -g godwit -m 0640 godwit.env /etc/godwit/godwit.env
install -o root -g root -m 0644 godwit.service /etc/systemd/system/godwit.service
systemctl daemon-reload
systemctl enable --now godwit
```

The service account owns nothing and logs in nowhere. It never needs write access to a directory: godwit keeps its state in the store and writes no files.

## Why the store password is in the environment file

`/proc/<pid>/cmdline` is world-readable on Linux; `/proc/<pid>/environ` is not. A DSN with a password in `ExecStart=` is therefore readable by every local user, and by anything that runs `ps`. The unit puts a password-less DSN on the command line

```
--store-dsn=postgres://godwit@store.internal:5432/godwit_store?sslmode=verify-full
```

and the password in `PGPASSWORD` in the environment file, which systemd reads as root and hands to the process. pgx completes the DSN from the libpq environment for any field it leaves out — verified against a real store. `--scratch-dsn` needs no trick: `GODWIT_SCRATCH_DSN` is read directly, so the whole DSN lives in the same file.

`PGPASSWORD` is process-wide, so it is also the fallback password for any *target* DSN that has none. Give every target DSN its own password, or use the `vault` or `kubernetes` credential provider.

`ProtectProc=invisible` in the unit hides *other* processes from godwit; it does not hide godwit's command line from them. Keeping the secret off the command line is what does that.

## Restarts, stops and logs

`Restart=always` with `RestartSec=5s` covers a crash and a reboot. Stopping is clean without any special handling: `serve` installs **no signal handler** — `cli.Main` runs `root.Execute()` with a background context, so the `srv.Shutdown` path in `server.Run` never fires — and under systemd the process is simply killed by `SIGTERM`, which systemd records as a clean stop. In a container the same code exits with status 2 instead, because Go cannot die from an unhandled signal as PID 1; on a VM you will not see that. Either way there is nothing to clean up: an interrupted run is resumed from its journal in the target database by whichever replica claims it next.

There is **no watchdog integration** — godwit does not call `sd_notify`, so `WatchdogSec` would kill a healthy service. A process that is up but wedged is caught from outside instead:

```bash
curl -fsS http://127.0.0.1:8474/readyz
```

`/readyz` returns `200` only when the store answers a ping within two seconds, and `503 store unavailable: …` otherwise; `/healthz` always returns `200 ok` and tells you only that the process is alive. Point a blackbox exporter, a monitoring agent or a `systemd` timer at `/readyz`, and scrape `/metrics` on the same port for the run and drift counters ([operations](../../../docs/operations.md#metrics)). All three are unauthenticated, which is one reason `--listen` is bound to `127.0.0.1` in the unit.

Logs go to stderr and journald picks them up. `GODWIT_LOG_FORMAT=json` is right when something ships them; set `text` while you are reading them by eye. There is no log file and no rotation to configure — that is `journald.conf`'s `SystemMaxUse`, or your shipper's problem.

## Publishing the UI

The listener is plaintext h2c and HTTP/1.1, and the unit binds it to loopback. Put nginx or Caddy in front for TLS:

```nginx
location /ui/ {
    proxy_pass http://127.0.0.1:8474;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_buffering off;
    proxy_read_timeout 900s;
    client_max_body_size 32m;
}
```

`--ui-origin` must name exactly the origin the browser uses. It is both the allowlist of origins a form post may come from **and** the allowlist of `Host` values the UI answers on at all: reached on any other host it returns `403 unknown host`, whatever the method. `proxy_set_header Host $host` keeps the browser's host intact, which is what makes the value in the unit match.

`proxy_buffering off` matters for the same reason it does on Kubernetes: `WatchRun` is a server stream and `migrate`, `revert` and `run confirm` read it. `client_max_body_size 32m` matches `--max-request-bytes`; below it nginx answers `413` before godwit sees the request.

## What a second machine buys you

The lease, and only the lease. Both machines run the identical unit, point `--store-dsn` at the **same** store, and need no coordination beyond it: each runs its own scheduler, each claims runs from the store, and one lease per target keeps them from colliding. A machine that dies mid-run loses its lease after `--lease-ttl` (30 s) and the other one claims the run and resumes it from the last committed statement in the target's journal. With one machine, the run waits for it to come back.

Two things follow:

- **The hostnames must differ.** The lease holder name is the machine's hostname, there is no flag for it, and `Heartbeat` matches on `(run_id, holder)` alone — two machines answering to one name can extend each other's leases, and both will execute the same run. Cloned VM images that keep the golden image's hostname are exactly how this happens.
- **API traffic can go to either**, in any proportion or none: the API is stateless and the scheduler runs on both whether or not anyone calls them. A load balancer in front is a convenience for the UI and the CLI, not a requirement for the lease.

`--drift-interval` is per replica: two machines fingerprint every baselined target twice per interval, and with a `vault` credential provider that is two Vault reads per target per interval ([deployment](../../../docs/deployment.md#how-often-godwit-asks-vault)).

## The pipeline

The CLI cannot use a TLS endpoint — its transport dials cleartext even for `https://` URLs ([the detail](../kubernetes-ingress-nginx/README.md#reaching-the-api)) — so a pipeline that runs `godwit` reaches the plaintext port over a private network: a runner on the machine itself, a runner on the same VPC or VPN, or an SSH tunnel. From anywhere else, drive the connect JSON API with `curl` over the TLS proxy ([api.md](../../../docs/api.md) has a call per RPC). The bearer token is the whole authorisation, so the plaintext port must never be the one crossing a public network.
