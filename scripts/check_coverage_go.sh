#!/usr/bin/env bash
# ==============================================================================
# Script: check_coverage_go.sh
# Purpose: Run the Go test suite with coverage and fail if total statement
#          coverage falls below the project gate (default 90%).
#
# Usage: scripts/check_coverage_go.sh [threshold]
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
THRESHOLD="${1:-90}"

cd "${ROOT_DIR}/client-go"

PROFILE="$(mktemp -t transit-go-cover.XXXXXX)"
trap 'rm -f "${PROFILE}"' EXIT

go test -coverprofile="${PROFILE}" ./... >/dev/null

TOTAL=$(go tool cover -func="${PROFILE}" | awk '/^total:/ {gsub(/%/,"",$NF); print $NF}')
echo "Go total statement coverage: ${TOTAL}% (gate: ${THRESHOLD}%)"

if awk "BEGIN {exit !(${TOTAL} < ${THRESHOLD})}"; then
    echo "ERROR: Go coverage ${TOTAL}% is below the ${THRESHOLD}% gate." >&2
    exit 1
fi

echo "OK: Go coverage gate passed."
