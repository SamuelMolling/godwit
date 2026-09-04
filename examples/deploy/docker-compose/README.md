# Docker Compose

The smallest deployment that is still a deployment: two godwit replicas, the store, the scratch server, and the secrets in a file the repository does not carry. It is not [`demo/`](../../../demo/README.md) — that stack builds the image from the working tree, hardcodes a master key and a token in the compose file, ships a target database and a dev-mode Vault, and exists to be killed. This one you can leave running.

**Assumes:** a host with Docker and Compose v2, and a `godwit` binary somewhere for the CLI half ([the quickstart](../../../README.md#quickstart) has `go install`).

**Leaves to you:** TLS, if anything but the host reaches the port; backups of the `store-data` volume; and the target databases, which are not part of this stack.

Files: [`compose.yaml`](compose.yaml), [`init/scratch.sh`](init/scratch.sh), and two environment files copied from [`env.example`](env.example) and [`godwit.env.example`](godwit.env.example).

## The two databases

`store` holds godwit's own control plane and is the only stateful thing here — it gets the named volume, and it is what you back up. `scratch` is where validation and `godwit diff` execute the SQL a caller submits; it has **no volume at all**, because every database on it is created and dropped inside the call that made it. Losing it costs the one role in `init/scratch.sh`.

They are separate services rather than two databases on one server because a `read` token is enough to reach that execution ([security](../../../docs/security.md#the-scratch-database)). `serve` checks the scratch role at start-up and refuses to run if it is a superuser or owns the store database, which is why `init/scratch.sh` spells out `CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS` and why the store's own role never gets `CREATEDB`.

The store role and database are made by the postgres image's own entrypoint from `POSTGRES_USER`, `POSTGRES_DB` and `POSTGRES_PASSWORD`, so no init script and no password in a committed file.

## Where the secrets come from

Two files, both `0600`, both outside git:

| File | Read by | Holds |
|---|---|---|
| `.env` | Compose itself, for `${...}` in `compose.yaml` | the three PostgreSQL passwords |
| `godwit.env` | the `godwit` containers, via `env_file` | `GODWIT_STORE_DSN`, `GODWIT_SCRATCH_DSN`, `GODWIT_MASTER_KEY`, `GODWIT_TOKENS` and the log settings |

```bash
cp env.example .env
cp godwit.env.example godwit.env
chmod 600 .env godwit.env
```

Then replace every `change-me`, and generate the real values:

```bash
openssl rand -hex 32   # GODWIT_MASTER_KEY: exactly 64 hex characters
openssl rand -hex 16   # one per token secret
```

`GODWIT_TOKENS` entries are `name:scope:secret` — three fields, always; a two-field entry is refused at start-up. Scopes are `read`, `pipeline`, `operator`, `admin`, cumulative in that order ([token spec](../../../docs/configuration.md#token-spec)).

### Why both DSNs are in the env file and not in `command`

`command` becomes the container's argv, which `docker inspect` and `docker ps --no-trunc` show to anyone who can reach the daemon. Both DSNs therefore live in `godwit.env`: `--store-dsn` falls back to `GODWIT_STORE_DSN` and `--scratch-dsn` to `GODWIT_SCRATCH_DSN`, so `compose.yaml` names no credential at all and every `change-me` is in one `0600` file.

## Run it

```bash
docker compose up -d
docker compose logs godwit
```

A clean start is two lines per replica and nothing else:

```
godwit-1  | level=INFO msg="store migrated" replica=f2b12aaf1d76 build=dev applied=17
godwit-1  | level=INFO msg=listening replica=f2b12aaf1d76 build=dev addr=[::]:8474 validation=true
```

A non-zero `applied` on one replica and `applied=0` on the other is the store migration racing correctly: both replicas run it, one applies the schema, the other finds nothing to do.

Then, from the host:

```bash
export GODWIT_SERVER=http://localhost:8474 GODWIT_TOKEN=<the admin secret>
godwit targets
godwit target add app --provider static --dsn 'postgres://app:app@app-db:5432/app?sslmode=disable'
godwit plan --target app --dir db/migrations
godwit migrate --target app --dir db/migrations
```

A target on another host needs a route from the `godwit` containers to it; a target in another Compose project needs its network attached to this one.

## Two replicas

`deploy.replicas: 2` with a **host port range** — `"8474-8475:8474"` — is what makes more than one replica work under Compose: each container takes the next free host port. The point is the lease, not throughput: a replica killed mid-run loses its lease after `--lease-ttl` (30 s) and the other one claims the run and finishes it from the journal in the target database. `demo/demo.sh` step 3 does exactly that with a `kill -9` if you want to watch it.

Each replica's lease holder name is its hostname, which under Compose is the container id, so they differ. Do not set `hostname:` on the service: two replicas answering to one name can heartbeat each other's leases, because `Heartbeat` matches on `(run_id, holder)` alone.

The port range is also why `compose.yaml` carries **two** `--ui-origin` values. `--ui-origin` is the allowlist of `Host` values the UI answers on at all, so a browser on `http://localhost:8475` gets `403 unknown host` unless that origin is listed too. Behind a proxy that fronts both replicas on one name, list that name instead.

Scale down to one for a single-host toy and the crash-safety story goes with it — the run waits for the container to come back.

## Health checks, restarts and logs

There is **no Compose healthcheck on `godwit`**, and there cannot be a useful one: the image is distroless, with no shell and no `curl`, so `CMD-SHELL` tests cannot run and `["CMD", "/godwit", "version"]` would prove only that the binary starts. Check it from outside instead — `/healthz` always answers `200 ok`, and `/readyz` answers `200` only when the store responds to a ping inside two seconds, so `/readyz` is the one worth polling.

`restart: unless-stopped` covers a crash and a host reboot. `docker compose stop` sends `SIGTERM`, which godwit handles: the listener stops, the replica stops claiming, the run it already holds is given until `--shutdown-timeout` (20 s) to finish, and the process exits **0**. Compose's own default is a 10-second wait before `SIGKILL`, which is why the service sets `stop_grace_period: 30s`. A run cut short either way is resumed from the journal by whichever replica claims it next.

`read_only: true` and `no-new-privileges` are safe because godwit writes nothing to its filesystem. Logs are JSON on stderr with rotation set on the json-file driver; set `GODWIT_LOG_FORMAT=text` in `godwit.env` while you are reading them by eye.

## The pipeline

The CLI here runs on the host against `http://localhost:8474`. A pipeline elsewhere needs a route to that port and, over anything public, TLS — which the binary does not do, so put a proxy in front and point the CLI at `--server https://…`, which it dials over TLS ([the detail](../kubernetes-ingress-nginx/README.md#reaching-the-api)). Reaching the plaintext port directly means a runner on the host or a private network between the runner and it.

## Verified

This stack was run: `docker compose up -d` on the image built from the working tree, both replicas healthy, then `godwit target add` (static provider, master key), `godwit plan` (scratch validation on the `scratch` service), `godwit migrate` and `godwit targets` against a throwaway target — `run … succeeded (attempt 1)`, one applied migration, drift `clean`. The scratch role passing the privilege check with no `not isolated` warning, `read_only: true` and the port range under `deploy.replicas: 2` were all observed in that run. The store DSN came from `PGPASSWORD` and a password-less `--store-dsn` then, and `SIGTERM` gave exit code 2; both were changed afterwards — `GODWIT_STORE_DSN` and the signal handler were verified as a real process against a real store, outside this stack.

Only the image reference differs from what was tested: `compose.yaml` names `ghcr.io/samuelmolling/godwit:main`, and the verification used a local build, because the published `main` tag was behind the working tree. Pin `sha-<short commit>` rather than `main` for anything you leave running.
