#!/usr/bin/env bash
# shellcheck disable=SC2016
set -euo pipefail

# Env: GODWIT_BIN COMMAND DIR BASE ACK SERVER TARGET ROLLOUT DRY_RUN SOURCE SCHEMA PRISMA PRISMA_BIN NAME GODWIT_TOKEN HEAD_SHA PR_NUMBER
#      GH_TOKEN REPOSITORY GITHUB_SERVER_URL GITHUB_REPOSITORY GITHUB_SHA RUNNER_TEMP GITHUB_OUTPUT GITHUB_STEP_SUMMARY
godwit="${GODWIT_BIN}"
work="$(mktemp -d "${RUNNER_TEMP}/godwit-${COMMAND}.XXXXXX")"
summary="${work}/summary.md"
errors="${work}/errors.txt"
status=0
blocking=""
run_id=""
plan_id=""
plan_key=""
stale=false
pending=""
changed=""
files=""

ensure_base() {
  if git rev-parse --verify --quiet "${BASE}^{commit}" >/dev/null; then
    return 0
  fi
  case "${BASE}" in
    origin/*) git fetch --no-tags --depth=1 origin "+refs/heads/${BASE#origin/}:refs/remotes/${BASE}" ;;
  esac
}

remote_args() {
  if [ -n "${ACK}" ]; then args+=(--ack "${ACK}"); fi
  if [ -n "${SERVER}" ]; then args+=(--server "${SERVER}"); fi
  if [ -n "${TARGET}" ]; then args+=(--target "${TARGET}"); fi
  if [ -n "${ROLLOUT}" ]; then args+=(--rollout "${ROLLOUT}"); fi
  if [ -z "${SOURCE}" ]; then
    SOURCE="${GITHUB_SERVER_URL#https://}/${GITHUB_REPOSITORY}@${HEAD_SHA:-${GITHUB_SHA}}"
    if [ -n "${DIR}" ]; then SOURCE="${SOURCE}:${DIR}"; fi
  fi
  args+=(--source "${SOURCE}")
}

refused() {
  {
    printf '## godwit %s\n\n%s:\n\n```\n' "$1" "$2"
    sed 's/^godwit: //' "${errors}"
    printf '```\n'
  } >"${summary}"
}

run_summary() {
  printf '%s' "$1" | jq -r --arg at "$2" '.run | "run `\(.id)`: **\(.state | ascii_downcase | ltrimstr("run_state_"))**"
    + (if .planId != "" and .planId != null then " (plan `\(.planId)`)" else " (implicit plan)" end) + $at
    + (if .error != "" and .error != null then "\n\n```\n\(.error)\n```" else "" end)'
}

migrate() {
  local label="$1" hint="$2" at="$3"
  args=(${dir_args[@]+"${dir_args[@]}"})
  remote_args
  events="${work}/events.jsonl"
  "${godwit}" migrate ${args[@]+"${args[@]}"} --json 2>"${errors}" | tee "${events}" || status=$?
  cat "${errors}" >&2
  last="$(tail -n 1 "${events}" 2>/dev/null || true)"
  run_id="$(printf '%s' "${last}" | jq -r '.run.id // empty' 2>/dev/null || true)"
  plan_id="$(printf '%s' "${last}" | jq -r '.run.planId // empty' 2>/dev/null || true)"
  if [ "${status}" = "3" ]; then
    stale=true
    refused "${label}" "refused: the stored plan is stale or missing, ${hint}"
  elif [ -n "${last}" ]; then
    { printf '## godwit %s\n\n' "${label}"; run_summary "${last}" "${at}"; } >"${summary}"
  elif [ -s "${errors}" ]; then
    refused "${label}" "no run created"
  else
    printf '## godwit %s\n\nno run created\n' "${label}" >"${summary}"
  fi
}

verify() {
  args=(${dir_args[@]+"${dir_args[@]}"})
  remote_args
  "${godwit}" migrate ${args[@]+"${args[@]}"} --dry-run --json >"${work}/plan.json" 2>"${errors}" || status=$?
  cat "${errors}" >&2
  if [ "${status}" != "0" ]; then
    refused verify "refused: the migration set does not pass admission"
    status=1

    return 0
  fi
  local target total
  target="$(jq -r '.target' "${work}/plan.json")"
  total="$(jq -r '.migrations | length' "${work}/plan.json")"
  pending="$(jq -r '[.migrations[] | select((.applied // false) | not)] | length' "${work}/plan.json")"
  if [ "${pending}" = "0" ]; then
    printf '## godwit verify\n\ntarget `%s`: %s migrations, all applied\n' "${target}" "${total}" >"${summary}"
  else
    {
      printf '## godwit verify\n\ntarget `%s`: **%s of %s migrations are not applied**; apply on the pull request before merging (`/godwit apply`):\n\n' "${target}" "${pending}" "${total}"
      jq -r '.migrations[] | select((.applied // false) | not) | "- `\(.version)_\(.name)`"' "${work}/plan.json"
    } >"${summary}"
    status=1
  fi
}

revert() {
  local prefix shas ids runs id last
  prefix="${GITHUB_SERVER_URL#https://}/${GITHUB_REPOSITORY}@"
  if ! shas="$(gh api --paginate "repos/${REPOSITORY}/pulls/${PR_NUMBER}/commits" --jq '.[].sha' | jq -R . | jq -sc .)"; then
    printf '## godwit revert\n\ncould not list the commits of pull request #%s\n' "${PR_NUMBER}" >"${summary}"
    status=1

    return 0
  fi
  args=()
  if [ -n "${SERVER}" ]; then args+=(--server "${SERVER}"); fi
  if [ -n "${TARGET}" ]; then args+=(--target "${TARGET}"); fi
  runs="${work}/runs.json"
  "${godwit}" runs ${args[@]+"${args[@]}"} --json >"${runs}" 2>"${errors}" || status=$?
  cat "${errors}" >&2
  if [ "${status}" != "0" ]; then
    refused revert "could not list the runs"

    return 0
  fi
  ids="$(jq -r --arg prefix "${prefix}" --argjson shas "${shas}" '.runs[]?
    | select((.reverts // "") == "")
    | select(.state == "RUN_STATE_SUCCEEDED" or .state == "RUN_STATE_AWAITING_CONTRACT" or .state == "RUN_STATE_FAILED" or .state == "RUN_STATE_NEEDS_ATTENTION")
    | select(.source as $src | $shas | any(. as $sha | $src == ($prefix + $sha) or ($src | startswith($prefix + $sha + ":"))))
    | .id' "${runs}")"
  printf '## godwit revert\n\n' >"${summary}"
  if [ -z "${ids}" ]; then
    printf 'no run of pull request #%s to revert\n' "${PR_NUMBER}" >>"${summary}"

    return 0
  fi
  args=()
  if [ -n "${ACK}" ]; then args+=(--ack "${ACK}"); fi
  if [ -n "${SERVER}" ]; then args+=(--server "${SERVER}"); fi
  events="${work}/events.jsonl"
  for id in ${ids}; do
    : >"${events}"
    "${godwit}" revert "${id}" ${args[@]+"${args[@]}"} --json 2>"${errors}" | tee "${events}" || status=$?
    cat "${errors}" >&2
    last="$(tail -n 1 "${events}" 2>/dev/null || true)"
    if [ -z "${last}" ]; then
      { printf 'run `%s`: revert refused\n\n```\n' "${id}"; sed 's/^godwit: //' "${errors}"; printf '```\n'; } >>"${summary}"
      status=1

      return 0
    fi
    run_id="$(printf '%s' "${last}" | jq -r '.run.id // empty')"
    printf '%s\n' "$(run_summary "${last}" "" | sed "s/^run \`${run_id}\`:/run \`${run_id}\` reverts \`${id}\`:/")" >>"${summary}"
    if [ "${status}" != "0" ]; then
      return 0
    fi
  done
}

diff() {
  args=(${dir_args[@]+"${dir_args[@]}"})
  if [ -n "${SERVER}" ]; then args+=(--server "${SERVER}"); fi
  if [ -n "${TARGET}" ]; then args+=(--target "${TARGET}"); fi
  if [ -n "${SCHEMA}" ]; then args+=(--schema "${SCHEMA}"); fi
  if [ -n "${PRISMA}" ]; then args+=(--prisma "${PRISMA}"); fi
  if [ -n "${PRISMA_BIN}" ]; then args+=(--prisma-bin "${PRISMA_BIN}"); fi
  if [ -n "${NAME}" ]; then args+=(--name "${NAME}"); fi
  if [ "${DRY_RUN}" = "true" ]; then args+=(--dry-run); fi
  "${godwit}" diff ${args[@]+"${args[@]}"} --json >"${work}/diff.json" 2>"${errors}" || status=$?
  cat "${errors}" >&2
  if [ "${status}" != "0" ]; then
    refused diff refused
    status=1

    return 0
  fi
  changed="$(jq -r '.changed' "${work}/diff.json")"
  files="$(jq -r '.files | join(" ")' "${work}/diff.json")"
  jq -r --arg schema "${PRISMA:-${SCHEMA}}" '
    "## godwit diff\n",
    (if .changed | not then "target `\(.target)`: no changes, it already matches `\($schema)`"
     else
       (if (.drift // "") != "" then "drift (the target'"'"'s live schema, not its history, is the starting point):\n\n```\n\(.drift)\n```\n" else empty end),
       "target `\(.target)` -> `\($schema)`: \(.statements | length) statement(s)\n",
       (.statements | to_entries[] | "- [\(.key)] `\(.value.mode)` \(.value.sql | split("\n")[0])",
         (.value.hazards[] | "  - hazard `\(.code)`: \(.detail)")),
       "\n<details><summary>up</summary>\n\n```sql\n\(.up_sql)\n```\n\n</details>\n<details><summary>down</summary>\n\n```sql\n\(.down_sql)\n```\n\n</details>",
       (if (.files | length) > 0 then "\nwrote " + (.files | map("`\(.)`") | join(", ")) else empty end)
     end)' "${work}/diff.json" >"${summary}"
}

dir_args=()
if [ -n "${DIR}" ]; then dir_args+=(--dir "${DIR}"); fi

case "${COMMAND}" in
  lint)
    args=(${dir_args[@]+"${dir_args[@]}"})
    if [ -n "${ACK}" ]; then args+=(--ack "${ACK}"); fi
    if [ -n "${BASE}" ]; then
      ensure_base
      args+=(--base "${BASE}")
    fi
    "${godwit}" lint ${args[@]+"${args[@]}"} --format markdown >"${summary}" || status=$?
    "${godwit}" lint ${args[@]+"${args[@]}"} --format json >"${work}/lint.json" 2>/dev/null || true
    blocking="$(jq -r '.blocking // 0' "${work}/lint.json" 2>/dev/null || true)"
    blocking="${blocking:-0}"
    ;;
  plan)
    args=(${dir_args[@]+"${dir_args[@]}"})
    remote_args
    "${godwit}" plan ${args[@]+"${args[@]}"} --format markdown >"${summary}" 2>"${errors}" || status=$?
    cat "${errors}" >&2
    if [ ! -s "${summary}" ]; then
      refused plan refused
    fi
    plan_id="$(sed -n 's/^## godwit plan \(.*\)$/\1/p' "${summary}" | head -n 1)"
    plan_key="$(sed -n 's/^key: \(.*\)$/\1/p' "${summary}" | head -n 1)"
    ;;
  migrate)
    if [ "${DRY_RUN}" = "true" ]; then
      args=(${dir_args[@]+"${dir_args[@]}"})
      remote_args
      "${godwit}" migrate ${args[@]+"${args[@]}"} --dry-run --format markdown >"${summary}" 2>"${errors}" || status=$?
      cat "${errors}" >&2
      if [ ! -s "${summary}" ]; then
        refused "dry run" refused
      fi
    else
      migrate migrate "push to the pull request to re-plan" ""
    fi
    ;;
  apply)
    migrate apply "re-run the pull request workflow (re-plan) or push, then comment \`/godwit apply\` again" \
      " at [\`${HEAD_SHA:0:7}\`](${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/commit/${HEAD_SHA})"
    ;;
  verify)
    verify
    ;;
  revert)
    revert
    ;;
  diff)
    diff
    ;;
  *)
    echo "unknown command '${COMMAND}' (want lint, plan, migrate, apply, verify, revert or diff)" >&2
    exit 2
    ;;
esac

cat "${summary}"
cat "${summary}" >>"${GITHUB_STEP_SUMMARY}"
{
  printf 'status=%s\n' "${status}"
  printf 'blocking=%s\n' "${blocking}"
  printf 'run-id=%s\n' "${run_id}"
  printf 'plan-id=%s\n' "${plan_id}"
  printf 'plan-key=%s\n' "${plan_key}"
  printf 'stale=%s\n' "${stale}"
  printf 'pending=%s\n' "${pending}"
  printf 'changed=%s\n' "${changed}"
  printf 'files=%s\n' "${files}"
  printf 'summary-path=%s\n' "${summary}"
} >>"${GITHUB_OUTPUT}"
