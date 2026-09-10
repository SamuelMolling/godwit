# 0021 — A target names the Vault its credentials live in, and there is no other one

Shipped in #136. It removes `VAULT_ADDR` and its siblings from the credential path entirely and replaces them with a registered credential store per target.

## The defect, found against a real deployment

godwit had **one** `VAULT_ADDR`, read once at start-up and handed to every `vault` target.

The deployment it was pointed at does not have one Vault. Vault is deployed per environment, and the module that provisions each database's `database/creds/migrate-*` role is applied with `provider "vault" { address = "http://vault.${local.environment}.internal" }`. So the production clusters' roles live in the production Vault and the staging clusters' live in the staging Vault, and one process cannot be pointed at both. Pointed at staging it reached the staging databases and could not reach production; pointed at production, the reverse. **No configuration of that service could manage a production database**, and the deployment is deliberately one instance serving both.

That is not a Vault problem. It is a process-wide setting deciding a per-target fact, and `internal/creds` had already drawn the line correctly everywhere else: `Provider.DSN(ctx, config)` takes the *target's* config, which is where `vault_path` and the template already lived. Only the address and the auth had been left in the environment.

## The decision

**A `vault` target names a credential store, and a credential store is the only thing that says which Vault a secret is read from.**

```
godwit credential-store add production \
  --vault-addr https://vault.production.internal --vault-k8s-role godwit

godwit target add orders --provider vault --credential-store production \
  --vault-path database/creds/migrate-orders \
  --vault-template 'postgres://{{username}}:{{password}}@orders-db:5432/orders'
```

The shape is External Secrets' `ClusterSecretStore`, which this team already runs and reads: a named credential source, registered once, referenced by many consumers, carrying the endpoint and the identity to present there. Two databases behind the same Vault with the same auth configure it once.

### There is no global fallback, and that is the point

A target that names no store **fails**, and the failure names the target and the commands that fix it:

```
target orders: this target names no credential store, and godwit reads no Vault without one:
register the Vault its credentials live in with `godwit credential-store add <store> --vault-addr=... --vault-k8s-role=...`,
then point the target at it with `godwit target add <target> --provider=vault --vault-path=... --credential-store=<store>`
```

A fallback would have kept the defect alive as the default path, and its failure mode is the worst available: a target silently resolving against the wrong Vault, either failing to authenticate for reasons that point nowhere or — worse — succeeding against a Vault nobody meant. `RegisterTarget` therefore refuses a `vault` target with no store, so the only way to reach the run-time error is a row registered before this change.

### The environment variables are gone from the credential path, not renamed

`VAULT_ADDR`, `VAULT_TOKEN`, `VAULT_K8S_ROLE`, `VAULT_K8S_MOUNT` and `VAULT_K8S_JWT` are read by nothing in the target path. The chart's `vault:` block is deleted. Every field they held now belongs to a store — including the ServiceAccount token file, which is per store rather than process-wide even though a pod has one identity, because that is the last thing that would otherwise have to be configured somewhere else.

**`vault-transit` is a different feature and keeps working, with settings of its own.** It is a *key* provider: it seals the DSNs of `static` targets, and it needs a Vault of its own. It used to read `VAULT_ADDR` — sharing configuration with the credential path by accident of history. It now reads `GODWIT_VAULT_TRANSIT_ADDR`, `GODWIT_VAULT_TRANSIT_TOKEN`, `GODWIT_VAULT_TRANSIT_K8S_ROLE`, `GODWIT_VAULT_TRANSIT_K8S_MOUNT` and `GODWIT_VAULT_TRANSIT_K8S_JWT`. It inherits nothing: a deployment that used it and upgrades fails at start-up with `key provider vault-transit needs GODWIT_VAULT_TRANSIT_ADDR`, which is the loud version of the only alternative, which was to keep working by reading a variable that now means something else.

### `static` and `kubernetes` are untouched, and need no store

A store carries an endpoint and an identity to present there. `static` has neither — its DSN is sealed in the target's own config by the [key provider](0012-the-key-is-optional-and-comes-from-a-provider.md). `kubernetes` has neither — it reads a file the kubelet already put in the pod. Forcing them through a store would be a required field with nothing in it. They register exactly as before, `credential_store` on either is refused with a message saying so, and `godwit targets` shows them as `none`.

### There is no kind field

A store today is a Vault: the fields are `vault_addr`, `vault_k8s_role`, `vault_k8s_mount`, `vault_k8s_jwt`, `vault_token_env`, and they are named for what they are. A `provider` column with one legal value would be scaffolding for a second credential source that does not exist, and it would carry no weight today — every check it could perform is vacuous while there is one kind.

The extensibility that was actually asked for survives without it. A target already names its provider, and `credential_store` is already just a name, so a GCP Secret Manager source arrives as a new `creds.Provider` plus its own fields on the store — **and no registered target changes**. That is the requirement; the field was only ever one way to meet it, and the more expensive one.

### Constraining what a store may point at

Registering a store is `admin`, so it is already privileged. State plainly what it obtains anyway:

- **the pod's ServiceAccount token**, POSTed to the store's address at the first resolve. That token is the identity every *other* Vault knows godwit by, so its holder can log in at the production Vault under godwit's role and read the credentials of every database godwit migrates. That is the escalation — not the JWT, but what it exchanges for.
- **a named environment variable's value**, with `--vault-token-env`. The required `VAULT_TOKEN` prefix exists for exactly this: without it an admin could name `GODWIT_STORE_DSN` and receive the control plane's own credentials.
- **the DSN godwit connects with**, since the store's Vault answers with it. A hostile store can point a production target at a database it controls and receive that target's migrations.

The second and third are not new in kind — an admin can already register a `static` target with a DSN of their choosing. The first is.

**The position: an operator-set host allowlist, and a scheme check. Nothing else.** `--vault-host` / `GODWIT_VAULT_HOSTS` moves *which Vaults godwit may authenticate at* out of the admin token — which lives in a Secret and is held by CI — and into the pod spec, which lives in git and is reviewed. It is empty by default, which accepts any host, because a single-operator deployment gains nothing from it; it should be set wherever more than one person holds an admin token. The scheme check refuses anything that is not `http` or `https` with a host, at registration, where the error is legible.

**Plaintext `http` is allowed and will stay allowed.** In-cluster addresses like `http://vault.production.svc` are how these deployments are wired, and the deployment that motivated this change uses exactly that. Refusing them would refuse the case this exists for. The allowlist is the control; transport security is the platform's.

The list is checked when a store is registered, not when one is read. Tightening it does not retroactively refuse a store registered under a wider one — `godwit credential-stores` prints every address, and re-registering is how a store is moved.

### Where a store is not needed to get started

Registering a store is an API call carrying an `admin` bearer token that writes a row to the control-plane database. Neither is a Vault. A service that has never had a store starts, serves, and answers every read; its `vault` targets refuse until one exists. There is no bootstrap problem to solve and no bootstrap path to keep.

## What it costs

**The live deployment breaks on upgrade, deliberately.** Its two registered targets carry no store, so they refuse every run until an operator registers one and re-registers them against it. The alternative was the fallback, and a break someone chose beats a fallback nobody remembers is there. The steps are in #136's description and the refusal names them.

**A store is one more object to register**, and the chart's Job registers `stores.list` ahead of `targets.list` for that reason. For a deployment with one Vault this is a line of configuration that used to be a line of configuration; for one with two, it is the difference between working and not.

**Connection handling stays what it was.** Each resolve logs in and reads, and the client token is not cached — that was true before this change and is true after it, per store rather than per process. One `http.Transport` is shared, which already pools per host, so N stores get N connection pools without N clients. Token caching and renewal do not exist in godwit and are not introduced here; they would apply equally to the old single-Vault path and belong in a change that says so.

## Rejected

| Thing | Verdict | Reason |
|---|---|---|
| Keeping `VAULT_ADDR` as the default for a target that names no store | refused | It is the defect. The default path would stay the broken one, and the failure it produces is a target reading from a Vault nobody chose. |
| Keeping `VAULT_ADDR` "only to bootstrap the first store" | refused | There is nothing to bootstrap: a store is registered with an admin token against the control-plane database. The variable would survive as a second way in that nobody needs and everybody eventually finds. |
| A `provider` / `kind` field on the store | refused | One legal value today. A second credential source needs a new provider and new fields either way, and no target changes in either design — so the field buys nothing now and can be added when it means something. |
| Letting `vault-transit` keep reading `VAULT_ADDR` | refused | It would be the one surviving global Vault setting, sharing a name with what was just removed. Its own variables and a loud start-up failure are cheaper than the confusion. |
| A store carrying a Vault token directly, sealed with the keyring | refused | A second sealed secret in the control plane that `settleKeys` must walk on every key rotation, and a store stranded under a lost key is a fleet-wide outage rather than one target's problem. `--vault-token-env` names a variable the operator already wires; godwit stores the name, never the value. |
| Refusing plaintext `http` | refused | It would refuse the in-cluster addresses this exists for, including the one that motivated it. |
| Enforcing the allowlist at resolve as well as at registration | deferred | Registration is where the error is legible and where the audit entry is written. A store registered under a wider list keeps working; `godwit credential-stores` makes that visible rather than silent. |
| Deleting a store | not built | Nothing deletes a target either. The foreign key refuses to remove a store a target still reads from, which is the property that matters; re-registering the name is how a store moves. |
