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
9. Reverts that run with `RevertRun`: `plan` comes back, `plan_v2` goes, the original run is marked `reverted`.
10. Registers the target again through a Vault KV secret (`vault` provider with a DSN template) and runs a migration with credentials resolved at claim time.
11. Lists every run through the CLI (`godwit runs`) from inside a replica — the same binary, the same API.
12. Scrapes `/metrics` on the surviving replica: runs per state, the reconciler takeover, the refused hazard and the drift check all show up as Prometheus series.

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
