#!/usr/bin/env bash
set -euo pipefail

# What `helm lint` cannot say: that the Secret is the only home of what it carries, that the GitHub
# App is off unless it is asked for, and that asking for it opens one listener on one Service of its own.
chart="${1:-deploy/helm/godwit}"

fail() {
  echo "helm assert: $1" >&2
  exit 1
}

refuses() {
  local why="$1"
  shift
  if helm template godwit "${chart}" "$@" >/dev/null 2>&1; then
    fail "${why}"
  fi
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

# One home: the Secret reaches the process whole, and nothing in the chart names an entry of it.
present "${off}" 'envFrom:' 'the default render does not source the Secret'
present "${off}" '^ +- secretRef:$' 'the Secret does not reach the process as a whole'
absent "${off}" 'secretKeyRef' 'a value names an entry of the Secret the operator already named there'
full="$(helm template godwit "${chart}" -f "${chart}/ci/full-values.yaml")"
absent "${full}" 'secretKeyRef' 'switching every optional block on made the chart name Secret entries again'
present "${full}" 'name: godwit-credentials' 'the full render does not source the Secret it was given'

# The identity godwit presents at Vault is the deployment's, not a value: every render carries it, at
# the file internal/creds names, and it is not the generic ServiceAccount token.
present "${off}" 'audience: godwit$' 'the default render mints no token for the godwit audience'
present "${off}" 'mountPath: /var/run/secrets/godwit/vault$' 'the token is not mounted where godwit reads it'
present "${off}" 'path: token$' 'the token is projected at a name godwit does not read'
if [[ "$(grep -c 'serviceAccountToken:' <<<"${off}")" != 1 ]]; then
  fail 'a deployment is one identity and rendered more than one projected token'
fi

# An explicit serviceAccountToken projection does not depend on the automounted one, so a deployment
# can hold its own token and not the generic one every Vault accepts.
noauto="$(helm template godwit "${chart}" -f "${chart}/ci/platform-gitops-values.yaml")"
present "${noauto}" 'automountServiceAccountToken: false' 'the gitops render still automounts the generic token'
present "${noauto}" 'audience: godwit$' 'turning the automount off took the projected token with it'

on="$(helm template godwit "${chart}" -f "${chart}/ci/platform-github-app-values.yaml")"
present "${on}" '- --github-webhook-addr=:8475' 'the App render does not open the webhook listener'
present "${on}" 'containerPort: 8475' 'the App render has no webhook container port'
present "${on}" 'name: godwit-webhook' 'the App render has no webhook Service'
present "${on}" '- --github-private-key-file=/secrets/github/private-key.pem' 'the App render does not point at the mounted key'
absent "${on}" 'name: GODWIT_GITHUB_PRIVATE_KEY' 'the App private key reached the process as environment, not as a file'
present "${on}" '^ +- key: github-private-key.pem$' 'the App render does not project the Secret entry holding the PEM'
# A secret volume is owned by root:fsGroup and the container is not root, so owner-only is unreadable.
present "${on}" 'defaultMode: 0440' 'the App private key is not mounted group-readable'

# envFrom turns every entry whose name a shell would accept into an environment variable, so the one
# entry that must stay out of the environment is refused when it is named like one.
app=(-f "${chart}/ci/platform-github-app-values.yaml")
refuses 'the App private key may be named after an environment variable, which envFrom would then set' \
  "${app[@]}" --set existingSecret.githubPrivateKey=GODWIT_GITHUB_PRIVATE_KEY
refuses 'the App renders with no Secret entry named for its private key' \
  "${app[@]}" --set existingSecret.githubPrivateKey=

# The API Service and the webhook Service are two objects, so a route attached to one cannot reach
# the other's port. Assert the webhook Service carries exactly one port, and that it is not the API's.
webhook="$(awk '/^# Source: godwit\/templates\/webhook-service.yaml/,/^---/' <<<"${on}")"
present "${webhook}" 'port: 8475' 'the webhook Service does not carry the webhook port'
absent "${webhook}" 'port: 8474' 'the webhook Service also carries the API port'

echo "helm assert: ok"
