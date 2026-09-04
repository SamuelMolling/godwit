# 0009 — The scratch database is a sandbox, not a corner of the store

Shipped in #83.

## The open question

A security review of `origin/main` opened with this, as its single Critical finding:

> `Diff` and validation execute caller-supplied DDL on a database created with the *store's own credentials on the store's own PostgreSQL server*. `Diff` needs only `read` scope.

The mechanism is not a bug. `Differ.Diff` and `Validator.Validate` need a real PostgreSQL to find out what submitted SQL produces — that is the whole point of the scratch replay, and it is why godwit can tell an author that migration 14 does not apply on top of the history before anything reaches a target. What was never decided is *where* that database lives and *as whom* the SQL runs. It defaulted to the only connection the service had, and the design documents talked about the scratch database as a compatibility caveat (`docs/security.md`: "migrations that reference roles, tablespaces or extensions that exist only on the target fail validation") rather than as an execution surface.

Three things reach it: `Diff` with the caller's pasted DDL, `buildRepeatables` with the `R__` bodies of the request, and `Validator.Validate` with every migration file of a `PlanRun` or `CreateRun`. The first two need `read` scope, the one `docs/security.md` recommends handing to pull-request pipelines.

The open question was therefore: what is the boundary, and what does PostgreSQL actually give us to draw it with?

## The decision

**Scratch databases get their own PostgreSQL and their own role, named by `--scratch-dsn`, and the service refuses to start when that role can act outside them.**

- `--scratch-dsn` / `GODWIT_SCRATCH_DSN` opens a second pool. `Validator` and `Differ` no longer hold the store pool at all; they hold a `controlplane.Scratch` that creates, connects to and drops databases on whatever that pool points at. The store role stops needing `CREATEDB`.
- The role is `LOGIN CREATEDB NOSUPERUSER NOCREATEROLE NOREPLICATION NOBYPASSRLS`. **godwit does not create it** — creating roles needs `CREATEROLE`, which is precisely what the scratch connection must not have, and the store role has no business holding it either. It is three lines of SQL in the docs, run once by whoever provisions the server.
- At start-up `Scratch.Check` reads `pg_roles`, `pg_has_role` and `pg_database` on the scratch connection. **Superuser, ownership of the store database, membership of `pg_execute_server_program` / `pg_read_server_files` / `pg_write_server_files`, `CREATEROLE` and `REPLICATION` each refuse the start**, naming what was found. `BYPASSRLS` and a scratch role that can still `CONNECT` to the store database are warnings.
- Scratch databases are cloned from **`template0`**, not the default `template1`. This is the one escalation the role attributes do not close: `dblink` and `postgres_fdw` cannot be *created* by a non-superuser (neither is a trusted extension), but a database inheriting them from `template1` has them already, and from there a scratch session can open a connection back to the store and read `cp_targets.config`. `--scratch-template <db>` names a prepared template for deployments whose migrations need extensions.

Everything above was verified against PostgreSQL 17 rather than reasoned from the manual, and the verification is a test: `TestScratchRefusesDangerousDDL` submits `DROP DATABASE <store> WITH (FORCE)`, `COPY … FROM PROGRAM`, `pg_read_file`, `CREATE EXTENSION dblink`, `CREATE EXTENSION postgres_fdw` and `ALTER ROLE` through the real `Differ.Diff` path and asserts PostgreSQL's refusal for each.

## Consequences to live with

- **`--scratch-dsn` unset keeps the old behaviour, loudly.** Making it required would break every existing deployment on upgrade, for a service whose store role usually does have `CREATEDB` today. So the fallback stays, and `serve` logs one `scratch database is not isolated` line per finding — for the documented store role that is "is a superuser" or "owns the store database" — plus a line naming `--scratch-dsn` as the fix, on every start. It is documented in `docs/security.md` as a laptop configuration. *Rejected: requiring `--scratch-dsn`* (upgrade breakage for a fix nobody asked for yet). *Rejected: silently keeping the old default* (the review's point is that nobody knew).
- **There is no flag to start with a privileged scratch DSN anyway.** An operator who deliberately points `--scratch-dsn` at a superuser has configured the isolation and defeated it in the same breath, and an override flag would become the thing people copy out of a blog post. The escape hatch is leaving the flag unset, which is warned about rather than silent. The cost is a service that will not boot after someone edits a Secret; the error names the role and the attribute.
- **`template0` is a behaviour change for anyone who installed extensions into `template1` on the store server**, which `docs/security.md` used to tell them to do ("Install the same extensions on the store server"). Their validations will start failing with the extension's own error. `--scratch-template template1` restores the old behaviour exactly; the honest fix is a prepared template holding only what validation needs.
- **`Diff` stays `read`.** Raising it looks like the safe move and is mostly theatre: the same execution path is reached by `PlanRun`, also `read` and also recommended for pull-request pipelines, so raising `Diff` alone moves nothing while breaking every consumer holding a read token — and raising both breaks the plan-on-pull-request workflow the whole CI story is built on. What made `read`-scope DDL execution dangerous was the identity it ran as, and that is what changed. The scope stays, and `docs/security.md` now says *why* the button is safe rather than claiming `Diff` writes nothing. *Rejected: a `diff` scope between `read` and `pipeline`* — decision 0001 already rejected a scope between `read` and `pipeline` for `PlanRun`, and a four-scope model with two exceptions is worse than a three-scope model with an isolated sandbox.
- **The blast radius that remains is one tenant against another.** All scratch databases share one role, so DDL submitted by one caller can drop another's in-flight scratch database and fail their validation. Per-diff roles would need `CREATEROLE` on the scratch connection, which is the attribute we refuse. Nothing bounds how many scratch databases exist, how large they grow or how long a statement runs; that is admission control, tracked separately. The answer for now is that the scratch server holds nothing and is sized for it, which the docs say.
- **`docs/decisions/0004` is now wrong on one line.** It says `Diff` "writes nothing to any database and nothing to disk" as the reason `read` may press the button. Nothing is *persisted*, which is what that sentence meant, but the DDL is executed, and 0004 read as if it were not. The record stands as written — it is history — and this one supersedes that reasoning.

## Refused or deferred

| Thing | Verdict | Reason |
|---|---|---|
| godwit creating the scratch role itself | refused | Needs `CREATEROLE` on a connection whose whole purpose is not having it. Three lines of SQL, once, by whoever owns the server. |
| `ALTER DATABASE … OWNER TO scratch` after the store role creates it | refused | The review's fallback for a single-server deployment. It keeps the store role in the loop for every scratch database and still leaves the scratch role on the store's server; a `CREATEDB` role creating and owning its own databases is simpler and works unchanged whether the DSN points at the same server or another. |
| A session `SET` that stops the DDL reaching outside its database | not available | PostgreSQL has no such GUC. Reach is a property of the role's attributes, its memberships and what the database was cloned from — all three are what this record configures. The session parameters that *are* settable (`statement_timeout`, `idle_in_transaction_session_timeout`) bound cost, not reach, and belong with admission control. |
| A semaphore on concurrent scratch databases, `statement_timeout` on the scratch session, a size cap on submitted schemas | deferred | Same root as this finding, different question: how much a caller may consume, not what they may reach. Being outside the store makes the answer cheaper — the resource at risk is now a disposable server. |
| Sweeping orphaned `godwit_diff_*` / `godwit_validate_*` after a SIGKILL | deferred | The runbook still says to drop them by hand. On a scratch server that holds nothing, `DROP DATABASE` in a loop is a safe thing to automate; on the store server it was not. |
