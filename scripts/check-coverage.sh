#!/usr/bin/env bash
# Checks that the total test coverage meets the project minimum (80%).
# Usage: scripts/check-coverage.sh [threshold]
set -euo pipefail

THRESHOLD="${1:-80}"
COVERPROFILE="${COVERPROFILE:-coverage.txt}"

# Prefer the project-local toolchain when the system go is unavailable.
if ! command -v go >/dev/null 2>&1 && [ -x ".tools/go/bin/go" ]; then
  export PATH="$PWD/.tools/go/bin:$PATH"
fi

go test -coverprofile="$COVERPROFILE" ./...

total=$(go tool cover -func="$COVERPROFILE" | awk '/^total:/ { gsub(/%/, "", $3); print $3 }')

echo "Total coverage: ${total}% (minimum: ${THRESHOLD}%)"
if ! awk -v c="$total" -v t="$THRESHOLD" 'BEGIN { exit (c + 0 >= t + 0) ? 0 : 1 }'; then
  echo "ERROR: total coverage ${total}% is below the ${THRESHOLD}% minimum" >&2
  exit 1
fi
