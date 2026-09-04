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

field() { # field <name>, response on stdin; aborts instead of feeding an error body to the next call
  local body v
  body=$(cat)
  v=$(printf '%s' "$body" | sed -nE "s/.*\"$1\":\"([^\"]+)\".*/\1/p")
  [ -n "$v" ] || { printf 'no %s in response: %s\n' "$1" "$body" >&2; exit 1; }
  printf '%s' "$v"
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
}' | field runId)
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
}' | field runId)
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

echo "==> what godwit-2 logged while taking over (structured, no SQL, no DSN)"
docker compose logs --no-log-prefix godwit-2 | grep -E 'msg="run (claimed|finished)"|msg="statement applied"' | tail -n 5 || true

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
echo "==> safe-DDL recipe: every hazard comes with the ready-to-copy alternative, real names included"
rpc PlanRun '{
  "target": "app",
  "acknowledgeHazards": ["H004"],
  "skipValidation": true,
  "files": [
    {"name": "20260901140001_users_id_bigint.up.sql", "body": "ALTER TABLE users ALTER COLUMN id TYPE bigint;"},
    {"name": "20260901140001_users_id_bigint.down.sql", "body": "SELECT 1;"}
  ]
}' 18475 | sed -E 's/.*"recipe":"([^"]*)".*/\1/; s/\\n/\n    /g; s/^/    /'
echo

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
}' 18475 | field runId)
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
echo "==> revert: only what that run applied, planned before anything runs"
echo "dry run (nothing is queued):"
rpc RevertRun "{\"runId\": \"$EC_ID\", \"acknowledgeHazards\": [\"H003\"], \"dryRun\": true}" 18475
RV_ID=$(rpc RevertRun "{\"runId\": \"$EC_ID\", \"acknowledgeHazards\": [\"H003\"]}" 18475 | field runId)
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$RV_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "revert run: $RV_ID ($STATE); the original is now:"
rpc GetRun "{\"runId\": \"$EC_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/'
docker compose exec -T target-db psql -U app -d app -c "\d users"

echo
echo "==> plan as contract: the pull request stores a plan with what the target looked like"
PLAN_FILES='[
  {"name": "20260901170000_users_email_idx.up.sql", "body": "CREATE INDEX CONCURRENTLY users_email_idx ON users (email);"},
  {"name": "20260901170000_users_email_idx.down.sql", "body": "DROP INDEX CONCURRENTLY users_email_idx;"}
]'
PLAN_ID=$(rpc PlanRun "{\"target\": \"app\", \"persist\": true, \"source\": \"github.com/acme/app@9c1e2f\", \"files\": $PLAN_FILES}" 18475 | field planId)
echo "plan: $PLAN_ID"

echo "==> someone changes the schema by hand between the review and the deploy"
docker compose exec -T target-db psql -U app -d app -c "ALTER TABLE users ADD COLUMN nickname text;"
echo "==> migrate refuses: the plan is stale, and says exactly what moved"
rpc CreateRun "{\"target\": \"app\", \"files\": $PLAN_FILES}" 18475
echo

echo "==> the change is blessed as the new baseline; the same migrate now re-plans and binds"
rpc AcceptBaseline '{"target": "app"}' 18475
PC_ID=$(rpc CreateRun "{\"target\": \"app\", \"files\": $PLAN_FILES}" 18475 | field runId)
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$PC_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "run: $PC_ID ($STATE), bound to plan:"
rpc GetRun "{\"runId\": \"$PC_ID\"}" 18475 | sed -E 's/.*"planId":"([^"]+)".*/\1/'

echo
echo "==> vault: the same database registered through a Vault KV secret, no DSN in the control plane"
docker compose exec -T -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN=demo-root vault \
  vault kv put -mount=secret app user=app password=app host=target-db > /dev/null
rpc RegisterTarget '{
  "name": "app-vault",
  "provider": "vault",
  "vaultPath": "secret/data/app",
  "vaultTemplate": "postgres://{{user}}:{{password}}@{{host}}:5432/app?sslmode=disable",
  "lockTimeout": "2s"
}' 18475
VT_ID=$(rpc CreateRun '{
  "target": "app-vault",
  "files": [
    {"name": "20260901175000_audit.up.sql", "body": "CREATE TABLE audit (id bigint PRIMARY KEY, note text);"},
    {"name": "20260901175000_audit.down.sql", "body": "DROP TABLE audit;"}
  ]
}' 18475 | field runId)
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$VT_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "state: $STATE (credentials resolved from Vault at claim time)"

echo
echo "==> timeouts: the target caps lock waits at 2s; this run also caps every statement at 1s, so pg_sleep(3) times out, and a timeout is transient: the run retries with backoff instead of failing"
TO_ID=$(rpc CreateRun '{
  "target": "app-vault",
  "statementTimeout": "1s",
  "files": [
    {"name": "20260901180000_slow.up.sql", "body": "SELECT pg_sleep(3);"},
    {"name": "20260901180000_slow.down.sql", "body": "SELECT 1;"}
  ]
}' 18475 | field runId)
for _ in $(seq 1 30); do
  rpc GetRun "{\"runId\": \"$TO_ID\"}" 18475 | grep -q '"retries":' && break
  sleep 1
done
docker compose exec -T godwit-2 /godwit run get "$TO_ID" --server http://localhost:8474 --token demo-token
curl -s localhost:18475/metrics | grep -E '^godwit_(statement_failures|run_retries)_total'

echo
echo "==> baseline: adopting a database that already has a schema, without replaying it"
docker compose exec -T target-db psql -U app -d app -c "CREATE DATABASE legacy;" > /dev/null
docker compose exec -T target-db psql -U app -d legacy \
  -c "CREATE TABLE orders (id bigint PRIMARY KEY, total numeric);" > /dev/null
rpc RegisterTarget '{
  "name": "legacy",
  "provider": "static",
  "dsn": "postgres://app:app@target-db:5432/legacy?sslmode=disable"
}' 18475
BASELINE_FILES='[
    {"name": "00000000000001_baseline.up.sql", "body": "CREATE TABLE orders (id bigint PRIMARY KEY, total numeric);"},
    {"name": "00000000000001_baseline.down.sql", "body": "DROP TABLE orders;"},
    {"name": "20260901190000_orders_status.up.sql", "body": "ALTER TABLE orders ADD COLUMN status text;"},
    {"name": "20260901190000_orders_status.down.sql", "body": "ALTER TABLE orders DROP COLUMN status;"}
  ]'
BL_ID=$(rpc BaselineTarget "{\"target\": \"legacy\", \"version\": 1, \"files\": $BASELINE_FILES}" 18475 | field runId)
docker compose exec -T godwit-2 /godwit run get "$BL_ID" --server http://localhost:8474 --token demo-token
docker compose exec -T target-db psql -U app -d legacy \
  -c "SELECT version, name FROM godwit.migrations ORDER BY version;"
echo "==> version 1 is marked applied without running; the next migration applies on top (validation replays the baseline files first)"
BL2_ID=$(rpc CreateRun "{\"target\": \"legacy\", \"files\": $BASELINE_FILES}" 18475 | field runId)
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$BL2_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "state: $STATE"
docker compose exec -T target-db psql -U app -d legacy -c "\d orders"
echo "==> a second baseline is refused: the target already has applied versions"
rpc BaselineTarget "{\"target\": \"legacy\", \"version\": 1, \"files\": $BASELINE_FILES}" 18475
echo
echo "==> target status: applied versions from the database, pending against the files, last run and drift baseline"
rpc GetTargetStatus "{\"target\": \"legacy\", \"files\": $BASELINE_FILES}" 18475
echo

echo
echo "==> already applied by hand: someone adds the column a pending migration would add"
docker compose exec -T target-db psql -U app -d legacy -c "ALTER TABLE orders ADD COLUMN note text;"
NOTE_FILES='[
    {"name": "00000000000001_baseline.up.sql", "body": "CREATE TABLE orders (id bigint PRIMARY KEY, total numeric);"},
    {"name": "00000000000001_baseline.down.sql", "body": "DROP TABLE orders;"},
    {"name": "20260901190000_orders_status.up.sql", "body": "ALTER TABLE orders ADD COLUMN status text;"},
    {"name": "20260901190000_orders_status.down.sql", "body": "ALTER TABLE orders DROP COLUMN status;"},
    {"name": "20260901190001_orders_note.up.sql", "body": "ALTER TABLE orders ADD COLUMN note text;"},
    {"name": "20260901190001_orders_note.down.sql", "body": "ALTER TABLE orders DROP COLUMN note;"}
  ]'
echo "==> the plan sees the effect is already there (alreadyApplied, effect) instead of reporting drift"
NOTE_PLAN=$(rpc PlanRun "{\"target\": \"legacy\", \"persist\": true, \"files\": $NOTE_FILES}" 18475)
echo "$NOTE_PLAN" | sed -E 's/.*"alreadyApplied":true,"effect":"([^"]+)".*/alreadyApplied: \1/'
echo "==> migrate binds to the plan and records the migration without running a statement"
NOTE_ID=$(rpc CreateRun "{\"target\": \"legacy\", \"files\": $NOTE_FILES}" 18475 | field runId)
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$NOTE_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "state: $STATE"
docker compose exec -T target-db psql -U app -d legacy \
  -c "SELECT r.version, m.name, r.stmt_count, r.state FROM godwit.runs r JOIN godwit.migrations m USING (version) ORDER BY r.version;"

echo
echo "==> repeatable migration: R__ has no version and re-applies whenever its body changes"
rep_files() {
  cat <<JSON
[
    {"name": "R__order_totals.up.sql", "body": "CREATE OR REPLACE VIEW order_totals AS SELECT id, total$1 FROM orders;"},
    {"name": "R__order_totals.down.sql", "body": "DROP VIEW IF EXISTS order_totals;"}
  ]
JSON
}
run_and_wait() {
  local id
  id=$(rpc CreateRun "{\"target\": \"legacy\", \"files\": $1}" 18475 | field runId)
  for _ in $(seq 1 30); do
    STATE=$(rpc GetRun "{\"runId\": \"$id\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
    [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
    sleep 1
  done
  echo "state: $STATE"
}
run_and_wait "$(rep_files '')"
echo "==> same file again: the checksum matches, so the plan reports it unchanged and nothing runs"
rpc PlanRun "{\"target\": \"legacy\", \"files\": $(rep_files '')}" 18475 | sed -E 's/.*("repeatable":true[^}]*).*/\1/'
echo "==> edit the view body and it applies again, under the same name"
run_and_wait "$(rep_files ', total * 2 AS doubled')"
docker compose exec -T target-db psql -U app -d legacy \
  -c "SELECT name, left(checksum, 12) AS checksum FROM godwit.repeatables;" \
  -c "SELECT pg_get_viewdef('order_totals'::regclass, true);"

echo
echo "==> schema diff: describe the whole database you want, godwit writes the migration from what legacy has now to it"
echo "==> first without the migration directory: legacy has a repeatable recorded, and a diff that cannot see what declares order_totals refuses instead of proposing to drop it"
rpc Diff '{
  "target": "legacy",
  "schema": "CREATE TABLE orders (id bigint PRIMARY KEY, total numeric, status text, note text, customer_id bigint); CREATE INDEX orders_customer_idx ON orders (customer_id);"
}' 18475
echo
echo "==> now with the R__ files: the view is built on the desired schema too, so upSql only adds the column and creates the index CONCURRENTLY, downSql drops both, and repeatableObjects names what the diff left alone"
rpc Diff "{
  \"target\": \"legacy\",
  \"schema\": \"CREATE TABLE orders (id bigint PRIMARY KEY, total numeric, status text, note text, customer_id bigint); CREATE INDEX orders_customer_idx ON orders (customer_id);\",
  \"files\": $(rep_files ', total * 2 AS doubled')
}" 18475
echo
echo "==> and the schema legacy already has reports no changes, order_totals included"
rpc Diff "{
  \"target\": \"legacy\",
  \"schema\": \"CREATE TABLE orders (id bigint PRIMARY KEY, total numeric, status text, note text);\",
  \"files\": $(rep_files ', total * 2 AS doubled')
}" 18475
echo

echo
echo "==> directive: one comment line asks godwit to run the lock-safe type change itself"
echo "==> the column it retypes arrives as a migration, so validation replays it; only the 5000 rows are loaded by hand"
run_and_wait '[
    {"name": "20260901190002_orders_quantity.up.sql", "body": "ALTER TABLE orders ADD COLUMN quantity integer NOT NULL DEFAULT 1;"},
    {"name": "20260901190002_orders_quantity.down.sql", "body": "ALTER TABLE orders DROP COLUMN quantity;"}
  ]'
docker compose exec -T target-db psql -U app -d legacy \
  -c "INSERT INTO orders (id, total, quantity) SELECT g, g, g FROM generate_series(1, 5000) g ON CONFLICT DO NOTHING;" > /dev/null
CT_FILES='[
    {"name": "20260901190003_quantity.up.sql", "body": "-- godwit: change-type public.orders.quantity bigint batch=1000\n"},
    {"name": "20260901190003_quantity.down.sql", "body": "-- godwit: revert\n"}
  ]'
echo "==> plan: the expansion is computed against the target's catalog and frozen into the plan"
rpc PlanRun "{\"target\": \"legacy\", \"rollout\": \"expand-contract\", \"persist\": true, \"files\": $CT_FILES}" 18475 \
  | tr ',' '\n' | grep -E '"(sql|phase|key|size)"' | head -20
CT_ID=$(rpc CreateRun "{\"target\": \"legacy\", \"rollout\": \"expand-contract\", \"files\": $CT_FILES}" 18475 | field runId)
for _ in $(seq 1 60); do
  STATE=$(rpc GetRun "{\"runId\": \"$CT_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_AWAITING_CONTRACT" ] && break
  sleep 1
done
echo "state: $STATE"
echo "==> the expand phase is applied: both columns exist and a trigger keeps them in sync"
docker compose exec -T target-db psql -U app -d legacy \
  -c "SELECT column_name, data_type FROM information_schema.columns WHERE table_name = 'orders' AND column_name LIKE 'quantity%' ORDER BY column_name;" \
  -c "INSERT INTO orders (id, total, quantity) VALUES (999001, 1, 42);" \
  -c "SELECT quantity, quantity_new FROM orders WHERE id = 999001;"
echo "==> confirm resumes the same run at the statement it stopped at and swaps the columns"
rpc ConfirmRollout "{\"runId\": \"$CT_ID\"}" 18475
for _ in $(seq 1 60); do
  STATE=$(rpc GetRun "{\"runId\": \"$CT_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "state: $STATE"
docker compose exec -T target-db psql -U app -d legacy \
  -c "SELECT column_name, data_type FROM information_schema.columns WHERE table_name = 'orders' AND column_name LIKE 'quantity%' ORDER BY column_name;"
echo "==> quantity is bigint, quantity_old is the rollback godwit kept and recorded as retired"

echo
echo "==> the simple directives: an index built concurrently and a NOT NULL taken without a table scan"
SD_FILES='[
    {"name": "20260901191000_simple.up.sql", "body": "-- godwit: add-index public.orders (status)\n-- godwit: add-not-null public.orders.total\n"},
    {"name": "20260901191000_simple.down.sql", "body": "-- godwit: revert\n"}
  ]'
rpc PlanRun "{\"target\": \"legacy\", \"persist\": true, \"files\": $SD_FILES}" 18475 \
  | tr ',' '\n' | grep -E '"sql"' | head -10
SD_ID=$(rpc CreateRun "{\"target\": \"legacy\", \"files\": $SD_FILES}" 18475 | field runId)
for _ in $(seq 1 60); do
  STATE=$(rpc GetRun "{\"runId\": \"$SD_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "state: $STATE"

echo
echo "==> drop-column is the one that lands in the contract phase: the rollback column goes only after a human confirms"
DC_FILES='[
    {"name": "20260901192000_drop_old.up.sql", "body": "-- godwit: drop-column public.orders.quantity_old\n"},
    {"name": "20260901192000_drop_old.down.sql", "body": "ALTER TABLE orders ADD COLUMN quantity_old integer;\n"}
  ]'
DC_ID=$(rpc CreateRun "{\"target\": \"legacy\", \"rollout\": \"expand-contract\", \"files\": $DC_FILES}" 18475 | field runId)
for _ in $(seq 1 60); do
  STATE=$(rpc GetRun "{\"runId\": \"$DC_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_AWAITING_CONTRACT" ] && break
  sleep 1
done
echo "state: $STATE  (quantity_old is still there)"
rpc ConfirmRollout "{\"runId\": \"$DC_ID\"}" 18475
for _ in $(seq 1 60); do
  STATE=$(rpc GetRun "{\"runId\": \"$DC_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "state: $STATE"
docker compose exec -T target-db psql -U app -d legacy \
  -c "SELECT column_name, is_nullable FROM information_schema.columns WHERE table_name = 'orders' ORDER BY column_name;" \
  -c "SELECT indexname FROM pg_indexes WHERE tablename = 'orders' ORDER BY indexname;"

echo
echo "==> per-target search_path: unqualified names land in the application's schema, the journal never moves"
docker compose exec -T target-db psql -U app -d app -c "CREATE DATABASE tenant;" > /dev/null
docker compose exec -T target-db psql -U app -d tenant -c "CREATE SCHEMA tenant;" > /dev/null
rpc RegisterTarget '{
  "name": "tenant",
  "provider": "static",
  "dsn": "postgres://app:app@target-db:5432/tenant?sslmode=disable",
  "searchPath": "tenant,public"
}' 18475
echo "==> a migration that creates a table called \"migrations\", unqualified, on a target whose path is tenant,public"
SP_ID=$(rpc CreateRun '{
  "target": "tenant",
  "files": [
    {"name": "20260901200000_shadow.up.sql", "body": "CREATE TABLE migrations (id bigint PRIMARY KEY, note text);"},
    {"name": "20260901200000_shadow.down.sql", "body": "DROP TABLE migrations;"}
  ]
}' 18475 | field runId)
for _ in $(seq 1 30); do
  STATE=$(rpc GetRun "{\"runId\": \"$SP_ID\"}" 18475 | sed -E 's/.*"state":"([^"]+)".*/\1/')
  [ "$STATE" = "RUN_STATE_SUCCEEDED" ] && break
  sleep 1
done
echo "state: $STATE"
echo "==> it is in tenant; godwit.migrations is still the journal, with the migration recorded in it"
docker compose exec -T target-db psql -U app -d tenant \
  -c "SELECT table_schema, table_name FROM information_schema.tables WHERE table_name = 'migrations' ORDER BY table_schema;"
docker compose exec -T target-db psql -U app -d tenant -c "SELECT version, name FROM godwit.migrations;"
docker compose exec -T godwit-2 /godwit target status tenant --server http://localhost:8474 --token demo-token

echo
echo "==> every registered target, from the control plane alone: settings, applied count, ready plans, drift, last run"
docker compose exec -T godwit-2 /godwit targets --server http://localhost:8474 --token demo-token

echo
echo "==> the same API from the CLI: every run so far, with who created it"
docker compose exec -T godwit-2 /godwit runs --server http://localhost:8474 --token demo-token

echo
echo "==> the audit log: every mutation so far, by the token name (demo) that made it"
docker compose exec -T godwit-2 /godwit audit --server http://localhost:8474 --token demo-token --limit 10

echo
echo "==> what Prometheus would see on replica 2"
curl -s localhost:18475/metrics | grep -E '^godwit_(runs|run_resumes_total|hazards_total|drift_checks_total)'

echo
echo "✅ paid-tier features, free: crash recovery, hazard gate, pre-apply validation, drift detection, expand/contract rollouts, -- godwit: directives expanded into lock-safe plans, revert, Vault credentials, lock and statement timeouts, per-target search_path, baselining, target status, migrations generated from a desired schema, named tokens and an audit log, Prometheus metrics."
echo "   (restore the dead replica with: docker compose up -d godwit-1)"
