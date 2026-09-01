# godwit demo

Two godwit replicas, a control-plane Postgres and a target Postgres — enough to see crash-safe execution with your own eyes.

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
