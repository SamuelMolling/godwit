#!/usr/bin/env bash
set -euo pipefail

dir="$(cd "$(dirname "$0")/../backstage" && pwd)"
cd "${dir}"

if [[ ! -f node_modules/.package-lock.json || package-lock.json -nt node_modules/.package-lock.json ]]; then
  npm ci --no-audit --no-fund
fi

npm run tsc
npm run lint
npm test
