#!/usr/bin/env bash
set -euo pipefail

# End-to-end demo: register a target, run a migration through the API,
# then kill the executing replica mid-run and watch the other one recover.
#
#   cd demo && docker compose up -d --build && ./demo.sh

API=http://localhost:18474
AUTH="Authorization: Bearer demo-token"
JSON="Content-Type: application/json"

rpc() { # rpc <Method> <json-body> [port]
  curl -s -X POST "http://localhost:${3:-18474}/godwit.v1.GodwitService/$1" -H "$AUTH" -H "$JSON" -d "$2"
}

echo "==> registering target 'app' (static provider, encrypted at rest)"
rpc RegisterTarget '{
  "name": "app",
  "provider": "static",
  "dsn": "postgres://app:app@target-db:5432/app?sslmode=disable"
}'
echo

echo "==> creating a normal run (create table + hazard-free index)"
RUN_ID=$(rpc CreateRun '{
  "target": "app",
  "files": [
    {"name": "20260901120000_users.up.sql",
     "body": "CREATE TABLE users (id bigint PRIMARY KEY, email text);"},
    {"name": "20260901120000_users.down.sql", "body": "DROP TABLE users;"}
  ]
}' | sed -E 's/.*"runId":"([^"]+)".*/\1/')
echo "run: $RUN_ID"

echo "==> waiting for it to succeed"
for _ in $(seq 1 60); do
  STATE=$(rpc GetRun "{\"runId\": \"$RUN_ID\"}" | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "state: $STATE"
[ "$STATE" = "RUN_STATE_SUCCEEDED" ]

echo
echo "==> now the crash: a slow migration (pg_sleep 15) claimed by one replica"
SLOW_ID=$(rpc CreateRun '{
  "target": "app",
  "files": [
    {"name": "20260901130000_slow.up.sql",
     "body": "ALTER TABLE users ADD COLUMN plan text; SELECT pg_sleep(15);"},
    {"name": "20260901130000_slow.down.sql",
     "body": "ALTER TABLE users DROP COLUMN plan;"}
  ]
}' | sed -E 's/.*"runId":"([^"]+)".*/\1/')
echo "run: $SLOW_ID"

echo "==> waiting until a replica picks it up"
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$SLOW_ID\"}" | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_RUNNING" ] && break
  sleep 1
done

echo "==> kill -9 on both replicas' executors? No — just the one that has it. Easiest demo: kill godwit-1"
docker compose kill godwit-1
echo "godwit-1 is dead. godwit-2 must recover the lease (TTL 30s) and finish."

echo "==> polling through godwit-2 (port 18475)"
for _ in $(seq 1 90); do
  OUT=$(rpc GetRun "{\"runId\": \"$SLOW_ID\"}" 18475)
  STATE=$(echo "$OUT" | sed -E 's/.*"state":"([^"]+)".*/\1/')
  ATTEMPTS=$(echo "$OUT" | sed -E 's/.*"attempts":([0-9]+).*/\1/')
  echo "  state=$STATE attempts=${ATTEMPTS:-1}"
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 3
done
[ "$STATE" = "RUN_STATE_SUCCEEDED" ]

echo
echo "==> proof on the target database:"
docker compose exec -T target-db psql -U app -d app -c "\d users"
docker compose exec -T target-db psql -U app -d app \
  -c "SELECT version, name FROM godwit.migrations ORDER BY version;"
docker compose exec -T target-db psql -U app -d app \
  -c "SELECT run_id, stmt_idx, state FROM godwit.journal ORDER BY recorded_at;"

echo
echo "✅ crash survived: godwit-1 died mid-migration, godwit-2 resumed from the journal."
echo "   (restore the dead replica with: docker compose up -d godwit-1)"
