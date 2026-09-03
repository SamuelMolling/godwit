#!/usr/bin/env bash
set -euo pipefail

# Env: GH_TOKEN SUMMARY COMMAND DRY_RUN COMMENT COMMENT_ON_PUSH EVENT_NAME REPOSITORY SHA PR_NUMBER HEAD_SHA STATUS STALE PHASE RUN_ID RUN_URL SKIPPED RUNNER_TEMP
if [ "${SKIPPED:-false}" = "true" ]; then
  exit 0
fi
case "${COMMENT}:${COMMAND}" in
  true:*|*:apply|*:confirm|*:revert) ;;
  *) exit 0 ;;
esac

case "${COMMAND}" in
  migrate)
    if [ "${DRY_RUN}" = "true" ]; then marker="dry-run"; else marker="migrate"; fi
    ;;
  apply|confirm|revert) marker="migrate" ;;
  *) marker="${COMMAND}" ;;
esac
MARKER="<!-- godwit:${marker} -->"
export MARKER

merged_pulls() {
  numbers="$(gh api "repos/${REPOSITORY}/commits/${SHA}/pulls" --jq '.[].number' 2>/dev/null || true)"
  if [ -z "${numbers}" ]; then
    echo "::notice::godwit: no pull request found for ${SHA}, nothing to comment on"
    exit 0
  fi
}

case "${COMMAND}" in
  apply|confirm|revert)
    numbers="${PR_NUMBER}"
    ;;
  verify)
    if [ "${EVENT_NAME}" != "push" ] || [ "${COMMENT_ON_PUSH}" != "true" ] || [ "${STATUS}" = "0" ]; then
      exit 0
    fi
    merged_pulls
    ;;
  *)
    real_migrate=false
    if [ "${COMMAND}" = "migrate" ] && [ "${DRY_RUN}" != "true" ]; then
      real_migrate=true
    fi
    case "${EVENT_NAME}" in
      pull_request|pull_request_target)
        if [ "${real_migrate}" = "true" ]; then
          exit 0
        fi
        numbers="${PR_NUMBER}"
        ;;
      push)
        if [ "${real_migrate}" != "true" ] || [ "${COMMENT_ON_PUSH}" != "true" ]; then
          exit 0
        fi
        merged_pulls
        ;;
      *)
        exit 0
        ;;
    esac
    ;;
esac

body="${RUNNER_TEMP}/godwit-comment.md"
{ printf '%s\n' "${MARKER}"; cat "${SUMMARY}"; } >"${body}"

comment_url=""
if [ "${COMMENT}" != "true" ]; then
  numbers=""
fi
for number in ${numbers}; do
  ids="$(gh api --paginate "repos/${REPOSITORY}/issues/${number}/comments" \
    --jq '.[] | select(.body | startswith(env.MARKER)) | .id' 2>/dev/null || true)"
  id="$(printf '%s\n' "${ids}" | awk 'NF { print; exit }')"
  if [ -n "${id}" ]; then
    url="repos/${REPOSITORY}/issues/comments/${id}"
    method=PATCH
  else
    url="repos/${REPOSITORY}/issues/${number}/comments"
    method=POST
  fi
  if ! comment_url="$(gh api --method "${method}" "${url}" -F "body=@${body}" --jq .html_url)"; then
    comment_url=""
    echo "::warning::godwit: could not post the comment on pull request #${number} (does the token have pull-requests: write?)"
  fi
done

case "${COMMAND}" in
  apply|confirm|revert) ;;
  *) exit 0 ;;
esac

short="${RUN_ID:0:8}"
case "${COMMAND}:${STATUS}:${STALE}" in
  apply:0:*)
    if [ "${PHASE:-}" = "awaiting-contract" ]; then
      state=pending
      description="expand applied; comment /godwit confirm to run the contract phase"
    else
      state=success
      description="applied by run ${short}; merge when the review is done"
    fi
    ;;
  apply:*:true) state=failure; description="plan stale or missing: re-plan on the pull request, then /godwit apply again" ;;
  apply:*) state=failure; description="apply failed${RUN_ID:+ (run ${short})}; see the pull request comment" ;;
  confirm:0:*) state=success; description="contract applied by run ${short}; merge when the review is done" ;;
  confirm:*)
    if [ -z "${RUN_ID}" ]; then
      exit 0
    fi
    state=failure
    description="contract phase failed (run ${short}); see the pull request comment"
    ;;
  revert:0:*)
    if [ -z "${RUN_ID}" ]; then
      exit 0
    fi
    state=failure
    description="reverted by run ${short}; comment /godwit apply to apply again"
    ;;
  revert:*) state=failure; description="revert failed${RUN_ID:+ (run ${short})}; see the pull request comment" ;;
esac
STATE="${state}" DESCRIPTION="${description}" TARGET_URL="${comment_url:-${RUN_URL}}" SHA="${HEAD_SHA}" "$(dirname "$0")/action-status.sh"
