# 0012 — The key is optional, comes from a provider, and the ciphertext says which

Shipped in #93.

## The open question

Three facts about `GODWIT_MASTER_KEY`, all true before this change:

1. **`serve` required it unconditionally.** A deployment whose targets all use the `vault` or `kubernetes` credential provider — neither of which stores a secret of godwit's own, only a path — could not start without 64 hex characters that encrypted nothing. The chart made it a required Secret key and the documentation said to generate one "even if nothing is encrypted under it yet".
2. **There was one key, in an environment variable.** The store held every `static` DSN under it, so a store dump plus the process environment was every target credential, offline, with nothing to log or revoke. No KMS of any kind.
3. **The ciphertext did not identify its key**, so rotation could not be incremental: a value either opened under the one configured key or did not. The documented procedure was to roll the replicas onto the new key and then re-register every `static` target by hand, with a window in between where those targets were refused.

(3) is what makes (2) expensive to fix and (1) tempting to leave alone. A stored value that cannot say what opens it cannot be migrated, so every improvement to key handling turns into "re-register everything".

## The decision

**The key is optional, it comes from a `KeyProvider`, and every value written carries a header naming the provider and key that opens it.**

```go
type KeyProvider interface {
	Name() string
	KeyID() string
	Seal(ctx context.Context, aad []byte, plaintext string) ([]byte, error)
	Open(ctx context.Context, aad []byte, keyID string, blob []byte) (string, error)
}
```

It is deliberately shaped like the `creds.Provider` the credential side already has: named, selected by a string, resolved from the environment, exercised by the same kind of table test. A `Keyring` wraps one provider and owns the wire format; its **zero value holds no provider**, which is what a deployment with no `static` targets runs with.

Three providers ship: `env` (the default, today's AES-256-GCM under `GODWIT_MASTER_KEY`, plus decrypt-only keys in `GODWIT_MASTER_KEY_PREVIOUS`), `gcpkms` and `vault-transit`. Both KMS providers use **envelope encryption**: a fresh 32-byte data key per value, sealed by the KMS, with the DSN encrypted locally under that data key. The KMS unwraps 32 bytes and never sees a DSN.

The format is `godwit1:<provider>:<base64url key id>:<base64 payload>`, and the header is the AEAD's additional authenticated data, so a rewritten header does not open. A value without the prefix is the old headerless form, read as `env` with an unknown key id: every configured `env` key is tried and GCM decides. Base64 contains no colon, so the two forms cannot be confused.

### Why Cloud KMS and not AWS KMS

**Dependency cost decided it.** The module pulls in no cloud SDK today — the heaviest things in `go.mod` are pgx, `pg_query_go` and testcontainers — and the `vault` credential provider is already 140 lines of `net/http` against a REST API rather than a client library. Cloud KMS keeps that shape: `POST …/cryptoKeys/…:encrypt` with a base64 body, and a bearer token the GCE metadata server hands over in one GET. That is **80 lines and zero new modules**.

AWS KMS has no equivalent. Its API needs SigV4, which is a hundred lines of canonicalisation that is easy to get subtly wrong and unpleasant to test, and production credentials on EKS arrive through `AssumeRoleWithWebIdentity`, which is a second protocol. The honest alternative is `aws-sdk-go-v2` — `config`, `credentials`, `service/kms`, `smithy-go` and their transitive set, roughly twenty modules, for one call. That is a real cost to impose on every build of a migration tool, and it is not paid until someone actually runs on AWS.

The interface is what makes that deferral safe rather than a bet: `AWSKMS` is a fourth `KeyProvider` with the same four methods, a new constant in one switch, and a fake endpoint in its test. Nothing else moves.

**Vault Transit was worth it now**, unlike AWS KMS, because it costs nothing: it reuses the `Vault` struct's HTTP call and its Kubernetes login, so a deployment that already reads target credentials from Vault gains a KMS-grade key provider without gaining a dependency, a cloud account or an IAM binding.

### Rotation: no `reencrypt` command

*Rejected: `godwit targets reencrypt --from-key <old>`*, which the brief for this change proposed. Once the header names the key, the command has nothing to do that the service cannot do on its own, and the automatic path is better on every axis:

**Every replica re-seals, at start-up, each `static` target whose header names anything other than the key in force.** An `env` rotation becomes: put the new key in `GODWIT_MASTER_KEY`, the old one in `GODWIT_MASTER_KEY_PREVIOUS`, roll, drop the old key on the next roll. There is no window where a target is refused, because the old key still opens values throughout. The same pass migrates headerless values from before this change on the first start.

A command would have to be run by a human who remembers to run it, needs an `admin` token, and needs a new RPC to reach the store — for an operation with no decision in it. The automatic pass is idempotent, converges whether it runs once or on every pod, and concurrent replicas write the same bytes under a plain upsert. And with a KMS provider it does nothing at all, because rotating a Cloud KMS or Transit key is invisible here: the ciphertext carries its own key version.

The pass is deliberately quiet about failure — a target it cannot open is logged and skipped, never fatal. See below.

### A missing key fails on use, not at start-up

The brief posed this directly: with `static` targets in the store and no key, refuse at start-up naming them, or fail on use?

**Fail on use, and warn at start-up naming them.** Refusing to start is the wrong shape for a control plane serving many targets: one target whose key went missing would take down runs on every other target, including `vault` and `kubernetes` ones that never needed a key. It is also inconsistent with every other credential provider — an unreachable Vault or an unmounted Secret fails that target's run and leaves the service up.

What start-up refusal buys is loudness, and that is bought more cheaply: `serve` logs `static targets are sealed and no key is configured` with the names, at warn level, next to the existing `no tokens configured`. The operator gets the list immediately without the outage. The refusal on use is precise about what is wrong — it names the key id the value wants — and `CreateRun` refuses before it queues anything, so there is no half-created run.

Registering a `static` target with no key is the one hard refusal (`invalid_argument`), because the alternative is storing something that will never open.

## Consequences to live with

- **Replicas write to `cp_targets` at start-up.** Only rows whose key id differs from the one in force, so normally none; on the first start after this change, every `static` row once. A store that is read-only for the service would log the failure per target and carry on with the values it can already open.
- **The key id is in the store, in the clear.** For `env` it is four bytes of a SHA-256, which names a key without helping anyone find it. For the KMS providers it is the key's resource name, which is not a secret but does tell a reader of the store which KMS key to ask for — and asking still needs IAM.
- **`Open` honours the key the header names, not the one configured.** That is what lets a value survive a move between KMS keys, and it means the service can be made to call any KMS key its identity is granted. Grant the service exactly the keys it should use; an attacker who can write `cp_targets` already owns the target.
- **`env` is still the default and still the weakest.** Nothing forces a deployment off it, and a store dump plus that key is still every `static` DSN. The documentation says so, and says what `gcpkms` and `vault-transit` buy, but the migration is opt-in.
- **The KMS providers are tested against fakes.** The HTTP shapes — Cloud KMS `:encrypt` / `:decrypt` with `additionalAuthenticatedData`, the metadata token endpoint, `transit/datakey/plaintext` and `transit/decrypt` — are exercised against local servers, not against Google or a real Vault. What that leaves unverified is the parts a test cannot reach anyway: IAM bindings, Workload Identity, and how a real endpoint behaves under a permission error.

## What this does not change

The key protects one thing: the DSN of a `static` target. It is not a signing key, it does not authenticate callers, it does not touch `GODWIT_TOKENS`, and it is deliberately not derived from or shared with anything the web UI uses ([0004](0004-ui-is-a-scoped-client.md) and the CSRF note in [security](../security.md#cross-site-requests) both depend on that separation). The best answer to key management here remains the one [security](../security.md#credential-providers) already gives: use `vault` with dynamic credentials and store no secret at all.
