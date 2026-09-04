# 0011 — A token spec is three fields or one, never two

Shipped in #84.

## The open question

`GODWIT_TOKENS` accepted three forms, and the parser resolved them by counting colons:

```go
parts := strings.SplitN(spec, ":", 3)
t := Token{Name: AnonymousActor, Scope: ScopeAdmin, Secret: parts[len(parts)-1]}
if len(parts) > 1 { t.Name = parts[0] }
if len(parts) == 3 { t.Scope = Scope(parts[1]) }
```

So `deploy:pipeline` parsed as **name `deploy`, secret `pipeline`, scope `admin`**. Nothing rejected it: the scope was never read, the duplicate-secret check does not look at shapes, and the service started and logged nothing. The operator who wrote it was asking for a pipeline token and got an admin token whose secret is a dictionary word, and the only way to notice is to read `scope=admin` in an access-log line for a call that should not have been allowed.

The question is not whether that is a bug. It is whether it can be fixed compatibly, and the answer is no: the two forms are the same string. `deploy:pipeline` is a valid `name:secret` spec *and* a plausible truncated `name:scope:secret`, and no rule tells them apart. A parser that guesses is the bug.

## The decision

**A spec has exactly three colon-separated fields, or exactly one. Two is refused at start-up.**

| Form | Name | Scope |
|---|---|---|
| `name:scope:secret` | `name` | `scope` |
| `secret` | `anonymous` | `admin` |

The refusal names the first field and nothing else — `token #1: "deploy:…" has two fields; that form used to read the second one as the secret and grant admin: want name:scope:secret or a bare secret` — because the second field of a spec written under the old rule *is* a secret, and a start-up error goes to a log.

*Rejected: refusing only a two-field spec whose second field parses as a scope name* (the review's suggestion). It fixes the case that is easy to demonstrate and leaves the shape that produced it. `deploy:ci-token` still silently means admin, and the operator who writes `ops:oncall` learns nothing. If the form cannot be read without guessing, it should not be read.

*Rejected: keeping `name:secret` and defaulting it to `read` instead of `admin`.* It stops the escalation and keeps the ambiguity: `deploy:pipeline` would then be a read token named `deploy`, still not what it says, and a rotation that moves a real secret into the second field would silently downgrade a caller instead of upgrading one. Quietly wrong in the other direction is not better.

*Rejected: a minimum secret length* (also suggested by the review). Worth doing and not this change: it is a second breaking rule, it lands on every fixture and example in the repository, and it belongs in a pull request that can also say what happens to a service that is already running with a short secret.

The bare-secret form stays. It has no second reading — one field is a secret, the actor is `anonymous`, the scope is `admin` — and it is what the demo and the smoke workflow use. A bare secret may not contain a colon; a three-field secret may, since everything past the second colon is the secret.

## Consequences to live with

- **This breaks a running service at start-up, deliberately.** A deployment whose `GODWIT_TOKENS` carries a two-field entry will not start after the upgrade; `serve` names the entry by its first field. The fix is one edit — insert the scope the token should have had — and it is worth doing under a start-up error rather than discovering it in an audit. The migration note is in [configuration](../configuration.md#token-spec) and [security](../security.md#tokens-and-scopes).
- **The failure is loud but not automatic.** Nothing infers the intended scope, because inferring it is the same guess the old parser made. `deploy:pipeline` might have been a pipeline token written wrong or an admin token whose secret is `pipeline`, and only its holder knows which.
- **`name:secret` disappears from the docs, the chart README and `serve --help`.** Every example in the repository already used the three-field form; the two-field form was documented as a convenience and used only by the demo compose file and the end-to-end rig, both updated here.

## What this does not fix

A token is still a static secret compared against a map, still read only at start-up, and still equivalent to the target's own credential at `pipeline` scope. This record is about one parser, not about the token model, which `docs/security.md` states plainly and this change does not revisit.
