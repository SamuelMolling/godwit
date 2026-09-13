# Deployment

How a database becomes a target, where its credentials come from, what the service needs to run on Kubernetes, and what to watch once it is running for real. [Configuration](configuration.md) is the reference for every flag and variable; this page is the order to do things in and the reason you would change one.

The short version:

- A target is registered by **one API call**, `RegisterTarget`, which the CLI spells `godwit target add`. It needs the `admin` scope. **There is no way to register or edit a target from the web UI** — `/ui/targets` and `/ui/targets/{name}` are read-only pages.
- What godwit stores per target is a *provider name plus a small config map*, never a live credential unless you chose `static`.
- **A database that is not empty must be adopted before the first plan** — `godwit target adopt --version` when godwit never journalled it, `godwit target adopt --from-journal` when it carries a journal from elsewhere.
- For Vault you register a **credential store** — a named Vault with its address and its auth — and every `vault` target names one. The service has no Vault address of its own to fall back on. Then a policy with `read` on exactly one path per target, and a `--vault-template` that turns the secret's fields into a DSN.
- One godwit serving every application's database is the intended shape. Put the service in the shared stack; put the target registration, the migrations and the hook Jobs with the application.

## Registering a target

`RegisterTarget` is the only writer of `cp_targets`. Nothing registers a target implicitly: `CreateRun`, `PlanRun` and `GetTargetStatus` on an unknown name all fail with `not_found: target "x": not found`.

```bash
godwit credential-store add production \
  --server https://godwit.internal --token "$GODWIT_ADMIN_TOKEN" \
  --vault-addr https://vault.internal:8200 --vault-k8s-role godwit

godwit target add orders \
  --server https://godwit.internal --token "$GODWIT_ADMIN_TOKEN" \
  --provider vault --credential-store production \
  --vault-path secret/data/orders/db \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db.internal:5432/orders' \
  --lock-timeout 5s --statement-timeout 0 --search-path app,public
```

The same call over the API ([api.md](../internals/api.md#registertarget--admin)):

```bash
curl -s -X POST https://godwit.internal/godwit.v1.GodwitService/RegisterTarget \
  -H "Authorization: Bearer $GODWIT_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"orders","provider":"vault",
       "vaultPath":"secret/data/orders/db",
       "vaultTemplate":"postgres://{{username}}:{{password}}@orders-db.internal:5432/orders",
       "lockTimeout":"5s","searchPath":"app,public"}'
# {}
```

**`godwit.yaml` does not describe targets.** The only key `target add` reads from it is `server`; `target`, `dir`, `rollout` and the timeouts in `godwit.yaml` belong to the commands that plan and migrate, not to registration. A repository's `godwit.yaml` therefore looks like this and says nothing about credentials:

```yaml
server: https://godwit.internal
target: orders
dir: db/migrations
rollout: expand-contract
```

`--token` is read from the flag or `GODWIT_TOKEN`; it is never taken from `godwit.yaml`, so an admin secret cannot be committed by accident.

### What is stored and what is not

| Provider | `cp_targets.config` holds | The credential lives in |
|---|---|---|
| `static` | `dsn`: `godwit1:<key provider>:<key id>:<payload>`, sealed by the configured [key provider](security.md#the-key-and-where-it-comes-from) | the store, sealed |
| `kubernetes` | `path`: an absolute file path inside the godwit pod | a Kubernetes Secret you mount |
| `vault` | `path` (under `/v1/`) and, optionally, `template` | Vault |

Alongside the provider config, the same row carries the [target settings](configuration.md#target-settings) — `lock_timeout`, `statement_timeout`, `require_plan`, `keep_old`, `search_path`. Nothing else: no migration directory, no schema, no history. The history is in the target's own `godwit` schema, and the file bodies are in `cp_run_files` per run.

The registration does **not** connect to the database. A wrong DSN, an unreadable Vault path or a mistyped mount is accepted here and surfaces on the first call that needs the target — `godwit target status <name>` is the cheapest way to find out: it answers for a target whose credential does not resolve, with `its own journal was not read: <why>` and everything the control plane still knows.

### Re-registering changes what you pass

`RegisterTarget` is an upsert that **merges**: a setting you do not pass keeps the value the target has, and a setting you pass empty is cleared.

```
$ godwit target add orders --provider vault --credential-store production --vault-path ... --lock-timeout 5s
$ godwit target add orders --lock-timeout 10s   # only the lock timeout moves
$ godwit target add orders --lock-timeout ""    # and back to the executor's default
$ godwit target show orders                     # what it is now, credential excluded
```

Runs, plans, drift events and the target's applied history are untouched either way. A declarative owner of a target — an ArgoCD Job, a Terraform `null_resource`, a make target — should still keep the full `target add` line and re-run *that*, because merging means the row is no longer whatever the last invocation said; what it drops is the need to hunt the current values down before changing one of them. Re-registration is also how you move a `static` target between key providers by hand, though [key rotation](security.md#rotation) does not need it.

### Who runs it

`RegisterTarget` and `RegisterCredentialStore` are the two RPCs that need `admin`, and they are the two calls this page opens with. Give the admin secret to whatever registers targets and stores, and to nothing else; applications and pipelines get `pipeline`, humans get `operator`, pull requests get `read` ([token spec](configuration.md#token-spec)).

Once there is more than one, whatever registers them should be a Job rather than a person: put the full `target add` line in the repository that owns the target and run it from a Job with an `admin` token, and the upsert makes re-running it on every sync the point rather than a hazard.

**Not the godwit Helm chart, which declares no target and no store.** They are control-plane rows, and a values file that also holds them makes the values authoritative by force — a sync overwrites whatever anyone registered against the API, silently. [Decision 0022](../decisions/0022-control-plane-data-is-not-chart-configuration.md) states the rule and what it cost to learn.

Registering is not adopting: a target whose database already has a schema still needs `godwit target adopt` before its first plan, and the chart does not do it for you.

### The UI

`/ui/targets` lists every registered target with its provider, `search_path`, timeouts and `require_plan`; `/ui/targets/{name}` shows one target's applied migrations, pending set, ready plans and open drift. Both are `GET` routes. The only `POST` routes the UI serves are run actions, drift actions and `/ui/diff` — **there is no form that registers, edits or removes a target**, at any scope, including `admin`. This is deliberate: `RegisterTarget` and `RegisterCredentialStore` — the two `admin` RPCs — are the ones the UI never calls, so an `admin` browser session is worth no more than an `operator` one ([security](security.md#web-ui)).

There is also no `target remove` — deleting a target is a `DELETE FROM cp_targets` on the store, and the runs that reference it keep their rows.

## Adopting an existing database

Almost no first target is empty. Registering it is one call; putting what it already holds on godwit's books is the next one, and **it must happen before the first `plan`.** Which call depends on what the database carries, and you can tell in one query:

```bash
psql "$DSN" -c "SELECT version, name FROM godwit.migrations ORDER BY version"
```

| | Adopt with |
|---|---|
| the relation does not exist — the schema is there, godwit never touched it | `godwit target adopt --version <N>` |
| rows come back — another godwit instance migrated it, or this one's store was rebuilt | `godwit target adopt --from-journal` |

One command either way; the flag names where the truth comes from. Prefer `--from-journal` whenever the journal exists: there is no version for you to get wrong.

### No godwit journal: adopt at a version

Write a schema dump as the first migration in the directory, so the replay has something to build from, then name the version it stands for:

```bash
pg_dump --schema-only --no-owner --no-privileges "$DSN" > db/migrations/20260101000000_baseline.up.sql
echo '-- no inverse: this is where the history starts' > db/migrations/20260101000000_baseline.down.sql

godwit target adopt orders --dir db/migrations --version 20260101000000
# target orders: adopted through version 20260101000000, nothing executed (run 3f9c…)
```

Every migration at or below `--version` is written into the target's `godwit.migrations` with its checksum, without running it. `--version` is required: godwit will not guess how much of your directory the database already contains. Migrations above it are left for the next `godwit migrate`.

If the directory already reaches further than the dump — say the database is at `20260415…` and the files go up to `20260901…` — adopt through `20260415000000` and let `migrate` apply the rest.

### A godwit journal from elsewhere: adopt from the journal

The database has `godwit.migrations` rows this service's store knows nothing about. Check out the commit those migrations were applied from, and:

```bash
godwit target adopt orders --dir db/migrations --from-journal
# target orders: adopted 12 migration(s) from its journal (run 6f2c…): 20260101000000_baseline, …
```

`--from-journal` **reads the target and writes only the store**. It takes no `--version`: the target's own journal says what it holds. Run it again and it says `nothing to adopt, the ledger already holds every migration its journal records`.

It refuses, rather than guessing, when the two genuinely disagree — a file whose checksum is not the one the target recorded, a migration the target ran that your directory does not carry, or a migration the store thinks is applied that the target does not have. Each refusal names the migrations it means; [the runbook](runbook.md#the-ledger-is-behind-a-target) has the query for each and what to do.

### What happens if you skip this

The first `godwit plan` refuses and tells you:

```
failed_precondition: target records migrations the ledger does not: orders records 20260101000000_baseline, …;
run `godwit target adopt orders --from-journal --dir <migrations>` to adopt what it already has
```

That refusal exists because the control plane's ledger is what the out-of-order guard and the scratch replay read ([decision 0014](../decisions/0014-the-target-journal-is-authoritative.md)). Planning against a ledger that cannot see what the target already has produces a plan for a database that does not exist.

## The three credential providers

The provider is resolved on **every operation that touches the database**: a run attempt, a drift check, `target status`, an adoption, and the observation `CreateRun` takes at admission. It is not cached.

| | `static` | `kubernetes` | `vault` |
|---|---|---|---|
| Operator supplies | the DSN, once, on `target add` | a mounted Secret + `--secret-path` | a Vault path + a template + auth on the service |
| godwit stores | the encrypted DSN | the file path | the Vault path |
| Service needs | a [key provider](security.md#the-key-and-where-it-comes-from): `GODWIT_MASTER_KEY`, or `GODWIT_KEY_PROVIDER` with `GODWIT_KMS_KEY` | the volume mounted in the pod | a registered credential store, and the ServiceAccount token its login presents |
| Rotating the credential | re-register the target | `kubectl apply` the Secret; picked up on the next read, no restart | rotate in Vault; picked up on the next read, no restart |
| Blast radius of a store dump | every DSN, if the key leaks with it (`env`); with a KMS key provider, a live KMS call as well | nothing | nothing |

**Only `static` needs a key.** A deployment whose targets are all `kubernetes` or `vault` starts with no `GODWIT_MASTER_KEY` and no KMS at all — they store a path, not a secret. Set a key when you register your first `static` target, or you get `invalid_argument: static provider needs a key`.

### `static`

```bash
godwit target add orders --provider static --dsn 'postgres://godwit:secret@orders-db:5432/orders'
```

The DSN is sealed in the handler and the sealed value goes into `cp_targets.config`, prefixed by a header naming the key provider and key that opens it. It is the simplest provider and the only one where a store backup can be a target credential — [security](security.md#the-key-and-where-it-comes-from) says what each key provider costs there, and why `gcpkms` or `vault-transit` is the better answer than a key in an environment variable.

Rotating the key needs no re-registration: with a KMS provider the KMS rotates on its own, and with `env` you add the new key, keep the old one in `GODWIT_MASTER_KEY_PREVIOUS`, and every replica re-seals what it finds at start-up ([rotation](security.md#rotation)). A target sealed under a key that is *gone* is refused with

```
godwit: static target: this value is sealed under key a1b2c3d4; put that key in GODWIT_MASTER_KEY or GODWIT_MASTER_KEY_PREVIOUS
```

— at `CreateRun`, not at claim time: admission observes the target before it queues anything, so **no run row is created** and there is nothing to resume. `--skip-validation` does not change that; the observation happens either way. A run already queued when the key changed fails at claim instead, permanently (a decrypt error is not in the transient set), and `godwit run resume` picks it up once the key is back or the target re-registered.

### `kubernetes`

The DSN is a file. godwit reads it and trims trailing whitespace on every use.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: orders-db
  namespace: godwit
type: Opaque
stringData:
  dsn: postgres://godwit:secret@orders-db.internal:5432/orders
```

Mount it into the godwit pods through the chart, which has no dedicated value for this — `extraVolumes` and `extraVolumeMounts` are passed through verbatim:

```yaml
extraVolumes:
  - name: orders-db
    secret:
      secretName: orders-db
extraVolumeMounts:
  - name: orders-db
    mountPath: /etc/godwit/targets/orders
    readOnly: true
```

`--secret-path` is then the **absolute path of the key inside the pod**: the mount path joined with the Secret key, `/etc/godwit/targets/orders/dsn`. There is no search path and no relative resolution; the string is handed to `os.ReadFile` as it stands.

```bash
godwit target add orders --provider kubernetes --secret-path /etc/godwit/targets/orders/dsn
```

Two consequences worth knowing before you pick this provider. The file must exist in **every** replica, so adding a target means rolling the Deployment — which is what `vault` avoids. And the path is per-target: ten targets are ten volumes and ten mounts on one pod spec.

`readOnlyRootFilesystem: true` (the chart default) does not interfere; a Secret volume is its own mount.

### `vault`

The provider reads one secret from **the Vault the target's credential store names**, and renders a template over its fields.

```bash
godwit target add orders --provider vault --credential-store production \
  --vault-path secret/data/orders/db \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db.internal:5432/orders'
```

- `--credential-store` is **required**, and an unregistered name is refused at registration. There is no service-wide `VAULT_ADDR` standing behind a target that names none; a target registered before stores existed fails every run with `target <name>: this target names no credential store`, which is what an upgrade to this version looks like.
- `--vault-path` is the path **under `/v1/`**, exactly as `vault read` takes it. For KV v2 that means the `data/` segment: a secret written to `secret/orders/db` is read at `secret/data/orders/db`.
- The response's `data` is unwrapped once when it contains an inner `data` object, so KV v2 (`{"data":{"data":{...},"metadata":{...}}}`) and the flat responses of the database secrets engine both work with the same template.
- `--vault-template` substitutes `{{field}}` from the secret. Omitted, it defaults to `{{dsn}}` — so a KV secret with a single `dsn` field needs no template at all.
- Substitution is **one pass** and **string fields only**. A field whose value itself contains `{{...}}` is not rescanned; a numeric field such as KV metadata's `version` or the database engine's `ttl` cannot be templated and fails with `vault secret has no field for ttl` even though the field is present.
- A missing field fails with `vault secret has no field for <names>`, naming the *template's* keys and never the partially rendered string — the error reaches `cp_runs.error`, notifications and Slack, so it must carry nothing the secret held.

The full Vault setup is the next section.

## Vault, end to end

### The store, which is where a Vault's address lives

**The service holds no address for a target's Vault.** It holds a table of credential stores, and every `vault` target names one:

```bash
godwit credential-store add production \
  --vault-addr https://vault.production.internal:8200 --vault-k8s-role godwit
godwit credential-store add staging \
  --vault-addr https://vault.staging.internal:8200 --vault-k8s-role godwit

godwit credential-stores
```

This is what lets one service reach databases whose credentials live in different Vaults — the shape you get whenever Vault is deployed per environment. [Decision 0021](../decisions/0021-a-target-names-the-vault-its-credentials-live-in.md) is the reasoning, including why there is no global default to fall back on.

A store is a row, not process configuration: changing an address is `credential-store add` again, and every target naming it moves at the next resolve. No pod roll, no restart.

| Field | Meaning |
|---|---|
| `--vault-addr` | base URL, `http` or `https` |
| `--vault-k8s-role`, `--vault-k8s-mount` | Kubernetes auth **at that Vault**; the mount defaults to `kubernetes` ([security](security.md#credential-stores)) |
| `--vault-token-env` | instead of Kubernetes auth, the name of an environment variable of the service holding a token for that Vault. It must begin with `VAULT_TOKEN`. For deployments that are not on Kubernetes; prefer the role everywhere else |

Kubernetes auth presents **the ServiceAccount token minted for the audience `godwit`**, read from `/var/run/secrets/godwit/vault/token`. The chart projects it unconditionally, with no key to set: a deployment is one identity, and the audience is a constant in the pod spec and in the binary.

```yaml
serviceAccount:
  create: true
  # godwit calls no Kubernetes API; false takes the generic, audience-less token out of the pod
  automountServiceAccountToken: false
```

**The only thing an operator configures is `audience="godwit"` on the Vault Kubernetes auth role.** Nothing on godwit's side.

Register a store before the targets that name it: a `vault` target whose store does not exist is refused.

The JWT is re-read from disk on every login, so a projected token Kubernetes rotates is picked up without a restart. That is the reason to prefer a role over `--vault-token-env`: godwit never renews a static token, so the day its TTL runs out every target on that store stops resolving at once.

**There is no `VAULT_ADDR` on the service and no `vault:` block in the chart.** `serve.keyProvider.vaultAddr` and its siblings configure a different thing — the [`vault-transit` key provider](security.md#the-key-and-where-it-comes-from), where godwit seals the DSNs of `static` targets — and they reach no target.

**Each Vault a store names needs its own Kubernetes auth mount** that trusts this cluster, with a role bound to godwit's ServiceAccount and carrying `audience="godwit"`. A Vault that does not know this cluster answers the login with `403`, which is what a run on that store reports.

**Do not add `audience` to a role pods already authenticate with.** A Vault validating through the TokenReview API sends no `audiences` when its role requests none, and TokenReview then rejects an audience-bound token; set `audience` and the same role rejects the generic one. Neither side flips cleanly, so an existing deployment cuts over on a second role:

1. `vault write auth/kubernetes/role/godwit-audience ... audience=godwit`, same policies as the old role.
2. Upgrade the chart. The pod now presents the projected token, and stores still naming the old role start failing their logins.
3. `godwit credential-store add <store> --vault-addr ... --vault-k8s-role godwit-audience` for each store, then `godwit target status` on one of its targets.

Steps 2 and 3 are a window in which a run on those stores refuses; nothing applies with the wrong credentials. Delete the old role once no store names it.

Nothing here is a chicken and egg: registering a store is an API call carrying an `admin` bearer token, and it writes a row to the control-plane database. No Vault is needed to reach a Vault, and a service that has never had a store starts, serves and answers normally — every `vault` target on it simply refuses until one exists.

### The KV secret and the template

```bash
vault kv put secret/orders/db \
  username=godwit_orders \
  password="$(openssl rand -base64 24)"
```

```bash
godwit target add orders --provider vault --credential-store production \
  --vault-path secret/data/orders/db \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db.internal:5432/orders'
```

Only the parts that are secret belong in Vault. Host, port and database name are in the template, where a mistake shows up as a connection error rather than as a rewrite of the secret.

### The policy

One path, one capability. The godwit role must not be able to list the mount, read a sibling application's secret, or write anything:

```hcl
# policy "godwit"
path "secret/data/orders/db" {
  capabilities = ["read"]
}
path "secret/data/billing/db" {
  capabilities = ["read"]
}
```

godwit issues a plain `GET` and nothing else — no `list`, no `metadata`, no renew, no revoke — so `read` on each registered path is the whole requirement. A wildcard (`secret/data/+/db`) is a convenience that hands the service every future application's credential; write the paths out and add one when you add a target.

### Binding Kubernetes auth to godwit's service account

The chart's ServiceAccount is named after the release (`godwit` for `helm upgrade --install godwit`), or `serviceAccount.name` when you set it.

```bash
vault write auth/kubernetes/role/godwit \
  bound_service_account_names=godwit \
  bound_service_account_namespaces=godwit \
  policies=godwit \
  token_ttl=5m token_max_ttl=5m
```

**Keep `token_ttl` short.** godwit logs in, uses the client token for one `GET`, and throws it away — it never renews it and never revokes it. Every fetch therefore leaves a token lease behind that lives out its TTL in Vault. A one-hour `token_ttl` with the drift monitor below means dozens of live godwit tokens at any moment for no benefit; five minutes is generous.

### How often godwit asks Vault

Measured against a local Vault stand-in, one target, one replica:

| Operation | Login + read |
|---|---|
| `godwit migrate` (one run, start to finish) | 3 — the admission observation, the run claim, the post-run snapshot |
| `godwit drift check <target>` | 1 |
| `godwit target status <name>` | 1 |
| `godwit targets` / `ListTargets` | 0 — answered from the store alone |
| the drift monitor | 1 **per baselined target, per replica, per `--drift-interval`** |

That last row is the one that sizes everything. With the defaults — `--drift-interval 5m`, `replicaCount: 2` — a target that has ever completed a run or a baseline costs **576 Vault logins and reads a day**, and twenty targets cost eleven thousand. It is cheap for Vault; it is not cheap for Vault's audit device, and it is decidedly not cheap if each read mints a database role. Raise `--drift-interval` (`serve.driftInterval`) when the schema-drift signal is worth less to you than the traffic.

### Dynamic database credentials

[Security](security.md#credential-providers) recommends dynamic credentials, and they work: the database secrets engine's response is flat (`{"data":{"username":...,"password":...}}`), the unwrapping leaves it alone, and the template renders it.

```bash
vault write database/config/orders \
  plugin_name=postgresql-database-plugin \
  allowed_roles=godwit-orders \
  connection_url='postgresql://{{username}}:{{password}}@orders-db.internal:5432/orders' \
  username=vault_root password=...

vault write database/roles/godwit-orders \
  db_name=orders \
  creation_statements="CREATE ROLE \"{{name}}\" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}'; \
    GRANT CREATE ON DATABASE orders TO \"{{name}}\"; GRANT app_owner TO \"{{name}}\";" \
  default_ttl=26h max_ttl=26h
```

```hcl
path "database/creds/godwit-orders" { capabilities = ["read"] }
```

```bash
godwit target add orders --provider vault --credential-store production \
  --vault-path database/creds/godwit-orders \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db.internal:5432/orders'
```

The role the statements create needs what [security: database privileges](security.md#database-privileges) asks of any target role — whatever the migrations need, plus `CREATE` on the database so godwit can make its `godwit` schema on first contact.

**Choose the TTL against `--run-timeout`, not against your usual migration.** The mechanism, verified in the code:

- the DSN is resolved **once per run attempt**, and the executor opens **one** connection with it and holds that connection for the whole attempt;
- godwit never re-reads the credential mid-run, never renews the Vault lease, and never revokes it;
- a *retry* re-resolves it, so an attempt that fails transiently comes back with a fresh credential.

So the failure mode is a single attempt outliving its own credential — exactly the case the brief worries about: a `-- godwit: backfill` walking a large table for six hours under a one-hour lease. `--run-timeout` is 24h by default, which is the wall clock godwit itself allows one attempt; set the Vault role's `default_ttl` and `max_ttl` **at or above** it (26h above, leaving margin) so no credential can expire inside a permitted run. If you would rather bound it tighter, lower `--run-timeout` to the longest run you will actually allow and keep the TTL above *that* — the two numbers should be set together, not independently.

*Untested here:* what PostgreSQL does to godwit's open session when Vault revokes the role at lease expiry depends on your Vault version's revocation statements, and typically includes `pg_terminate_backend` on the role's backends. Assume the session dies. If it does, the statement fails, and godwit's own machinery does the right thing — a killed connection is classified as transient (`network`), the run goes back to `queued` with backoff, the next attempt resolves a fresh credential and the journal resumes from the last committed statement. What you lose is the in-flight statement, which for a `CREATE INDEX` without `CONCURRENTLY` or a large `ALTER` is not nothing.

**The cost dynamic credentials carry here** is the drift monitor: every check is a fetch, and every fetch of `database/creds/*` creates a new PostgreSQL role that lives until its TTL. A 26h TTL with the default five-minute interval and two replicas leaves roughly **six hundred live `v-godwit-orders-*` roles per target** in the cluster at steady state. PostgreSQL roles are cluster-wide and cheap but not free, `\du` becomes unusable, and Vault's lease-count quota is a real limit.

**The way out, and the recommendation for a first production deployment:** use a Vault **static role** instead of a dynamic one. One PostgreSQL role, whose password Vault rotates on a schedule, read from `database/static-creds/`:

```bash
vault write database/static-roles/godwit-orders \
  db_name=orders username=godwit_orders rotation_period=24h
```

```bash
godwit target add orders --provider vault --credential-store production \
  --vault-path database/static-creds/godwit-orders \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db.internal:5432/orders'
```

The response shape (`username`, `password`, plus numeric `ttl` / `rotation_period` fields the template ignores) renders correctly — verified. There is no lease per read, so the drift monitor costs nothing but an HTTP call; the credential still rotates without anyone touching godwit; and a rotation during a run cannot break it, because PostgreSQL does not re-authenticate an established session. Reach for `database/creds/` when a per-run identity in the target's `pg_stat_activity` and audit log is worth the role churn.

### When Vault is unreachable

| Vault's answer | godwit's behaviour |
|---|---|
| connection refused, DNS failure, timeout | classified transient: the run returns to `queued` with backoff and retries, up to `--max-attempts` |
| `403` on the read or the login | a plain error: the run finishes `failed`, `godwit run resume` after you fix the policy — or the store's role, if the login is what was refused |
| reachable at admission, gone at claim | the run exists and takes the rows above |
| gone at admission | `CreateRun` is refused; no run row exists |

## Deploying the service on Kubernetes

The chart in [deploy/helm/godwit](../../deploy/helm/godwit/README.md) assumes nothing about the platform beyond Kubernetes itself: routing is an Ingress or a Gateway API `HTTPRoute` or neither, the Secret is whatever object your cluster produces it with, and anything else the chart does not model goes in `extraObjects` as a raw manifest rendered with the release. What follows is ArgoCD-shaped because that is the setup the hooks below assume, but nothing before "Migrating on deploy" needs ArgoCD.

### What it needs before the first pod

1. **A PostgreSQL database for the store.** godwit creates its own tables but not the database — `CREATE DATABASE godwit_store` first, owned by the store role. That role needs nothing beyond ownership once step 2 is done.
2. **A second PostgreSQL for the scratch databases.** `Diff`, `PlanRun` and `CreateRun` execute caller-submitted SQL there, and `Diff` needs only `read` scope, so the weakest token in the system runs DDL of its author's choosing on that server. It holds nothing:

   ```sql
   CREATE ROLE godwit_scratch LOGIN PASSWORD '...'
     CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS;
   REVOKE CONNECT ON DATABASE godwit_store FROM PUBLIC;
   ```

   Then put its DSN in the Secret as `GODWIT_SCRATCH_DSN`. Left out, scratch databases stay on the store server under the store's own role, submitted DDL runs as the owner of the control plane, and every pod logs `scratch database is not isolated` on every start. That warning is accurate; do not ship staging without a scratch DSN if anyone but you holds a token.
3. **The Secret.** The chart never creates it, and reads it whole: every entry becomes an environment variable of that name, so what godwit reads from the Secret is configured in the Secret and never also in a value. `GODWIT_STORE_DSN` is the one entry it cannot start without, and a Secret that does not exist at all leaves the pod in `CreateContainerConfigError`; everything else in [configuration's table](configuration.md#environment) is optional and configures itself by being there — a missing `GODWIT_TOKENS` starts and logs `no tokens configured`. The master key is one of the optional ones — a deployment whose targets are all `vault` or `kubernetes` needs none, since [only `static` needs a key](#the-three-credential-providers) — and a deployment that does register `static` targets adds `--from-literal=GODWIT_MASTER_KEY=$(openssl rand -hex 32)` below.

   ```bash
   kubectl -n godwit create secret generic godwit \
     --from-literal=GODWIT_TOKENS='orders:pipeline:...,billing:pipeline:...,ops:operator:...,register:admin:...' \
     --from-literal=GODWIT_STORE_DSN='postgres://godwit:...@store.internal:5432/godwit_store' \
     --from-literal=GODWIT_SCRATCH_DSN='postgres://godwit_scratch:...@scratch.internal:5432/postgres'
   ```

   `GODWIT_TOKENS` is read at start-up only: adding an application's token is a pod roll. Plan for that when you add applications.

   The chart has no opinion about where those values come from. On a platform whose secrets arrive through an operator, put the object that produces them in `extraObjects` and it is rendered with the release — an `ExternalSecret`, a `VaultStaticSecret`, a `SealedSecret`, whatever your cluster runs:

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

### The values that matter

```yaml
replicaCount: 2

image:
  tag: sha-1a2b3c4        # pin; `main` moves
  pullPolicy: IfNotPresent

existingSecret:
  name: godwit

serve:
  driftInterval: 5m       # see "how often godwit asks Vault"
  limits:
    runTimeout: 24h       # the wall clock one attempt gets; pair it with the Vault TTL
    maxConcurrentRuns: 4  # per replica
  ui:
    enabled: true
    origins: ["https://godwit.staging.internal"]

notifications:
  publicUrl: https://godwit.staging.internal
  slack:
    channel: C0123456789

serviceMonitor:
  enabled: true
```

There is no `vault:` block: a target's Vault is a credential store registered against the API, and the chart has no value that reaches one ([the store, which is where a Vault's address lives](#the-store-which-is-where-a-vaults-address-lives)). `serve.keyProvider.vaultAddr` is the `vault-transit` key provider's own Vault and reaches no target.

Everything not modelled by the chart goes through `serve.extraArgs` — `--lease-ttl`, `--tick-interval`, `--max-attempts`, `--require-plan`, `--plan-ttl` and `--plan-retention` have no values of their own.

`--ui-origin` is not optional once something publishes `/ui`: it is both the allowlist of origins a form post may come from and the allowlist of hosts the UI answers on at all. Whatever publishes it terminates the TLS the plaintext listener does not; an ordinary HTTP backend carries the whole API, because connect's unary calls and its one server stream (`WatchRun`) both work over HTTP/1.1. An h2c or gRPC backend is only needed by a client that insists on HTTP/2, and a gRPC backend cannot share a hostname with `/ui`.

### Publishing the API

The chart renders either kind of route and neither by default. `ingress.*` is a `networking.k8s.io/v1` Ingress; `httpRoute.*` is a Gateway API `HTTPRoute` for a cluster that routes that way, with `parentRefs` naming the Gateway or `ListenerSet` it attaches to:

```yaml
httpRoute:
  enabled: true
  parentRefs:
    - { name: internal, namespace: gateway-system, sectionName: https }
  hostnames: [godwit.staging.internal]
```

Both are upstream kinds and neither carries an implementation's annotations. The Gateway, its listeners, its certificate and any implementation-specific policy stay outside — in `extraObjects` if you want them in this release, in the platform's own stack otherwise.

Nothing publishes godwit for the PreSync and PostSync hooks: those run in the cluster and reach `http://godwit.<namespace>.svc:8474` over the Service. A route exists for the browser UI and for a CLI on somebody's laptop, and a deployment that wants neither can leave both off.

### Publishing the GitHub App webhook

Different question, different answer. The API's route is for people inside; the App's webhook is the one address that has to be reachable from GitHub, and it must publish nothing else.

`serve.githubApp.enabled: true` opens a second listener serving `/github/webhook` and 404, and renders it a Service of its own — `<release>-webhook`, carrying `serve.githubApp.port` and no API port. Give it its own hostname on whatever Gateway faces the internet, one exact path, and a `backendRef` naming that Service:

```yaml
rules:
  - matches: [{ path: { type: Exact, value: /github/webhook } }]
    backendRefs: [{ name: godwit-webhook, port: 8475 }]
```

Two Services rather than two ports on one is the point: a Service is what a route attaches to, and this one has no API port, so the failure that matters — a public hostname that reaches the API — needs someone to name the other Service, not to mistype a port. [ci/platform-github-app-values.yaml](../../deploy/helm/godwit/ci/platform-github-app-values.yaml) is the whole shape, route and NetworkPolicy included.

With `serve.githubApp.enabled: false` (the default) none of it is rendered: no flag, no second container port, no Service. Registering the App itself is [creating the App](../use/github-app.md#creating-the-app).

The App's private key comes in either way, and `existingSecret.githubPrivateKey` chooses. It defaults to empty: no volume is mounted and the PEM arrives with everything else through `envFrom`, under the name `GODWIT_GITHUB_PRIVATE_KEY`, which is where a Secret written for godwit's environment already has it. Name an entry instead and it is projected as a file and read with `--github-private-key-file`, which keeps the PEM out of the environment that a sidecar, a core dump and `kubectl exec -- env` all read. Naming both is a start-up failure, not a precedence rule.

### Migrating on deploy: the PreSync and PostSync hooks

The wiring is in [deploy/argocd/](../../deploy/argocd/README.md); what matters when you are deciding how to lay this out:

```
PreSync   godwit migrate --target orders --dir /migrations --rollout expand-contract
Sync      the application rolls out on the expanded schema
PostSync  godwit run confirm --latest --allow-none --target orders
```

- both Jobs are ordinary `ghcr.io/samuelmolling/godwit` pods that only talk to the service — they hold a `pipeline` token, never a database credential;
- the migrations reach the PreSync Job through a ConfigMap rendered from the application's own `db/migrations`, so the hook applies exactly the revision ArgoCD is syncing;
- `backoffLimit: 0`, because the service already retries; a second Job would create a second run;
- the PreSync Job exits 0 on `succeeded` **or** `awaiting_contract`, 1 on `failed`/`needs_attention`, 3 when the plan the pull request stored is stale — and a non-zero exit fails the sync before any pod changes.

The rest of what the two manifests carry: `GODWIT_SERVER=http://godwit.<namespace>.svc:8474` and `GODWIT_TOKEN` from a Secret of the application's own (`orders-godwit`, key `token`, scope `pipeline`); `hook-delete-policy: BeforeHookCreation`; `activeDeadlineSeconds` 3600 on the PreSync Job and 600 on the PostSync one. The ConfigMap must hold both halves of every migration — the CLI loads the directory and refuses a version with a side missing. Replace `orders`, the Secret name and the image tag (`:main` in the examples; `sha-<short commit>` is the immutable one) for a reproducible hook. Runs these Jobs create carry `created_by = <token name>` and an empty `source` unless `--source` is added to the args.

**The PreSync run binds to the plan the pull request stored** without being told which: the ConfigMap holds the same `.up.sql` / `.down.sql` bodies as the repository, and the plan key is computed from those bodies, the target and the rollout. It matches as long as the pull request planned with the same `target` and `rollout` the Job passes.

**If the sync fails between the two hooks**, the run stays `awaiting_contract`: the old pods keep working against the expanded schema, and the next successful sync's PostSync confirms it — or an operator reverts it.

**One mismatch to fix for long migrations.** `presync-job.yaml` ships `activeDeadlineSeconds: 3600` while `--run-timeout` is `24h`. The Job is a client streaming a run that the *service* executes: when the deadline kills the Job pod, the run keeps going. The sync fails, ArgoCD reports the hook as failed, and the DDL is still being applied. Raise `activeDeadlineSeconds` above the longest migration you expect from that application, or accept that a long one will always report as a failed sync and be watched from `/ui` instead.

### Shared or per application

Deploy **one godwit** and register every application's database as a target on it. That is the design, not a compromise:

- **Targets are first class.** One store means `godwit targets`, `/ui`, `cp_audit` and the drift monitor are one view across every database. Per-application instances fragment exactly the things a platform team wants centralised.
- **The scheduler is already multi-tenant.** Runs are serialised *per target* by a lease row; `--max-concurrent-runs` (4 per replica) is what lets unrelated targets migrate in parallel. Nothing about a second application makes a second deployment necessary.
- **The expensive pieces are shared ones.** The scratch PostgreSQL and its `max_connections` and disk headroom, the store, the master key, the Vault role and policy, the route that publishes it, the ServiceMonitor and the alert rules — multiply them per application and you have multiplied the operational surface without reducing any risk.
- **Isolation is per token, not per deployment.** One `pipeline` token per application, named for it, is what makes `cp_runs.created_by` and `cp_audit.actor` mean something, and it rotates independently of every other application's.

So, for an `infra-tools`-style stack:

| In the shared stack | With the application |
|---|---|
| the `godwit` Application (chart, values, image pin) | the `orders-migrations` ConfigMap |
| the store and scratch PostgreSQL | the PreSync and PostSync hook Jobs |
| the `godwit` Secret (master key, tokens, store and scratch DSNs) | the `orders-godwit` Secret holding that application's `pipeline` token |
| the Vault policy and Kubernetes auth role | the Vault path holding the application database's credential |
| the route, ServiceMonitor and alert rules | the `godwit.yaml` in the application repository, and its GitHub Action steps |
| — | the Job that registers this target (`credential-store add`, `target add`, admin token) |

Sync the shared stack first — [examples/argocd/application.yaml](../../examples/argocd/application.yaml) puts `sync-wave: "-1"` on the godwit Application, because the PreSync hook fails the application's sync when the service is not there yet.

The honest costs of sharing, stated so you can decide rather than discover: one `admin` token registers every target, so it belongs to a Job and not to a person; a store dump plus the master key is every `static` target's credential, which is a reason to use `vault` or `kubernetes` and store nothing; and the per-replica `--max-concurrent-runs` and `--max-concurrent-diffs` are shared budgets, so a repository that opens forty pull requests an hour can make another team's plan wait. Raise the limits and the replica count before you consider a second deployment.

## A staging checklist

The smallest correct setup, in order. Steps 1–4 are the shared stack; 5–7 are the first application.

**1. Databases.** On the store server:

```sql
CREATE ROLE godwit LOGIN PASSWORD '...';
CREATE DATABASE godwit_store OWNER godwit;
REVOKE CONNECT ON DATABASE godwit_store FROM PUBLIC;
```

On a *different* PostgreSQL, the scratch one:

```sql
CREATE ROLE godwit_scratch LOGIN PASSWORD '...'
  CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS;
```

**2. Secret.**

```bash
kubectl create namespace godwit
kubectl -n godwit create secret generic godwit \
  --from-literal=GODWIT_MASTER_KEY=$(openssl rand -hex 32) \
  --from-literal=GODWIT_TOKENS='orders:pipeline:'"$(openssl rand -hex 16)"',ops:operator:'"$(openssl rand -hex 16)"',register:admin:'"$(openssl rand -hex 16)" \
  --from-literal=GODWIT_STORE_DSN='postgres://godwit:...@store:5432/godwit_store' \
  --from-literal=GODWIT_SCRATCH_DSN='postgres://godwit_scratch:...@scratch:5432/postgres'
```

**3. Vault** (skip for a first pass with `--provider static`, and come back — a `static` target needs the `GODWIT_MASTER_KEY` entry step 2 put in the Secret):

```bash
vault kv put secret/orders/db username=godwit_orders password='...'
vault policy write godwit - <<'EOF'
path "secret/data/orders/db" { capabilities = ["read"] }
EOF
vault write auth/kubernetes/role/godwit \
  bound_service_account_names=godwit bound_service_account_namespaces=godwit \
  audience=godwit \
  policies=godwit token_ttl=5m token_max_ttl=5m
```

`audience` is what makes the role answer *was this token issued for me*, and not only *who may log in*. Without it the role accepts any token the cluster issues for this ServiceAccount, including one godwit was tricked into presenting somewhere else. The string is `godwit` at every Vault — it is a constant of the deployment, not a per-Vault choice — and this is the one line of Vault configuration the change asks for.

**4. Install.**

```bash
helm upgrade --install godwit deploy/helm/godwit -n godwit \
  --set image.tag=sha-1a2b3c4
kubectl -n godwit logs deploy/godwit | grep -E 'listening|not isolated|no tokens'
```

A clean start logs `store migrated` and `listening`. Any `scratch database is not isolated` line means the Secret has no `GODWIT_SCRATCH_DSN` for step 1's second server; `no tokens configured` means the Secret's `GODWIT_TOKENS` is empty and every caller is an anonymous admin.

**5. Register the target.**

```bash
kubectl -n godwit port-forward svc/godwit 8474:8474 &
export GODWIT_SERVER=http://localhost:8474 GODWIT_TOKEN=<the register:admin secret>

godwit credential-store add production \
  --vault-addr https://vault.internal:8200 --vault-k8s-role godwit

godwit target add orders --provider vault --credential-store production \
  --vault-path secret/data/orders/db \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db.internal:5432/orders' \
  --lock-timeout 5s

godwit targets                 # the row exists, from the store alone
godwit target status orders    # this one actually reaches Vault and the database
```

`target status` failing here is the point of running it: `names no credential store` means the target was registered without `--credential-store`, `credential store "x": not found` means it names one nobody registered, `status 403` means the policy or the Kubernetes auth role, `no field for x` means the template, and a connection error means the host in the template.

Register the first one by hand — the feedback loop is faster and `target status` is the whole point. Once it answers, keep those two lines in the repository that owns the target and run them from a Job, not from a laptop. They do not go into the chart's values: the row they write is control-plane data, and the values file is not its second home ([decision 0022](../decisions/0022-control-plane-data-is-not-chart-configuration.md)).

**5b. Adopt what the database already has.** Skip only if it is genuinely empty; see [adopting an existing database](#adopting-an-existing-database).

```bash
psql "$DSN" -c "SELECT count(*) FROM godwit.migrations"   # relation missing → --version; rows → --from-journal
godwit target adopt orders --dir db/migrations --version <the version the schema stands at>
# or
godwit target adopt orders --dir db/migrations --from-journal
```

**6. First run, by hand, before any hook exists.**

```bash
export GODWIT_TOKEN=<the orders:pipeline secret>
godwit plan --target orders --dir db/migrations --save   # read scope is enough; --save is what stores it
godwit migrate --target orders --dir db/migrations
godwit run get <run-id>
```

**7. Then the hooks.** Copy `deploy/argocd/presync-job.yaml` and `postsync-confirm.yaml` into the application's chart, rename `orders`, create the `orders-godwit` Secret with the same `pipeline` token, add the ConfigMap that carries `db/migrations`, and let the next sync do it.

Turn `serve.ui.enabled` on once there is something to look at, with `serve.ui.origins` set to the host `ingress` or `httpRoute` publishes, and add the [alert rules](#metrics) — `GodwitRunNeedsAttention` and `GodwitQueuedNotClaimed` are the two that matter on day one.

## High availability

Two replicas is a floor, not a preference. The crash-safety story is a leased scheduler: a replica that dies mid-run loses its lease after `--lease-ttl` (30s) and *another replica* claims the run and resumes it from the journal in the target. With one replica there is no other replica, and the run waits for the pod to come back.

Every replica runs the same four loops: API, scheduler, drift monitor, validator. Nothing is elected.

- **Runs** are serialised per target by a lease row in `cp_leases` (one lease per target at a time, `FOR UPDATE SKIP LOCKED` on claim). Any replica claims a `queued` run, or a `running` one whose lease expired; a claim increments `attempts`. Heartbeats renew the lease every `lease-ttl/4`; a beat that fails is retried every `lease-ttl/10` until a fifth of the lease is left, and then the replica cancels the run it can no longer prove it owns rather than run past its lease. A replica that dies mid-run loses its lease after `--lease-ttl`; the next claim resumes from the journal in the target ([leases](../internals/runs.md#leases)). The holder is the process's identity (`<name>/<16 hex characters>`, drawn at start-up), not its hostname, so replicas that share a machine hold separate leases and a restarted one cannot reclaim its own run before the TTL.
- **Drift** is checked by every replica on its own `--drift-interval`; the partial unique index `cp_drift_events_open_idx` keeps duplicate open events out, so two replicas detecting the same diff produce one row.
- **Notifications** are per replica and in-memory (queue of 256 per provider); Slack thread ids live in `cp_notifications`, so any replica continues a thread.
- **Transient failures** (classes `08` connection, `53` insufficient resources — a full disk and an exhausted memory budget included — and `57` operator intervention, plus `40001`, `40P01`, `55P03`, `58000`, `58030`, connection resets, deadline exceeded): the run goes back to `queued` with `not_before = now + backoff` (`--tick-interval` doubled per attempt, capped at 5 minutes, ±20% jitter) and `retries + 1`; nobody is paged, `godwit_run_retries_total` counts it, the run's `error` starts with `transient:`. `57P04` (database dropped), `58P01` and `58P02` (a missing or duplicated file) are the exceptions inside those classes and are permanent, like any other SQL error: the run finishes as `failed` at once with `sql:` in front.
- **Store outage**: `/readyz` fails (it pings the store), the scheduler logs and retries on the next tick, in-flight statements finish or fail on their own timeouts. No run is lost: the journal is in the target, the run row in the store.

Each replica executes up to `--max-concurrent-runs` (4) runs at once, each on its own goroutine: the scheduler's ticker claims work and hands it off, so a run that takes an hour occupies one slot and the replica keeps claiming for every other target. A run that outlives `--run-timeout` (24h) is cancelled and finished as `failed` — raise it above the longest backfill you expect to run in one go, and remember that `statement_timeout: "0"` on a run means *no statement limit*, so this is the only wall clock such a run has.

Defaults that matter: `replicaCount: 2`, a PodDisruptionBudget with `minAvailable: 1`, soft pod anti-affinity, `terminationGracePeriodSeconds: 30`, `--shutdown-timeout` 20s. `SIGTERM` shuts the listener down, stops the replica claiming, and then waits for the runs it already holds: their leases keep beating, so nothing else may take them, and a run that finishes inside the window is recorded normally. A run that does not is cut when the timeout expires — it stays `running` until its lease expires (30s by default), then another replica takes it and `godwit_run_resumes_total{source="reconciler"}` goes up by one, which is also what happens on a `SIGKILL` or a lost node. Keep `--shutdown-timeout` under the platform's kill delay (`terminationGracePeriodSeconds`, ECS `stopTimeout`, systemd `TimeoutStopSec`), or the platform ends the process first and the drain buys nothing. Set `--lease-ttl` above your longest expected GC pause and below how long you tolerate a stalled run; `--max-attempts` (5) bounds how many attempts a run gets, whether they ended in a lost lease or a transient failure, before it is finished as `needs_attention` (`transient: gave up after N attempts`).

## The store

One PostgreSQL database, tables in the default schema of the store role, plus a `godwit` schema because the store migrates itself with the same engine.

| Table | Rows | Grows with |
|---|---|---|
| `cp_targets` | one per target; `config` holds the encrypted DSN or the provider config | targets |
| `cp_runs` | one per run: state, attempts, rollout, phase, `reverts`, timeouts, kind (`migrate`, `baseline`, `reconcile`), `created_by`, `source`, error | runs |
| `cp_run_files` | every migration file body sent with a run (the source of the bodies a replay and a revert use, narrowed by `cp_run_applied`) | runs × files; the largest table |
| `cp_run_applied` | one row per migration a run put on the books: order, whether its contract phase is held, whether the run merely `adopted` it from the target's own journal, the directive expansion frozen for it, and the revert that undid it. This is what the applied set and the scratch-validation replay are scoped to; a revert takes the same rows minus the adopted ones | runs × migrations applied |
| `cp_plans` | one per stored plan: key, rollout, state, observation, drift, directive expansions, the run it is bound to | `godwit plan --target --save` calls; swept by `--plan-retention` |
| `cp_plan_files` | the file bodies of a stored plan | plans × files; second largest |
| `cp_retired_columns` | one per `<c>_old` a completed `change-type` left behind, so `godwit diff` stops proposing to drop it; cleared by the revert or the `drop-column` that removes the column | `change-type` directives |
| `cp_leases` | one per claimed run | runs (never pruned; tiny) |
| `cp_snapshots` | one per target: schema fingerprint and definition after the last successful run or baseline | targets |
| `cp_drift_events` | one per detected diff, `resolved_at` when it goes away or is accepted | drift |
| `cp_notifications` | Slack root message ts per run / drift key | runs |
| `cp_audit` | one per admitted mutation (`target.register`, `target.baseline`, `target.reconcile`, `run.create`, `run.reattach`, `run.revert`, `run.resume`, `run.park`, `run.confirm`, `drift.accept`) | mutations |
| `cp_webhook_deliveries` | one per GitHub App delivery id, so a redelivery is not acted on twice | deliveries; swept at four times `--github-webhook-max-age` |
| `cp_github_runs` | one per run a pull request command created: which repository, pull request, head and check it answers on, and the last state it was reported at. Deleted with the run it names | runs the App created; swept once reported, on the same schedule as the deliveries |

Privileges for the store role: owner of the store database, and that is all it needs once `--scratch-dsn` points scratch databases elsewhere. Left unset, it also needs `CREATEDB` — and then submitted DDL executes as the owner of the control-plane database, which [security](security.md#the-scratch-database) explains and `serve` warns about on every start.

The scratch role wants `LOGIN CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS` and `CONNECT` on the database its DSN names. Validation creates `godwit_validate_<id>` there, replays the target's history from `cp_run_files` plus the new files, and drops it `WITH (FORCE)`; `Diff` creates up to five `godwit_diff_<id>` databases per call the same way. Without `CREATEDB` every `CreateRun` fails with `replay history ...` / `create scratch database` and the only way forward is `--skip-validation`, which is not the fix. `serve` refuses to start when the scratch role is a superuser, owns the store database, is a member of `pg_execute_server_program` / `pg_read_server_files` / `pg_write_server_files`, or holds `CREATEROLE` or `REPLICATION`; the message names each finding.

Connections: one pool per replica for the store, `--store-max-conns` wide (20 by default, and it wins over `pool_max_conns` in the DSN), a second pool for the scratch server when `--scratch-dsn` is set, sized `max(4, 2 × --max-concurrent-diffs)` from the concurrency gate that is its only source of demand, one single connection per claimed run, drift check, status inspection or baseline against the target, opened for the operation and closed after it, plus one connection to the scratch database per validation. Size `max_connections` on the **scratch** server for `replicas × (its pool + validations and diffs in flight)` and give it its own disk — a submitted schema decides how much it uses; the store sees only the replica pools. The store pool used to be unsized, which left it at pgx's `max(4, NumCPU)` — different on every node size, and small enough that a burst of `Diff` calls left the scheduler waiting in `Acquire`.

Sizing: the store is small. `cp_run_files` keeps the full text of every file for every run; a repository with 500 migrations of 2 KB sent on every run costs 1 MB per run. Prune it with the retention queries below rather than sending fewer files (the service needs the whole history for validation).

## Backups and PITR

godwit does not back anything up. Before these actions, take a backup or note a PITR restore point on the **target**:

- `godwit run confirm` (the contract phase runs the destructive statements you deferred);
- `godwit revert` (down migrations are typically `DROP`; godwit refuses one that would drop a non-empty table or column unless `--allow-data-loss`);
- `godwit migrate --ack H002,...` (any acknowledged destructive hazard);
- `godwit target adopt --version` (not destructive to data, but rewrites `godwit.migrations`).

```sql
-- on the target, right before the run
SELECT pg_create_restore_point('before-godwit-' || to_char(now(), 'YYYYMMDDHH24MISS'));
```

Restore the store together with the target when you roll back a target by PITR: a `cp_runs` row that says `succeeded` for a migration the restored target no longer has is exactly the situation [drift](runbook.md#drift-detected) and [checksum mismatch](runbook.md#checksum-mismatch) describe.

## Retention

There is no retention command; run these from cron or a scheduled Job. Never delete a run that still has standing `cp_run_applied` rows: validation replays those rows, reading their bodies out of that run's `cp_run_files`, to rebuild the target's history.

Store:

```sql
-- runs older than 90 days that are settled and hold nothing the target still has
WITH old AS (
  SELECT id FROM cp_runs r
  WHERE finished_at < now() - interval '90 days'
    AND state IN ('failed', 'reverted', 'needs_attention')
    AND NOT EXISTS (
      SELECT 1 FROM cp_run_applied a
      WHERE a.run_id = r.id AND a.reverted_by IS NULL)
)
DELETE FROM cp_run_files WHERE run_id IN (SELECT id FROM old);
-- repeat for cp_leases, cp_notifications (key = run_id::text), then cp_runs itself

-- resolved drift events
DELETE FROM cp_drift_events WHERE resolved_at < now() - interval '180 days';

-- audit trail (keep what your compliance window requires)
DELETE FROM cp_audit WHERE at < now() - interval '365 days';
```

The `NOT EXISTS` is the load-bearing part: a `failed` run that applied three migrations before it stopped still owns their history, so deleting its files takes the replay's bodies with it. Do not delete runs with `kind = 'migrate'` that still have standing rows unless the target has been adopted at a version since: [`BaselineTarget`](../internals/drift.md#adopting-an-existing-database), the RPC behind `godwit target adopt --version`, records its run as the new history root, after which older runs are no longer replayed. Runs of kind `baseline` and `reconcile` — the two an adoption writes — are history roots themselves and hold the only copy of the bodies they adopted; never delete them while their rows stand.

Target (`godwit` schema):

```sql
-- statement journal of finished runs; keep godwit.migrations and godwit.repeatables intact
DELETE FROM godwit.journal j USING godwit.runs r
WHERE j.run_id = r.id AND r.state = 'succeeded' AND r.finished_at < now() - interval '90 days';
DELETE FROM godwit.runs WHERE state = 'succeeded' AND finished_at < now() - interval '90 days';
```

Never delete a `running` or `failed` row from `godwit.runs`: the next attempt reopens it to know where to resume.

## Checkpoints

Reach for one when `godwit plan --target` has become slow and the log line for a plan shows the replay dominating it. The replay executes every migration the target has applied, on a fresh scratch database, before every plan — so its cost grows with the length of the history, not with the size of the change.

**Signals it is time.** A `godwit plan --target` on a pull request measured in minutes; a directory past a few hundred versioned files; the service logging `plan stored` long after the request came in. If the history is short and slow, a checkpoint will not help — the replay is executing real work, and the checkpoint's body would execute the same amount.

**How much it buys.** Two savings, and they are different sizes. The first is everything the history did and then undid: tables created and dropped, columns added and renamed, indexes rebuilt, `ALTER`s stacked on one table. A history with churn collapses hard — over 1000 churning migrations the scratch replay goes from 10.6 s to 0.14 s, and in the repository's own test a 24-migration churning history replays in about a quarter of the time, executing one migration instead of 24. The second is the per-migration overhead: the replay pays an advisory lock, a bootstrap, a run row and a `finalize` for every migration it executes, and a checkpoint pays them once for the whole collapsed range however additive it was. A purely additive history collapses into a body of roughly the same number of statements and still gains that: 1000 of them replay in 6.1 s whole and 3.2 s from the checkpoint. What it never buys is time the history spent doing real work: if the history is short and slow, the checkpoint's body executes the same work.

**Taking one.**

```
godwit checkpoint --name squash --dry-run   # read it first
godwit checkpoint --name squash
git add db/migrations/*_squash.up.sql && git commit
```

Then open it as an ordinary pull request: `lint` and `plan` run on it like any migration, and the plan says whether each target will run the checkpoint or record it.

**Before you merge it**, check `godwit targets`: every target's newest applied version must be at or above the checkpoint's `through=`, or the collapsed files must still be in the directory (they are, unless you deleted them). A target parked below the checkpoint with the files gone is refused at plan time, by name, and the way out is to restore the files or `godwit target adopt --version` it.

**After it is merged**, the migrations it collapsed are frozen: they can no longer be reverted on any target ([checkpoints](../internals/journal.md#checkpoints)), and `godwit revert` says so instead of running their down files. Keep the files in the repository until every target has passed the checkpoint; they are what carries a target that stopped below it.

**Checkpoint or baseline?** A baseline adopts *one target* whose schema godwit did not build, by writing its history without running anything; it is per target, it does not travel, and the directory it takes still has to contain a file describing that schema. A checkpoint changes *the directory*, for every target, and is generated from the files rather than from a database. Baseline when you are adopting an existing database; checkpoint when the replay of a history godwit itself built has got too long.

## Upgrades

1. Read the release notes for new store migrations (`internal/controlplane/schema.go`, `storeMigrations`).
2. Roll the image (`ghcr.io/samuelmolling/godwit`: `main` follows the branch, `sha-<short commit>` pins one build; set `image.tag` in the chart). On start every replica runs `Migrate` on the store under the store's own advisory lock; the first one applies, the others see nothing pending. The log line `store migrated` carries `applied=N`. Each store migration is one transaction: it commits with the row that records it, so a replica killed part way through leaves the store exactly as it was and the next start applies it whole.
3. Store migrations are forward-only in practice; `DownSQL` exists but no command applies it. To roll back a release, restore the store from backup taken before step 2.
4. Old and new replicas share the store during the rollout; keep migrations additive (they are).

Target-side `godwit` schema changes are bootstrapped with `CREATE ... IF NOT EXISTS` on first contact, no explicit step. It is one transaction under an advisory lock of its own, so replicas that meet a fresh target at the same moment queue rather than race, and a bootstrap that cannot finish leaves nothing half-created.

## Web UI

`serve --ui` mounts an operator UI at `/ui`. Sign-in and what each scope may press are in [security.md](security.md#web-ui); the pages themselves are read-mostly and every action they offer is an RPC the same token could call.

| Page | What it answers |
|---|---|
| `/ui/` | what is running, what needs a human, every run newest first; `?target=` filters. A running backfill carries its rows and batches under the state pill |
| `/ui/runs/{id}` | one run's timeline from `cp_audit`, the statement it is on, a live *Backfill* block while a batched statement runs, its error, the plan it is bound to, and resume / park / confirm / revert |
| `/ui/targets` | every registered target with its provider, `search_path`, timeouts, `require_plan`, applied count, ready plans, runs waiting for a human and open drift |
| `/ui/targets/{name}` | one target: what its journal has applied (checksum mismatches flagged) and its repeatables, what the newest ready plan still has to apply, the ready plans themselves, the open drift with check and accept, and the registered settings |
| `/ui/migrations` | which target has which migration, one row per migration **and** content: a version standing under two checksums is two rows and the target holding the other one reads *differs*. `?target=` narrows to what stands on one target, `?gaps=1` to what is not everywhere |
| `/ui/plans` | every stored plan newest first, filtered by target (`?target=`) and state (`?state=ready\|bound\|superseded`), with the key prefix, rollout, author, migration count and the run each one is bound to |
| `/ui/plans/{id}` | one plan in full: statements per migration grouped by phase, every hazard with its recipe, `already applied by hand` with the effect it recorded, the directives a migration carried and the expansion they produced, the observation the plan was taken against, and the drift the target had at that moment |
| `/ui/drift` | drift events per target, with check and accept-baseline |
| `/ui/diff` | the desired schema pasted as DDL against a target, answered with the up/down migration and the filenames to save it under; on a target that records repeatable migrations it supplies the `R__` pairs from the newest stored plan, the run that last succeeded, or boxes on the page |

The rail and every target list come from `ListTargets`, so a registered target that was never migrated appears from the moment it is registered. The plan list asks `ListPlans` once per target. A plan that retention has swept renders as *pruned* rather than a `404`: the run keeps the record of what it applied.

Both pages read `Run.progress`, which the executor reports after every committed batch and the scheduler saves at most once a second. Rows written and batches committed are counted; the total is `pg_class.reltuples` for the table taken once when the backfill started, so it is shown as `~n` and the percentage as `≈`, and a run can finish either side of it — the batch only touches rows that still need it. The 3s htmx poll the page already runs is what moves the numbers; a run that is not running shows no backfill block, because `cp_runs.progress` is cleared by every transition that starts or ends an attempt and a settled run therefore carries none.

`/ui/targets/{name}` takes its *pending* set from the target's newest **ready** plan, because the service holds no migration directory of its own; `godwit target status <name> --dir ./migrations` is the comparison against the files on disk, and it is also what flags a checksum mismatch.

## Admission limits

Set in [configuration](configuration.md#admission-limits); this is when to move them.

| Symptom | Raise |
|---|---|
| `invalid_argument: migration file <name> is N bytes, limit 4194304` | `--max-file-bytes`, and check whether the file is a schema dump that belongs in a `schema_source` instead |
| `invalid_argument: too many migrations: N, limit 2000` | `--max-migrations`, and `--max-request-bytes` with it; a directory past two thousand migrations is the only legitimate cause |
| `invalid_argument: too many migration files: N, limit 5000` | `--max-files`; at two files a migration the migration limit is reached first, so this one means the directory holds files that are not migration halves |
| `invalid_argument: schema is N bytes, limit 4194304` | `--max-file-bytes`; it bounds the desired schema `Diff` accepts as well |
| the client reports the message as too large before the service answers | `--max-request-bytes`, above the sum of what one run sends |
| `resource_exhausted: too many concurrent validation requests` on pull-request plans | `--max-concurrent-diffs`, and size the scratch server's `max_connections` and disk for it: each admitted call builds four to five databases there |
| a queued run waits while unrelated targets migrate | `--max-concurrent-runs`, and `--store-max-conns` with it |
| a long backfill is finished as `failed` with `context deadline exceeded` | `--run-timeout`, above the backfill's real duration |

`ListAudit` and `ListPlans` clamp `limit` to 1000 with no flag; page through with `--limit` and the newest-first order instead of asking for the whole table.

## Metrics

`GET /metrics`, Prometheus text format, unauthenticated, no access-log line. Every series is in `internal/metrics/metrics.go`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `godwit_build_info` | gauge | `version`, `commit` | always 1 |
| `godwit_runs` | gauge | `target`, `state` | current run count, computed at scrape time |
| `godwit_run_age_seconds` | gauge | `target`, `state` | age of the oldest run in that state |
| `godwit_run_resumes_total` | counter | `target`, `source` | `reconciler` (lease expired) or `manual` (`ResumeRun`) |
| `godwit_run_retries_total` | counter | `target`, `code` | transient failures retried on their own; `code` is the SQLSTATE, `network` or `timeout` |
| `godwit_run_attempts` | histogram | — | attempts a finished run took (buckets 1–5) |
| `godwit_heartbeat_failures_total` | counter | — | lease renewals that failed |
| `godwit_run_duration_seconds` | histogram | `target`, `result` | wall time of an attempt |
| `godwit_statement_duration_seconds` | histogram | `target`, `kind` | per statement, `tx` or `no_tx` |
| `godwit_statement_failures_total` | counter | `target`, `reason` | `lock_timeout`, `statement_timeout`, `sqlstate_<code>`, `error` |
| `godwit_hazards_total` | counter | `code`, `acked` | hazards seen at admission |
| `godwit_validation_failures_total` | counter | `target` | runs refused by the scratch replay |
| `godwit_drift_checks_total` | counter | `target`, `result` | `clean`, `drifted`, `accepted` |
| `godwit_api_requests_total` | counter | `method`, `code` | connect code per RPC |
| `godwit_api_request_duration_seconds` | histogram | `method` | |
| `godwit_notifications_total` | counter | `provider`, `result` | `delivered`, `failed`, `dropped` |
| `godwit_webhook_deliveries_total` | counter | `event`, `result` | GitHub App deliveries, including the ones refused before they were parsed |

Alert rules to start from:

```yaml
groups:
- name: godwit
  rules:
  - alert: GodwitRunNeedsAttention
    expr: sum by (target) (godwit_runs{state="needs_attention"}) > 0
    for: 1m
    labels: {severity: page}
    annotations: {summary: "godwit run on {{ $labels.target }} gave up; see runbook: needs_attention"}
  - alert: GodwitRunFailed
    expr: sum by (target) (godwit_runs{state="failed"}) > 0
    for: 5m
    labels: {severity: ticket}
  - alert: GodwitRunStuckRunning
    expr: godwit_run_age_seconds{state="running"} > 3600
    for: 5m
    labels: {severity: page}
  - alert: GodwitAwaitingContractTooLong
    expr: godwit_run_age_seconds{state="awaiting_contract"} > 86400
    labels: {severity: ticket}
  - alert: GodwitQueuedNotClaimed
    expr: godwit_run_age_seconds{state="queued"} > 60
    for: 2m
    labels: {severity: page}
    annotations: {summary: "no replica is claiming runs (scheduler down or store unreachable)"}
  - alert: GodwitLockTimeouts
    expr: increase(godwit_statement_failures_total{reason="lock_timeout"}[15m]) > 0
    labels: {severity: ticket}
  - alert: GodwitSchemaDrift
    expr: increase(godwit_drift_checks_total{result="drifted"}[10m]) > 0
    labels: {severity: ticket}
  - alert: GodwitHeartbeatFailures
    expr: increase(godwit_heartbeat_failures_total[10m]) > 0
    labels: {severity: ticket}
  - alert: GodwitNotificationsDropped
    expr: increase(godwit_notifications_total{result=~"dropped|failed"}[10m]) > 0
    labels: {severity: ticket}
  - alert: GodwitApiErrors
    expr: sum(rate(godwit_api_requests_total{code=~"internal|unavailable"}[5m])) > 0
    labels: {severity: ticket}
  - alert: GodwitDown
    expr: absent(godwit_build_info)
    for: 2m
    labels: {severity: page}
```

`GodwitRunFailed` is a ticket, not a page: a `failed` run is a migration that errored on SQL (`sql:` in its error); transient errors never reach `failed`, they retry with backoff and the run says `retrying` in the meantime. The pipeline that created it already failed loudly.

## Notifications

Configured by environment only ([configuration](configuration.md#environment)). Both providers receive the same events:

| `kind` | `type` |
|---|---|
| `run` | `created`, `running`, `retrying`, `succeeded`, `failed`, `needs_attention`, `awaiting_contract`, `confirmed`, `resumed`, `parked`, `reverted` |
| `drift` | `detected`, `resolved`, `accepted` |

**Webhook** (`GODWIT_WEBHOOK_URL`): one `POST` per event, `Content-Type: application/json`, 10s timeout, any status ≥ 300 counts as `failed`. Body:

```json
{
  "kind": "run",
  "type": "failed",
  "target": "orders",
  "run_id": "0d3c6c6e-3f9b-4b8a-9c8e-1d1f0c1b2a3c",
  "state": "failed",
  "attempt": 1,
  "rollout": "expand-contract",
  "phase": "expand",
  "actor": "ci",
  "detail": "statement 2: ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)",
  "at": "2026-09-02T03:14:15Z",
  "text": "godwit run failed on orders (run 0d3c6c6e): statement 2: ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)"
}
```

`text` is the one-line rendering; every other field is the event. Drift events carry `target`, `detail` (the diff or the acceptance) and no `run_id`.

**Slack** (`GODWIT_SLACK_TOKEN` + `GODWIT_SLACK_CHANNEL`): Block Kit messages. `GODWIT_SLACK_MODE=thread` (default) posts one root per run (`created`) and replies in its thread as it progresses, updating the root's state line; drift gets a fresh root per detection under key `drift:<target>`, with `resolved`/`accepted` as replies. `edit` mode keeps a single message per key and rewrites it (`chat.update`). Delivery retries three times on 429 (honouring `Retry-After`), 5xx and network errors with 1s/2s/4s backoff; `detail` is cut at 500 characters. With `GODWIT_PUBLIC_URL` set, every run message has an "Open run" button to `<url>/ui/runs/<id>`; the same setting, read from the CLI's own environment, is what links the run and its plan from [`godwit run report`](../use/cli.md#godwit-run-report). The bot needs `chat:write` in the channel.

Delivery is asynchronous: one worker per provider with a queue of 256 events; a full queue drops the event with `notification dropped` in the log and `result="dropped"` in the metric. Shutdown drains the queues.

## Logging

`--log-format json` (default) or `text`, `--log-level`. Every line carries `replica` (this process's lease identity, `<name>/<16 hex characters>` — the same string as `cp_leases.holder`, so a log line and a lease row can be matched) and `build`. What to grep for:

| Event | Level | Keys |
|---|---|---|
| `store migrated` | info | `applied` |
| `listening` | info | `addr`, `validation` |
| `api call` | info, warn on non-ok | `method`, `actor`, `scope`, `code`, `duration_ms`, `error` |
| `run created`, `revert created`, `run resumed`, `run parked`, `rollout confirmed`, `target registered`, `baseline accepted` | info | `run`, `target`, `actor` |
| `run refused by hazard gate`, `run refused by validation` | warn | `target`, `actor`, `error` |
| `run claimed` | info | `run`, `target`, `attempt`, `rollout`, `phase`, `reverts` |
| `run retrying` | warn | `run`, `target`, `attempt`, `wait`, `error` |
| `run re-attached` | info | `run`, `target`, `state`, `plan`, `resumed` |
| `run finished` | info, error when the run errored | `run`, `target`, `attempt`, `state`, `duration_ms`, `error` |
| `statement applied` / `statement failed` | debug / warn | `run`, `target`, `migration`, `stmt`, `kind`, `duration_ms`, `error` |
| `heartbeat lost` | warn | `run`, `error` |
| `drift checked` | debug clean, info drifted | `target`, `result` |
| `schema drift detected` / `schema drift resolved` | warn / info | `target` |
| `notification dropped` | warn | `provider`, `kind`, `type` |
| `audit write failed` | error | `action`, `error` |

Never logged: DSNs, tokens, master key, migration SQL text. `/metrics`, `/healthz` and `/readyz` produce no access-log line.

## Probes

`GET /healthz` returns 200 once the process listens. `GET /readyz` pings the store and returns 503 while it is unreachable; the Helm chart wires both. Kubernetes routing away from a replica whose store ping fails is what you want: its scheduler cannot claim anyway.

A replica migrates the store before it listens, so both probes fail while it does — including the wait for the advisory lock another replica is holding to migrate. The chart's `startupProbe` (`/healthz`, `periodSeconds: 5`, `failureThreshold: 60`) is what keeps that off the liveness budget: Kubernetes suspends liveness and readiness until it passes, and a replica that would have been killed part way through the wait now gets five minutes to come up. A release carrying a store migration slower than that needs a higher `startupProbe.failureThreshold`, not a longer liveness period.
