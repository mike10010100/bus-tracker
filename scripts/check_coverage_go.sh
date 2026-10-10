#!/usr/bin/env bash
# ==============================================================================
# Script: check_coverage_go.sh
# Purpose: Run the Go test suite with coverage and fail if total statement
#          coverage falls below the project gate (default 93%).
#
# Usage: scripts/check_coverage_go.sh [threshold]
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
THRESHOLD="${1:-93}"

cd "${ROOT_DIR}/client-go"

PROFILE="$(mktemp "${TMPDIR:-/tmp}/transit-go-cover.XXXXXX")"
TEST_LOG="$(mktemp "${TMPDIR:-/tmp}/transit-go-test.XXXXXX")"
trap 'rm -f "${PROFILE}" "${TEST_LOG}"' EXIT

if ! go test -coverprofile="${PROFILE}" ./... >"${TEST_LOG}" 2>&1; then
    cat "${TEST_LOG}" >&2
    echo "ERROR: Go unit tests failed during coverage run." >&2
    exit 1
fi

TOTAL=$(go tool cover -func="${PROFILE}" | awk '/^total:/ {gsub(/%/,"",$NF); print $NF}')

if [[ -z "${TOTAL}" ]]; then
    echo "ERROR: Could not parse total coverage from coverage profile." >&2
    exit 1
fi

echo "Go total statement coverage: ${TOTAL}% (gate: ${THRESHOLD}%)"

if awk -v total="${TOTAL}" -v thresh="${THRESHOLD}" 'BEGIN { exit !(total < thresh) }'; then
    echo "ERROR: Go coverage ${TOTAL}% is below the ${THRESHOLD}% gate." >&2
    exit 1
fi

echo "OK: Go coverage gate passed."
