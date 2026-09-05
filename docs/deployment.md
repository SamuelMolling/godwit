# Deployment

How a database becomes a target, where its credentials come from, and what the service needs to run on Kubernetes. [Configuration](configuration.md) is the reference for every flag and variable; this page is the order to do things in.

The short version:

- A target is registered by **one API call**, `RegisterTarget`, which the CLI spells `godwit target add`. It needs the `admin` scope. **There is no way to register or edit a target from the web UI** — `/ui/targets` and `/ui/targets/{name}` are read-only pages.
- What godwit stores per target is a *provider name plus a small config map*, never a live credential unless you chose `static`.
- **A database that is not empty must be adopted before the first plan** — `godwit target baseline` when godwit never journalled it, `godwit target reconcile` when it carries a journal from elsewhere.
- For Vault you need `VAULT_ADDR` on the service, a way to authenticate (Kubernetes auth is the one to use), a policy with `read` on exactly one path per target, and a `--vault-template` that turns the secret's fields into a DSN.
- One godwit serving every application's database is the intended shape. Put the service in the shared stack; put the target registration, the migrations and the hook Jobs with the application.

## Registering a target

`RegisterTarget` is the only writer of `cp_targets`. Nothing registers a target implicitly: `CreateRun`, `PlanRun` and `GetTargetStatus` on an unknown name all fail with `not_found: target "x": not found`.

```bash
godwit target add orders \
  --server https://godwit.internal --token "$GODWIT_ADMIN_TOKEN" \
  --provider vault \
  --vault-path secret/data/orders/db \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db.internal:5432/orders' \
  --lock-timeout 5s --statement-timeout 0 --search-path app,public
```

The same call over the API ([api.md](api.md#registertarget--admin)):

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

The registration does **not** connect to the database. A wrong DSN, an unreadable Vault path or a mistyped mount is accepted here and surfaces on the first call that needs the target — `godwit target status <name>` is the cheapest way to find out.

### Re-registering replaces the whole row

`RegisterTarget` is an upsert that writes a **new config map**, not a patch. Every setting you do not pass again is dropped:

```
$ godwit target add orders --provider vault --vault-path ... --lock-timeout 5s
$ godwit targets     # LOCK 5s
$ godwit target add orders --provider vault --vault-path ...          # no --lock-timeout
$ godwit targets     # LOCK none
```

Runs, plans, drift events and the target's applied history are untouched — only `provider` and `config` are replaced. Keep the full `target add` line in the repository that owns the target (an ArgoCD Job, a Terraform `null_resource`, a make target) and re-run *that*, rather than typing a shorter version of it. Re-registration is also how you move a `static` target between key providers by hand, though [key rotation](security.md#rotation) does not need it.

### Who runs it

`RegisterTarget` is the only RPC that needs `admin`. Give the admin secret to whatever registers targets and to nothing else; applications and pipelines get `pipeline`, humans get `operator`, pull requests get `read` ([token spec](configuration.md#token-spec)).

Whatever registers them should be a Job rather than a person, and the Helm chart renders one from values so the set of targets is declared rather than typed. `targets.list` is a list of `target add` lines, `targets.tokenSecret` names the Secret holding the admin token, and the chart runs one invocation per entry — every entry but the last as an init container, since `target add` takes one target at a time and the image is distroless. It is idempotent by construction: the upsert makes re-running it on every sync the point rather than a hazard.

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

The Job is a Helm `post-install,post-upgrade` hook by default; `targets.helmHook: false` plus `targets.annotations: {argocd.argoproj.io/hook: Sync, argocd.argoproj.io/hook-delete-policy: BeforeHookCreation}` makes it an ArgoCD hook instead. Because the upsert replaces the whole row, that list has to be the *only* place those targets are registered — the row it writes is the row you get, and a flag somebody added by hand from a laptop is gone at the next sync.

Registering is not adopting: a target whose database already has a schema still needs `baseline` or `reconcile` before its first plan, and the chart does not do it for you.

### The UI

`/ui/targets` lists every registered target with its provider, `search_path`, timeouts and `require_plan`; `/ui/targets/{name}` shows one target's applied migrations, pending set, ready plans and open drift. Both are `GET` routes. The only `POST` routes the UI serves are run actions, drift actions and `/ui/diff` — **there is no form that registers, edits or removes a target**, at any scope, including `admin`. This is deliberate: `RegisterTarget` is the one RPC the UI never calls, so an `admin` browser session is worth no more than an `operator` one ([security](security.md#web-ui)).

There is also no `target remove` — deleting a target is a `DELETE FROM cp_targets` on the store, and the runs that reference it keep their rows.

## Adopting an existing database

Almost no first target is empty. Registering it is one call; putting what it already holds on godwit's books is the next one, and **it must happen before the first `plan`.** Which call depends on what the database carries, and you can tell in one query:

```bash
psql "$DSN" -c "SELECT version, name FROM godwit.migrations ORDER BY version"
```

| | Adopt with |
|---|---|
| the relation does not exist — the schema is there, godwit never touched it | `godwit target baseline` |
| rows come back — another godwit instance migrated it, or this one's store was rebuilt | `godwit target reconcile` |

### No godwit journal: baseline

Write a schema dump as the first migration in the directory, so the replay has something to build from, then name the version it stands for:

```bash
pg_dump --schema-only --no-owner --no-privileges "$DSN" > db/migrations/20260101000000_baseline.up.sql
echo '-- no inverse: this is where the history starts' > db/migrations/20260101000000_baseline.down.sql

godwit target baseline orders --dir db/migrations --version 20260101000000
# target orders: baselined to version 20260101000000 (run 3f9c…)
```

Every migration at or below `--version` is written into the target's `godwit.migrations` with its checksum, without running it. `--version` is required: godwit will not guess how much of your directory the database already contains. Migrations above it are left for the next `godwit migrate`.

If the directory already reaches further than the dump — say the database is at `20260415…` and the files go up to `20260901…` — baseline to `20260415000000` and let `migrate` apply the rest.

### A godwit journal from elsewhere: reconcile

The database has `godwit.migrations` rows this service's store knows nothing about. Check out the commit those migrations were applied from, and:

```bash
godwit target reconcile orders --dir db/migrations
# target orders: adopted 12 migration(s) from its journal (run 6f2c…): 20260101000000_baseline, …
```

Reconciling **reads the target and writes only the store**. It takes no `--version`: the target's own journal says what it holds. Run it again and it says `already reconciled, nothing to adopt`.

It refuses, rather than guessing, when the two genuinely disagree — a file whose checksum is not the one the target recorded, a migration the target ran that your directory does not carry, or a migration the store thinks is applied that the target does not have. Each refusal names the migrations it means; [the runbook](runbook.md#the-ledger-is-behind-a-target) has the query for each and what to do.

### What happens if you skip this

The first `godwit plan` refuses and tells you:

```
failed_precondition: target records migrations the ledger does not: orders records 20260101000000_baseline, …;
run `godwit target reconcile orders --dir <migrations>` to adopt what it already has
```

That refusal exists because the control plane's ledger is what the out-of-order guard and the scratch replay read ([decision 0014](decisions/0014-the-target-journal-is-authoritative.md)). Planning against a ledger that cannot see what the target already has produces a plan for a database that does not exist.

## The three credential providers

The provider is resolved on **every operation that touches the database**: a run attempt, a drift check, `target status`, a baseline, and the observation `CreateRun` takes at admission. It is not cached.

| | `static` | `kubernetes` | `vault` |
|---|---|---|---|
| Operator supplies | the DSN, once, on `target add` | a mounted Secret + `--secret-path` | a Vault path + a template + auth on the service |
| godwit stores | the encrypted DSN | the file path | the Vault path |
| Service needs | a [key provider](security.md#the-key-and-where-it-comes-from): `GODWIT_MASTER_KEY`, or `GODWIT_KEY_PROVIDER` with `GODWIT_KMS_KEY` | the volume mounted in the pod | `VAULT_ADDR` + a token or Kubernetes auth |
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

The provider reads one secret and renders a template over its fields.

```bash
godwit target add orders --provider vault \
  --vault-path secret/data/orders/db \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db.internal:5432/orders'
```

- `--vault-path` is the path **under `/v1/`**, exactly as `vault read` takes it. For KV v2 that means the `data/` segment: a secret written to `secret/orders/db` is read at `secret/data/orders/db`.
- The response's `data` is unwrapped once when it contains an inner `data` object, so KV v2 (`{"data":{"data":{...},"metadata":{...}}}`) and the flat responses of the database secrets engine both work with the same template.
- `--vault-template` substitutes `{{field}}` from the secret. Omitted, it defaults to `{{dsn}}` — so a KV secret with a single `dsn` field needs no template at all.
- Substitution is **one pass** and **string fields only**. A field whose value itself contains `{{...}}` is not rescanned; a numeric field such as KV metadata's `version` or the database engine's `ttl` cannot be templated and fails with `vault secret has no field for ttl` even though the field is present.
- A missing field fails with `vault secret has no field for <names>`, naming the *template's* keys and never the partially rendered string — the error reaches `cp_runs.error`, notifications and Slack, so it must carry nothing the secret held.

The full Vault setup is the next section.

## Vault, end to end

### Service configuration

Environment on the godwit pods, read **once at start-up**:

| Variable | Chart value | Meaning |
|---|---|---|
| `VAULT_ADDR` | `vault.addr` | base URL; without it every `vault` target fails `vault provider not configured: set VAULT_ADDR` |
| `VAULT_TOKEN` | `vault.tokenSecret.name` / `.key` | a static token; when set, no login happens |
| `VAULT_K8S_ROLE` | `vault.k8sRole` | the role for `POST auth/<mount>/login` |
| `VAULT_K8S_MOUNT` | `vault.k8sMount` | auth mount, default `kubernetes` |
| `VAULT_K8S_JWT` | — (use `extraEnv`) | service-account token file, default `/var/run/secrets/kubernetes.io/serviceaccount/token` |

Because they are read at start-up, changing `VAULT_ADDR` or `VAULT_TOKEN` needs a pod roll. The *service-account JWT* is different: it is re-read from disk on every login, so a projected token that Kubernetes rotates is picked up without a restart. That alone is a reason to prefer Kubernetes auth over `VAULT_TOKEN`: godwit never renews a static token, so the day its TTL runs out every `vault` target stops resolving at once.

Leave `vault.tokenSecret.name` empty and set `vault.k8sRole`:

```yaml
vault:
  addr: https://vault.internal:8200
  k8sRole: godwit
  k8sMount: kubernetes

serviceAccount:
  create: true
  automountServiceAccountToken: true   # the default; the provider reads the projected token
```

### The KV secret and the template

```bash
vault kv put secret/orders/db \
  username=godwit_orders \
  password="$(openssl rand -base64 24)"
```

```bash
godwit target add orders --provider vault \
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
godwit target add orders --provider vault \
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
godwit target add orders --provider vault \
  --vault-path database/static-creds/godwit-orders \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db.internal:5432/orders'
```

The response shape (`username`, `password`, plus numeric `ttl` / `rotation_period` fields the template ignores) renders correctly — verified. There is no lease per read, so the drift monitor costs nothing but an HTTP call; the credential still rotates without anyone touching godwit; and a rotation during a run cannot break it, because PostgreSQL does not re-authenticate an established session. Reach for `database/creds/` when a per-run identity in the target's `pg_stat_activity` and audit log is worth the role churn.

### When Vault is unreachable

| Vault's answer | godwit's behaviour |
|---|---|
| connection refused, DNS failure, timeout | classified transient: the run returns to `queued` with backoff and retries, up to `--max-attempts` |
| `403` on the read or the login | a plain error: the run finishes `failed`, `godwit run resume` after you fix the policy |
| reachable at admission, gone at claim | the run exists and takes the rows above |
| gone at admission | `CreateRun` is refused; no run row exists |

## Deploying the service on Kubernetes

The chart in [deploy/helm/godwit](../deploy/helm/godwit/README.md) assumes nothing about the platform beyond Kubernetes itself: routing is an Ingress or a Gateway API `HTTPRoute` or neither, the Secret is whatever object your cluster produces it with, and anything else the chart does not model goes in `extraObjects` as a raw manifest rendered with the release. What follows is ArgoCD-shaped because that is the setup the hooks below assume, but nothing before "Migrating on deploy" needs ArgoCD.

### What it needs before the first pod

1. **A PostgreSQL database for the store.** godwit creates its own tables but not the database — `CREATE DATABASE godwit_store` first, owned by the store role. That role needs nothing beyond ownership once step 2 is done.
2. **A second PostgreSQL for the scratch databases.** `Diff`, `PlanRun` and `CreateRun` execute caller-submitted SQL there, and `Diff` needs only `read` scope, so the weakest token in the system runs DDL of its author's choosing on that server. It holds nothing:

   ```sql
   CREATE ROLE godwit_scratch LOGIN PASSWORD '...'
     CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS;
   REVOKE CONNECT ON DATABASE godwit_store FROM PUBLIC;
   ```

   Then `serve.scratch.enabled: true`. Left off, scratch databases stay on the store server under the store's own role, submitted DDL runs as the owner of the control plane, and every pod logs `scratch database is not isolated` on every start. That warning is accurate; do not ship staging without `--scratch-dsn` if anyone but you holds a token.
3. **The Secret.** The chart never creates it. The Deployment references `tokens` and `storeDSN` unconditionally, and a Secret missing either leaves the pod in `CreateContainerConfigError`. The master key is *not* one of them: `existingSecret.keys.masterKey` defaults to empty and is wrapped in a `with`, so `GODWIT_MASTER_KEY` is omitted unless you ask for it — which is what a deployment whose targets are all `vault` or `kubernetes` wants, since [only `static` needs a key](#the-three-credential-providers). A deployment that does register `static` targets adds `--from-literal=GODWIT_MASTER_KEY=$(openssl rand -hex 32)` below and `existingSecret.keys.masterKey: GODWIT_MASTER_KEY` in the values.

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
  scratch:
    enabled: true
  driftInterval: 5m       # see "how often godwit asks Vault"
  limits:
    runTimeout: 24h       # the wall clock one attempt gets; pair it with the Vault TTL
    maxConcurrentRuns: 4  # per replica
  ui:
    enabled: true
    origins: ["https://godwit.staging.internal"]

vault:
  addr: https://vault.internal:8200
  k8sRole: godwit

notifications:
  publicUrl: https://godwit.staging.internal
  slack:
    channel: C0123456789

serviceMonitor:
  enabled: true
```

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

### Replicas and the lease

Two replicas is a floor, not a preference. The crash-safety story is a leased scheduler: a replica that dies mid-run loses its lease after `--lease-ttl` (30s) and *another replica* claims the run and resumes it from the journal in the target. With one replica there is no other replica, and the run waits for the pod to come back.

The chart's defaults line up with that: `podDisruptionBudget.minAvailable: 1` so a node drain never takes both, soft pod anti-affinity so they are not on one node, `terminationGracePeriodSeconds: 30` against a `--shutdown-timeout` of 20s. A rolling update on a replica holding a run is normal operation: on `SIGTERM` the replica stops serving, stops claiming, and keeps the run it holds — its lease still beating — until the run finishes or the shutdown timeout ends it. A run that outlives the timeout stays `running` until its lease expires, the other replica takes it, and `godwit_run_resumes_total{source="reconciler"}` counts it.

### Migrating on deploy: the PreSync and PostSync hooks

The wiring is in [deploy/argocd/](../deploy/argocd/README.md) and [ci-cd.md](ci-cd.md#argocd); what matters when you are deciding how to lay this out:

```
PreSync   godwit migrate --target orders --dir /migrations --rollout expand-contract
Sync      the application rolls out on the expanded schema
PostSync  godwit run confirm --latest --allow-none --target orders
```

- both Jobs are ordinary `ghcr.io/samuelmolling/godwit` pods that only talk to the service — they hold a `pipeline` token, never a database credential;
- the migrations reach the PreSync Job through a ConfigMap rendered from the application's own `db/migrations`, so the hook applies exactly the revision ArgoCD is syncing;
- `backoffLimit: 0`, because the service already retries; a second Job would create a second run;
- the PreSync Job exits 0 on `succeeded` **or** `awaiting_contract`, 1 on `failed`/`needs_attention`, 3 when the plan the pull request stored is stale — and a non-zero exit fails the sync before any pod changes.

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
| the registration Job (`godwit target add` per target, admin token) | — |

Sync the shared stack first — [examples/argocd/application.yaml](../examples/argocd/application.yaml) puts `sync-wave: "-1"` on the godwit Application, because the PreSync hook fails the application's sync when the service is not there yet.

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

**3. Vault** (skip for a first pass with `--provider static`, and come back — a `static` target also needs `--set existingSecret.keys.masterKey=GODWIT_MASTER_KEY` in step 4, since the chart wires no key by default):

```bash
vault kv put secret/orders/db username=godwit_orders password='...'
vault policy write godwit - <<'EOF'
path "secret/data/orders/db" { capabilities = ["read"] }
EOF
vault write auth/kubernetes/role/godwit \
  bound_service_account_names=godwit bound_service_account_namespaces=godwit \
  policies=godwit token_ttl=5m token_max_ttl=5m
```

**4. Install.**

```bash
helm upgrade --install godwit deploy/helm/godwit -n godwit \
  --set image.tag=sha-1a2b3c4 \
  --set serve.scratch.enabled=true \
  --set vault.addr=https://vault.internal:8200 \
  --set vault.k8sRole=godwit
kubectl -n godwit logs deploy/godwit | grep -E 'listening|not isolated|no tokens'
```

A clean start logs `store migrated` and `listening`. Any `scratch database is not isolated` line means step 1's second server is not wired; `no tokens configured` means the Secret's `GODWIT_TOKENS` is empty and every caller is an anonymous admin.

**5. Register the target.**

```bash
kubectl -n godwit port-forward svc/godwit 8474:8474 &
export GODWIT_SERVER=http://localhost:8474 GODWIT_TOKEN=<the register:admin secret>

godwit target add orders --provider vault \
  --vault-path secret/data/orders/db \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db.internal:5432/orders' \
  --lock-timeout 5s

godwit targets                 # the row exists, from the store alone
godwit target status orders    # this one actually reaches Vault and the database
```

`target status` failing here is the point of running it: `vault provider not configured` means `VAULT_ADDR` is unset, `status 403` means the policy or the Kubernetes auth role, `no field for x` means the template, and a connection error means the host in the template.

Register the first one by hand — the feedback loop is faster and `target status` is the whole point. Once it answers, move the same line into `targets.list` in the values so the next sync owns it, and remember that the list then replaces the row entirely: whatever you typed here has to be in it.

**5b. Adopt what the database already has.** Skip only if it is genuinely empty; see [adopting an existing database](#adopting-an-existing-database).

```bash
psql "$DSN" -c "SELECT count(*) FROM godwit.migrations"   # relation missing → baseline; rows → reconcile
godwit target baseline orders --dir db/migrations --version <the version the schema stands at>
# or
godwit target reconcile orders --dir db/migrations
```

**6. First run, by hand, before any hook exists.**

```bash
export GODWIT_TOKEN=<the orders:pipeline secret>
godwit plan --target orders --dir db/migrations     # read scope is enough; stores the plan
godwit migrate --target orders --dir db/migrations
godwit run get <run-id>
```

**7. Then the hooks.** Copy `deploy/argocd/presync-job.yaml` and `postsync-confirm.yaml` into the application's chart, rename `orders`, create the `orders-godwit` Secret with the same `pipeline` token, add the ConfigMap that carries `db/migrations`, and let the next sync do it.

Turn `serve.ui.enabled` on once there is something to look at, with `serve.ui.origins` set to the host `ingress` or `httpRoute` publishes, and add the [alert rules](operations.md#metrics) — `GodwitRunNeedsAttention` and `GodwitQueuedNotClaimed` are the two that matter on day one.
