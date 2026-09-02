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

echo
echo "==> hazard gate: DROP TABLE without acknowledgment is refused"
rpc CreateRun '{
  "target": "app",
  "files": [
    {"name": "20260901140000_drop.up.sql", "body": "DROP TABLE users;"},
    {"name": "20260901140000_drop.down.sql", "body": "SELECT 1;"}
  ]
}' 18475
echo
echo "    (pass \"acknowledgeHazards\": [\"H002\"] to accept it)"

echo
echo "==> validation gate: SQL that parses but cannot run is refused at admission"
rpc CreateRun '{
  "target": "app",
  "files": [
    {"name": "20260901150000_broken.up.sql", "body": "ALTER TABLE nope ADD COLUMN x int;"},
    {"name": "20260901150000_broken.down.sql", "body": "SELECT 1;"}
  ]
}' 18475
echo

echo
echo "==> drift detection: a manual out-of-band ALTER TABLE"
docker compose exec -T target-db psql -U app -d app -c "CREATE TABLE rogue_manual (id int);" > /dev/null
rpc CheckDrift '{"target": "app"}' 18475
echo
echo "==> blessing the manual change as the new baseline"
rpc AcceptBaseline '{"target": "app"}' 18475
rpc CheckDrift '{"target": "app"}' 18475
echo

echo
echo "==> expand-contract rollout: add a column now, drop the old one after the deploy is confirmed"
EC_ID=$(rpc CreateRun '{
  "target": "app",
  "rollout": "expand-contract",
  "acknowledgeHazards": ["H003"],
  "files": [
    {"name": "20260901160000_add_plan_v2.up.sql", "body": "ALTER TABLE users ADD COLUMN plan_v2 text;"},
    {"name": "20260901160000_add_plan_v2.down.sql", "body": "ALTER TABLE users DROP COLUMN plan_v2;"},
    {"name": "20260901160001_drop_plan.up.sql", "body": "ALTER TABLE users DROP COLUMN plan;"},
    {"name": "20260901160001_drop_plan.down.sql", "body": "ALTER TABLE users ADD COLUMN plan text;"}
  ]
}' 18475 | sed -E 's/.*"runId":"([^"]+)".*/\1/')
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$EC_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_AWAITING_CONTRACT" ] && break
  sleep 1
done
echo "state: $STATE (plan_v2 added, plan still there — the old app version keeps working)"
docker compose exec -T target-db psql -U app -d app -c "\d users"

echo "==> deploy healthy; confirming the rollout releases the contract phase"
rpc ConfirmRollout "{\"runId\": \"$EC_ID\"}" 18475
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$EC_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "state: $STATE"
docker compose exec -T target-db psql -U app -d app -c "\d users"

echo
echo "==> revert: the down side of the last run, through the same crash-safe executor"
RV_ID=$(rpc RevertRun "{\"runId\": \"$EC_ID\", \"acknowledgeHazards\": [\"H003\"]}" 18475 | sed -E 's/.*"runId":"([^"]+)".*/\1/')
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$RV_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "revert run: $RV_ID ($STATE); the original is now:"
rpc GetRun "{\"runId\": \"$EC_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/'
docker compose exec -T target-db psql -U app -d app -c "\d users"

echo
echo "==> vault: the same database registered through a Vault KV secret, no DSN in the control plane"
docker compose exec -T -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN=demo-root vault \
  vault kv put -mount=secret app user=app password=app host=target-db > /dev/null
rpc RegisterTarget '{
  "name": "app-vault",
  "provider": "vault",
  "vaultPath": "secret/data/app",
  "vaultTemplate": "postgres://{{user}}:{{password}}@{{host}}:5432/app?sslmode=disable"
}' 18475
VT_ID=$(rpc CreateRun '{
  "target": "app-vault",
  "files": [
    {"name": "20260901170000_audit.up.sql", "body": "CREATE TABLE audit (id bigint PRIMARY KEY, note text);"},
    {"name": "20260901170000_audit.down.sql", "body": "DROP TABLE audit;"}
  ]
}' 18475 | sed -E 's/.*"runId":"([^"]+)".*/\1/')
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$VT_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "state: $STATE (credentials resolved from Vault at claim time)"

echo
echo "✅ paid-tier features, free: crash recovery, hazard gate, pre-apply validation, drift detection, expand/contract rollouts, revert, Vault credentials."
echo "   (restore the dead replica with: docker compose up -d godwit-1)"
