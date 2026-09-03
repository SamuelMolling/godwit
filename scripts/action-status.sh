#!/usr/bin/env bash
set -euo pipefail

# Env: GH_TOKEN REPOSITORY SHA STATE DESCRIPTION TARGET_URL
if ! gh api --method POST "repos/${REPOSITORY}/statuses/${SHA}" \
  -f state="${STATE}" -f context=godwit/applied -f description="${DESCRIPTION:0:140}" -f target_url="${TARGET_URL}" >/dev/null; then
  echo "::warning::godwit: could not set the godwit/applied status on ${SHA} (does the token have statuses: write?)"
fi
