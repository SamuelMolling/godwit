#!/usr/bin/env bash
set -euo pipefail

# What `helm lint` cannot say about a render; every assertion below carries its own failure message.
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

present "${off}" 'envFrom:' 'the default render does not source the Secret'
present "${off}" '^ +- secretRef:$' 'the Secret does not reach the process as a whole'
absent "${off}" 'secretKeyRef' 'a value names an entry of the Secret the operator already named there'
full="$(helm template godwit "${chart}" -f "${chart}/ci/full-values.yaml")"
absent "${full}" 'secretKeyRef' 'switching every optional block on made the chart name Secret entries again'
present "${full}" 'name: godwit-credentials' 'the full render does not source the Secret it was given'

present "${off}" 'audience: godwit$' 'the default render mints no token for the godwit audience'
present "${off}" 'mountPath: /var/run/secrets/godwit/vault$' 'the token is not mounted where godwit reads it'
present "${off}" 'path: token$' 'the token is projected at a name godwit does not read'
if [[ "$(grep -c 'serviceAccountToken:' <<<"${off}")" != 1 ]]; then
  fail 'a deployment is one identity and rendered more than one projected token'
fi

noauto="$(helm template godwit "${chart}" -f "${chart}/ci/platform-gitops-values.yaml")"
present "${noauto}" 'automountServiceAccountToken: false' 'the gitops render still automounts the generic token'
present "${noauto}" 'audience: godwit$' 'turning the automount off took the projected token with it'

app=(-f "${chart}/ci/platform-github-app-values.yaml")
on="$(helm template godwit "${chart}" "${app[@]}")"
present "${on}" '- --github-webhook-addr=:8475' 'the App render does not open the webhook listener'
present "${on}" 'containerPort: 8475' 'the App render has no webhook container port'
present "${on}" 'name: godwit-webhook' 'the App render has no webhook Service'
present "${on}" '- --github-private-key-file=/secrets/github/private-key.pem' 'the App render does not point at the mounted key'
absent "${on}" 'name: GODWIT_GITHUB_PRIVATE_KEY' 'the App private key has a second home: a file and an environment variable'
present "${on}" '^ +- key: github-private-key.pem$' 'the App render does not project the Secret entry holding the PEM'
present "${on}" 'defaultMode: 0440' 'the App private key is not mounted group-readable'

env_key="$(helm template godwit "${chart}" "${app[@]}" --set existingSecret.githubPrivateKey=)"
present "${env_key}" '- --github-webhook-addr=:8475' 'the App stopped opening its listener without a key file'
absent "${env_key}" 'github-private-key-file' 'the App render reads a key file the chart projects no entry into'
absent "${env_key}" 'github-app-key' 'the App render mounts a key volume with no Secret entry to project'

webhook="$(awk '/^# Source: godwit\/templates\/webhook-service.yaml/,/^---/' <<<"${on}")"
present "${webhook}" 'port: 8475' 'the webhook Service does not carry the webhook port'
absent "${webhook}" 'port: 8474' 'the webhook Service also carries the API port'

echo "helm assert: ok"
