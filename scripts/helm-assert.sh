#!/usr/bin/env bash
set -euo pipefail

# What `helm lint` cannot say: that the GitHub App is off unless it is asked for, and that asking for
# it opens one listener on one Service of its own.
chart="${1:-deploy/helm/godwit}"

fail() {
  echo "helm assert: $1" >&2
  exit 1
}

absent() {
  if grep -qE -e "$2" <<<"$1"; then
    fail "$3"
  fi
}

present() {
  if ! grep -qE -e "$2" <<<"$1"; then
    fail "$3"
  fi
}

off="$(helm template godwit "${chart}")"
absent "${off}" '--github-webhook-addr' 'the default render passes --github-webhook-addr; off must mean the listener never opens'
absent "${off}" 'name: webhook' 'the default render carries a webhook port or Service'
absent "${off}" 'GODWIT_GITHUB_' 'the default render carries GitHub App environment'
absent "${off}" 'github-app-key' 'the default render mounts the App private key'

on="$(helm template godwit "${chart}" -f "${chart}/ci/platform-github-app-values.yaml")"
present "${on}" '- --github-webhook-addr=:8475' 'the App render does not open the webhook listener'
present "${on}" 'containerPort: 8475' 'the App render has no webhook container port'
present "${on}" 'name: godwit-webhook' 'the App render has no webhook Service'
present "${on}" '- --github-private-key-file=/secrets/github/private-key.pem' 'the App render does not point at the mounted key'
absent "${on}" 'name: GODWIT_GITHUB_PRIVATE_KEY' 'the App private key reached the process as environment, not as a file'
# A secret volume is owned by root:fsGroup and the container is not root, so owner-only is unreadable.
present "${on}" 'defaultMode: 0440' 'the App private key is not mounted group-readable'

# The API Service and the webhook Service are two objects, so a route attached to one cannot reach
# the other's port. Assert the webhook Service carries exactly one port, and that it is not the API's.
webhook="$(awk '/^# Source: godwit\/templates\/webhook-service.yaml/,/^---/' <<<"${on}")"
present "${webhook}" 'port: 8475' 'the webhook Service does not carry the webhook port'
absent "${webhook}" 'port: 8474' 'the webhook Service also carries the API port'

echo "helm assert: ok"
