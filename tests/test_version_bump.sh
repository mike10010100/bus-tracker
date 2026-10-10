#!/usr/bin/env bash
# ==============================================================================
# Script: test_version_bump.sh
# Purpose: Unit tests for scripts/check_version_bump.sh SemVer validation
# ==============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/check_version_bump.sh"

TEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/test_ver_bump_XXXXXX")"
trap 'rm -rf "$TEST_TMP"' EXIT

cd "$TEST_TMP"
git init -q
git branch -M main
git config user.name "CI Tester"
git config user.email "tester@example.com"

echo "1.0.0" > VERSION
cat << 'EOF' > CHANGELOG.md
# Changelog
## [1.0.0] - 2026-01-01
- Initial release
EOF

git add VERSION CHANGELOG.md
git commit -qm "Initial commit"

# Test 1: On main branch with matching CHANGELOG
if ! bash "$SCRIPT" >/dev/null 2>&1; then
    echo "FAIL: Expected check_version_bump to pass on base branch with valid changelog" >&2
    exit 1
fi

# Test 2: In PR branch with valid SemVer bump and CHANGELOG entry
git checkout -qb feat/new-feature
echo "1.1.0" > VERSION
cat << 'EOF' >> CHANGELOG.md

## [1.1.0] - 2026-01-02
- Added new feature
EOF

if ! bash "$SCRIPT" main >/dev/null 2>&1; then
    echo "FAIL: Expected check_version_bump to pass with valid version bump and changelog" >&2
    exit 1
fi

# Test 3: Version not bumped (equal to main)
echo "1.0.0" > VERSION
if bash "$SCRIPT" main >/dev/null 2>&1; then
    echo "FAIL: Expected check_version_bump to fail when version is not incremented" >&2
    exit 1
fi

# Test 4: Version bumped but missing CHANGELOG entry
echo "1.2.0" > VERSION
if bash "$SCRIPT" main >/dev/null 2>&1; then
    echo "FAIL: Expected check_version_bump to fail when CHANGELOG is missing entry" >&2
    exit 1
fi

echo "OK: check_version_bump tests passed"
