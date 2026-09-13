# 0024 — The API publishes only the errors it classified; everything else is redacted

Shipped in #PR.

## The question

`rpcErr` classifies a handful of sentinel errors and sends the rest to a `default:` branch that wrapped
the error in `CodeInternal` and published its text. One filter stood in front of that branch — `safe`,
which caught `pgconn.ParseConfigError` and `pgconn.ConnectError` and replaced them with *"cannot reach the
database for this call"*. Every other type reached the caller verbatim.

That was tolerable while the only caller was a CLI holding a token. It stopped being tolerable when the
GitHub App started fencing the same message into a comment on a pull request, which on a public repository
is the internet. [#146](https://github.com/SamuelMolling/godwit/pull/146) said so at the time: *"`refusedBy`
publishes the service's message verbatim, so any future error carrying a DSN, store address or Vault path
must be redacted at the service, not filtered at the App."*

It came true on the path a webhook plan already takes. `PlanRun` resolves the target's credentials, and a
failure there is an unclassified error:

- `internal/creds/vault.go` wraps as `read vault secret %s: %w` around `status %d: %s` carrying Vault's own
  response body — the secret path and Vault's error text;
- `internal/creds/gcpkms.go` has the same shape, and a Cloud KMS refusal names the full
  `projects/…/locations/…/keyRings/…/cryptoKeys/…` resource.

## The decision

**An allow-list of safe types fails open for every type nobody thought of, so the list is inverted.** The
`CodeInternal` branch is, by construction, *the error godwit did not recognise*. Nothing that is not
recognised is published. `safe` now replaces every unclassified error with `the call failed; the detail is
in the server log`, keeping the original as the `Unwrap` cause so the access log, `errors.Is` and the
handlers that classify all still see it.

Two things still cross into a comment from that branch, and only two:

- **A dial failure** — `ParseConfigError` or `ConnectError` — as `cannot reach the database for this call`,
  unchanged.
- **A `pgconn.PgError`**: the failing PostgreSQL server's own answer, `permission denied for schema orders`,
  and *only* that. godwit's own wrapping around it is dropped, because the wrapping is where a path or an
  address would be. The error is the reason this is not a blanket redaction: it is the one thing in the
  branch the person reading the comment can act on, it is written by a server godwit dialled rather than by
  godwit, and it names database objects the pull request is already about.

**The order of those two is load-bearing.** A refused login arrives as a `PgError` naming the DSN's user,
wrapped in the `ConnectError` for the dial that carried it, so the dial cases have to be decided first or
`password authentication failed for user "orders_migrator"` is published as a server answer.

**An error meant to be read is classified, not exempted.** Making the redactor fail closed took the
operator guidance in `internal/creds` with it — *"this target names no credential store […] register the
Vault its credentials live in with `godwit credential-store add`"* is exactly what its reader needs. The
answer is not a hole in the redactor: those refusals now carry `creds.ErrCredentialConfig` and `rpcErr`
returns them as `failed_precondition`. They name only registration values, which `ListTargets` and
`credential-stores` already return at `read` scope. Anything else that should be readable is made readable
the same way — by being classified, at the site that knows what it is.

## What it costs

- **A caller debugging an unclassified failure needs the server log.** `godwit run resume` against a store
  that is down now says `the call failed; the detail is in the server log`, and the detail is one `kubectl
  logs` away under `detail`. That is the same trade the dial redaction already made.
- **A refusal that deserves to be read and is not yet classified reads as an internal failure** until
  somebody classifies it. That is the failure mode the inversion chooses, and it is the recoverable one.

## Not fixed here

**A run's own error still reaches the comment whole.** `cp_runs.error` is written by
`controlplane.failureDetail` from the raw error and rendered into the pull request report by the App's
reporter, so a run that fails at claim because Vault refused the read publishes that refusal. It is the
same leak on a second path, and it is a different decision — that column is also the record an operator
reads with `godwit run show`, so redacting it at write time and redacting it at read time are not the same
choice. Left open deliberately rather than half-fixed.
