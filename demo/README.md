# godwit demo

Two godwit replicas, a control-plane Postgres, a target Postgres and a dev-mode Vault — enough to see crash-safe execution with your own eyes.

```bash
cd demo
docker compose up -d --build
./demo.sh
```

The script:

1. Registers the target database via the API (`static` provider — the DSN is AES-GCM-encrypted at rest in the store).
2. Runs a normal migration and waits for `SUCCEEDED`.
3. Queues a slow migration (`ALTER TABLE … ; SELECT pg_sleep(15)`), waits until a replica claims it, then **`docker compose kill`s that replica mid-run**.
4. Polls through the surviving replica: after the lease TTL (30s) it recovers the run (`attempts: 2`) and finishes it from the journal in the target database.
5. Shows the proof on the target: the schema, `godwit.migrations`, and the statement-level `godwit.journal`.
6. Exercises the gates: a `DROP TABLE` refused without hazard acknowledgment, SQL that parses but cannot run refused at validation.
7. Detects a manual out-of-band change (drift) and blesses it as the new baseline.
8. Runs an `expand-contract` rollout: the added column lands, the drop waits in `awaiting_contract` until `ConfirmRollout`.
9. Reverts that run with `RevertRun`, scoped to what that run applied and never to the rest of the directory: the dry run prints the plan first, then `plan` comes back, `plan_v2` goes, and the original run is marked `reverted` while staying in the history.
10. Registers the target again through a Vault KV secret (`vault` provider with a DSN template, `lock_timeout: 2s`) and runs a migration with credentials resolved at claim time.
11. Queues a run with a per-run `statement_timeout: 1s` over `SELECT pg_sleep(3)`: it hits PostgreSQL's statement-timeout error, which is transient, so the run goes back to `queued` with a backoff (`transient: ... (retry in Ns)`) instead of failing; `run get` shows the effective timeouts and the retry count, `godwit_statement_failures_total{reason="statement_timeout"}` and `godwit_run_retries_total{code="57014"}` count it. After `--max-attempts` (5) it parks as `needs_attention`.
12. Baselines a database that already has a schema (`legacy`): `BaselineTarget` marks the schema-dump migration applied without running it, the next migration applies on top after validation replays the baseline files, and a second baseline is refused because the target already has applied versions.
13. Asks `GetTargetStatus` for `legacy`: both versions applied with their checksums, nothing pending, the migrate run as last run, the drift baseline it took.
14. Adds a column to `legacy` by hand, then plans a migration that would add it: the stored plan reports it as `alreadyApplied` with its `effect`, and the run records it with zero statements instead of executing it.
15. Runs a repeatable migration (`R__order_totals`): the view is created, sending the same file again reports it `unchanged` and runs nothing, and editing the view body applies it again under the same name — `godwit.repeatables` keeps one row per name with the checksum last applied.
16. Sends `Diff` the whole desired `orders` table plus an index: the response is the up SQL from what `legacy` has now to it (`ADD COLUMN`, `CREATE INDEX CONCURRENTLY`) and the down SQL back. Both calls also propose dropping `order_totals`: `Diff` compares the desired schema against everything the database holds, and a repeatable migration's view is not in the desired schema unless you declare it there too.
17. Registers a `tenant` target with `--search-path tenant,public` and runs a migration creating an unqualified table called `migrations`: it lands in `tenant`, while `godwit.migrations` next to it is still the journal with that migration recorded in it.
18. Lists every run through the CLI (`godwit runs`) from inside a replica — the same binary, the same API; the `KIND` column tells baseline runs from migrations.
19. Scrapes `/metrics` on the surviving replica: runs per state, the reconciler takeover, the refused hazard and the drift check all show up as Prometheus series.

While the stack is up, open <http://localhost:18475/ui> to see the same runs and drift events in the operator UI. It asks for basic auth: the password is a token secret — `demo-token`, the one entry of `GODWIT_TOKENS` — and the username is ignored. You sign in as `ui:demo` with that token's scope, so the pages offer exactly the actions the token may take and the audit log names the same identity the API calls use.

Poke around:

```bash
# API (connect JSON; same endpoints speak gRPC)
curl -s -X POST localhost:18474/godwit.v1.GodwitService/ListRuns \
  -H 'Authorization: Bearer demo-token' -H 'Content-Type: application/json' -d '{}'

# target database
psql postgres://app:app@localhost:15433/app

# tear down
docker compose down -v
```
