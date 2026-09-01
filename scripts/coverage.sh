#!/usr/bin/env bash
set -euo pipefail

# 100% gate; excluded: cmd/godwit/main.go (only wires os.Exit) and gen/ (generated).
profile="${1:-coverage.out}"

go test ./... -race -covermode=atomic -coverpkg=./... -coverprofile="${profile}"

grep -v -e "/cmd/godwit/main.go:" -e "/gen/" "${profile}" > "${profile}.filtered"

total="$(go tool cover -func="${profile}.filtered" | awk '/^total:/ {print $3}')"
echo "total coverage: ${total}"

if [[ "${total}" != "100.0%" ]]; then
  echo "coverage gate failed: expected 100.0%, got ${total}" >&2
  exit 1
fi
