# 0023 — The token godwit presents is minted for the Vault it goes to

A credential store makes godwit authenticate at an address an admin chose. It presented the pod's
generic ServiceAccount token to do it. This record replaces that token with one Kubernetes mints per
audience, makes the audience a required property of the store, and deletes `--vault-host` /
`GODWIT_VAULT_HOSTS` / `stores.allowedHosts`, the allowlist that stood in for the missing binding.

## The question

[0021](0021-a-target-names-the-vault-its-credentials-live-in.md) gave a store an arbitrary address
and a Kubernetes login, and read the JWT from
`/var/run/secrets/kubernetes.io/serviceaccount/token`. That file is the token the API server issues
for its own audiences. Its holder can present it at any Vault whose Kubernetes auth trusts the
cluster, and every such Vault validates it correctly and grants godwit's policy — because it *is*
godwit's token. Registering a store at a server someone controls therefore harvested a credential
that worked at the real one.

The Vault-side role answers **who may log in**: `bound_service_account_names`,
`bound_service_account_namespaces`. Nothing in it answers **was this token issued for me**. That
question has an answer in Kubernetes — the `aud` claim of a projected token — and godwit was not
asking it.

## The decision

**A store names the audience its Vault requires, and godwit presents only the token minted for it.**

```bash
godwit credential-store add production \
  --vault-addr https://vault.production.internal --vault-k8s-role godwit \
  --vault-audience vault.production.internal
```

```yaml
stores:
  audiences: [vault.production.internal, vault.staging.internal]
  tokenExpirationSeconds: 3600
```

The deployment projects one token per audience at `/var/run/secrets/godwit/vault/<audience>`, and
the store's audience is how godwit finds it. A Vault whose role carries `audience=` rejects every
other token, so a harvested one is inert anywhere but the Vault it names.

### Required, not defaulted

There is no default audience, and `--vault-audience` is refused as empty rather than filled in.

A default of `""` is the hole: it means *mint nothing in particular*, which is a token every Vault
accepts, arrived at silently by an operator who never typed it. A non-empty default — `vault`, say —
is worse in a subtler way: it is a guess that happens to work at every Vault configured with the
same string, so the credential is shared again, and the operator whose Vault expects something else
gets a `403` that names nothing. Required means the failure is at registration, in one sentence, on
the machine of the person who knows the answer.

The audience is also constrained to a plain name — letters, digits, dot, dash, underscore. It is the
file name of the projected token, so an audience of `../../kubernetes.io/serviceaccount/token` would
be a store that reads the generic token again. That is refused at registration and again in
`internal/creds` before the read, because only the second one is on the path a row already in the
database takes.

### More than one audience is more than one source, not more than one volume

A `serviceAccountToken` projection carries exactly one audience. A projected volume carries a list
of sources, and each may be a `serviceAccountToken` with its own audience, `expirationSeconds` and
`path`. So *n* audiences are *n* sources of one volume, one mount, distinct file names:

```yaml
- name: vault-audience-tokens
  projected:
    sources:
      - serviceAccountToken: { audience: vault.production.internal, expirationSeconds: 3600, path: vault.production.internal }
      - serviceAccountToken: { audience: vault.staging.internal,    expirationSeconds: 3600, path: vault.staging.internal }
```

The chart renders that from `stores.audiences` and fails the render on an audience it cannot use as
a file name. Nothing in godwit needs a second knob for the path: the audience is the path.

### Rotation

The kubelet rewrites a projected token well before it expires, and the token it replaced stops
working. `Vault.token` reads the file inside the login, and the login happens on every fetch — the
client token was never cached either — so a rotation is picked up with no restart. This was already
true of the code and is now covered by a test that rewrites the file between two reads and asserts
the second login carried the new bytes, because it is the property that turns a working service into
one that works for an hour.

### Deleting the host allowlist

`--vault-host` was the guardrail on exactly this leak: it bounded the addresses a store could be
registered at, so an admin token could not aim godwit's credentials at a server of their own. It
goes, and the honest accounting is that it does not go because the leak is closed to zero.

What is now structural: **the credential that works everywhere is gone.** No control-plane row can
name the generic token, and a store's token is refused by every Vault but the one that requires its
audience. What a deployment holds is the list in `stores.audiences` — in the reviewed pod spec, not
behind the admin token, which is the property 0021 wanted from the allowlist in the first place, and
now it is the credential itself rather than a string compared against one.

What remains: an admin-token holder can still register a store at an address of their choosing with
an audience the pod projects, receive that token, and replay it at the Vault that audience names.
The allowlist covered that case and the audience does not. Three things argue for taking it out
anyway:

- It was **empty by default**, so the deployments most likely to be attacked this way — the ones
  nobody hardened — had no protection at all. `stores.audiences` has no such default: a Kubernetes
  store with no projected audience simply has no token, and its targets refuse.
- It compared a **URL string**, which is the weakest available name for a Vault. The audience is
  checked by the recipient, on a signature, which is the strongest.
- It is **shaped for Vault**, and a store is meant to become a credential source in general. A cloud
  secret manager has a fixed endpoint and IAM on the provider's side; "allowed hosts" says nothing
  there, while "the identity this workload presents" says the same thing everywhere.

The residual is one deliberate follow-up away — pairing each audience with the address it may be
presented to — and that is not built today because it re-creates the address-shaped guardrail this
record is arguing out of the design, on the strength of a threat that already requires an admin
token. It is written down here so the next person does not have to rediscover that it is open.

### Not in scope

`GODWIT_VAULT_TRANSIT_K8S_JWT`, the key provider's own Vault login, still reads the generic token.
Its address comes from the pod spec, not from a row an admin token can write, so there is no
arbitrary destination to harvest it to. It gets an audience when someone gives it one; it is not
this change.

## What it costs

A breaking change, twice over. Every Kubernetes-auth store must be re-registered with an audience,
and every Vault role must set `audience=`. Neither side is a no-op flip: with the role's `audience`
set and the deployment still projecting nothing, logins fail; and a Vault that validates via the
TokenReview API rejects an audience-bound token when the role requests no audience, so projecting
first is not obviously safe either. The migration is therefore *additive* — project the new token,
create a **second** Vault role carrying the audience and the same policies, re-register the store
against it, and remove the old role once nothing uses it. The database migration drops `k8s_jwt` and
adds `k8s_audience` empty: a store carried over resolves to no audience and refuses, naming the flag
that fixes it, which is the correct behaviour for a row that cannot be upgraded without an operator
deciding what the audience is.

The audience must be usable as a file name, so a Vault configured with a URL-shaped audience needs
its role changed. This is a real constraint on the operator and is the price of having no second
knob for the path.

## What was refused

| | | |
|---|---|---|
| A default audience | refused | Any default is a credential more than one Vault accepts, reached without the operator saying so. |
| Keeping `--vault-k8s-jwt` as a per-store path | refused | It is the way a store points itself back at the generic token. The audience determines the path; nothing else may. |
| Keeping `--vault-host` alongside the audience | refused | Above. It bounds a case the audience leaves open, and pays for it with an address-shaped key in a store abstraction meant to outgrow addresses, defaulting to off. |
| Verifying the `aud` claim in godwit before presenting | refused | With the path derived from the audience there is no file to present but the right one; parsing our own JWT would guard a state the code cannot reach. |
| One projected volume per audience | refused | Sources are the unit that carries an audience. One volume, one mount, *n* sources. |
