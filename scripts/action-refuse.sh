#!/usr/bin/env bash
set -euo pipefail

# Env: MESSAGE GH_TOKEN COMMAND DRY_RUN COMMENT EVENT_NAME EVENT_PATH REPOSITORY RUN_URL RUNNER_TEMP
MARKER="<!-- godwit:refused -->"
export MARKER
COMMENT="${COMMENT:-true}"
DRY_RUN="${DRY_RUN:-false}"

event() { jq -r "$1" "${EVENT_PATH}" 2>/dev/null || true; }

number=""
head=""
case "${EVENT_NAME}" in
  issue_comment)
    if [ -n "$(event '.issue.pull_request // empty')" ]; then
      number="$(event '.issue.number // ""')"
    fi
    ;;
  pull_request|pull_request_target|pull_request_review)
    number="$(event '.pull_request.number // ""')"
    head="$(event '.pull_request.head.sha // ""')"
    ;;
esac
if [ -z "${number}" ] || [ -n "${number//[0-9]/}" ]; then
  echo "::warning::godwit: the refusal is in this log only; a ${EVENT_NAME} event names no pull request to answer on"
  exit 0
fi
if [ "${#head}" -ne 40 ]; then
  head="$(gh api "repos/${REPOSITORY}/pulls/${number}" 2>/dev/null | jq -r '.head.sha // ""' || true)"
fi

body="${RUNNER_TEMP}/godwit-refused.md"
{
  printf '%s\n' "${MARKER}"
  printf '## godwit %s refused\n\n' "${COMMAND}"
  printf '%s\n\n' "${MESSAGE}"
  printf 'Nothing ran. [Workflow run](%s)\n' "${RUN_URL}"
} >"${body}"

comment_url=""
if [ "${COMMENT}" = "true" ]; then
  # One refusal stands at a time, and it is the newest comment: an edited one sits where nobody scrolls.
  ids="$(gh api --paginate "repos/${REPOSITORY}/issues/${number}/comments" \
    --jq '.[] | select(.body | startswith(env.MARKER)) | .id' 2>/dev/null || true)"
  for id in ${ids}; do
    gh api --method DELETE "repos/${REPOSITORY}/issues/comments/${id}" >/dev/null 2>&1 || true
  done
  if ! comment_url="$(gh api --method POST "repos/${REPOSITORY}/issues/${number}/comments" -F "body=@${body}" --jq .html_url)"; then
    comment_url=""
    echo "::warning::godwit: could not post the refusal on pull request #${number} (does the token have pull-requests: write?)"
  fi
fi

kind="${COMMAND}"
if [ "${COMMAND}" = "migrate" ] && [ "${DRY_RUN}" = "true" ]; then kind=plan; fi
case "${kind}" in
  plan) context=godwit/plan ;;
  apply|confirm|revert|migrate) context=godwit/applied ;;
  *) exit 0 ;;
esac
if [ "${#head}" -ne 40 ] || [ -n "${head//[0-9a-f]/}" ]; then
  echo "::warning::godwit: the ${context} status was not set; pull request #${number} carries no readable head commit"
  exit 0
fi
STATE=failure DESCRIPTION="${MESSAGE}" TARGET_URL="${comment_url:-${RUN_URL}}" SHA="${head}" CONTEXT="${context}" \
  "$(dirname "$0")/action-status.sh"
