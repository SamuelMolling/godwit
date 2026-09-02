#!/usr/bin/env bash
set -euo pipefail

# Env: GH_TOKEN MARKER SUMMARY COMMAND DRY_RUN COMMENT_ON_PUSH EVENT_NAME REPOSITORY SHA PR_NUMBER RUNNER_TEMP
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
    numbers="$(gh api "repos/${REPOSITORY}/commits/${SHA}/pulls" --jq '.[].number' 2>/dev/null || true)"
    if [ -z "${numbers}" ]; then
      echo "::notice::godwit: no pull request found for ${SHA}, nothing to comment on"
      exit 0
    fi
    ;;
  *)
    exit 0
    ;;
esac

body="${RUNNER_TEMP}/godwit-comment.md"
{ printf '%s\n' "${MARKER}"; cat "${SUMMARY}"; } >"${body}"

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
  if ! gh api --method "${method}" "${url}" -F "body=@${body}" >/dev/null; then
    echo "::warning::godwit: could not post the comment on pull request #${number} (does the token have pull-requests: write?)"
  fi
done
