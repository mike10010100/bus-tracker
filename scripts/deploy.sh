#!/usr/bin/env bash
# ==============================================================================
# Script: deploy.sh
# Purpose: Auto-extracts version, compiles static Kindle ARM client (if Go present),
#          builds version-tagged & latest Docker images, and launches the bus-tracker stack.
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

# Locate docker compose command
COMPOSE_CMD=""
if docker compose version >/dev/null 2>&1; then
    COMPOSE_CMD="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE_CMD="docker-compose"
elif [[ -x "/home/linuxbrew/.linuxbrew/bin/docker-compose" ]]; then
    COMPOSE_CMD="/home/linuxbrew/.linuxbrew/bin/docker-compose"
else
    echo "Error: Neither 'docker compose' nor 'docker-compose' found!" >&2
    exit 1
fi

# Extract SemVer version from VERSION file or server.py
VERSION=""
if [[ -f "VERSION" ]]; then
    VERSION=$(cat VERSION | tr -d ' \n\r')
elif [[ -f "server/server.py" ]]; then
    VERSION=$(grep -m1 '^SERVER_VERSION\s*=' server/server.py | sed -E 's/SERVER_VERSION\s*=\s*"([^"]+)".*/\1/')
elif [[ -f "server.py" ]]; then
    VERSION=$(grep -m1 '^SERVER_VERSION\s*=' server.py | sed -E 's/SERVER_VERSION\s*=\s*"([^"]+)".*/\1/')
fi

if [[ -z "${VERSION}" ]]; then
    echo "Error: Could not parse version from VERSION or server/server.py!" >&2
    exit 1
fi

IMAGE_TAG="v${VERSION}"
echo "======================================================================"
echo "🚌 Deploying Transit Tracker (${IMAGE_TAG})"
echo "======================================================================"

# Build versioned image (Docker multi-stage compiles tracker-arm automatically)
export IMAGE_TAG
${COMPOSE_CMD} build transit-tracker

# Maintain 'latest' tag pointing to the new versioned build
if docker image inspect "transit-tracker:${IMAGE_TAG}" >/dev/null 2>&1; then
    docker tag "transit-tracker:${IMAGE_TAG}" "transit-tracker:latest" || true
fi

# Launch the stack
${COMPOSE_CMD} up -d --force-recreate transit-tracker

# Wait for transit-tracker to pass healthcheck
echo "⏳ Waiting for Transit Tracker to pass healthcheck..."
CONTAINER_NAME="transit-tracker"
MAX_WAIT_SECS=60
ELAPSED=0
HEALTH_STATUS="unknown"

while [[ ${ELAPSED} -lt ${MAX_WAIT_SECS} ]]; do
    HEALTH_STATUS=$(docker inspect --format='{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${CONTAINER_NAME}" 2>/dev/null || echo "starting")
    if [[ "${HEALTH_STATUS}" == "healthy" ]]; then
        echo ""
        echo "✅ Transit Tracker is healthy and serving dashboard (took ${ELAPSED}s)."
        break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
    echo -n "."
done

if [[ "${HEALTH_STATUS}" != "healthy" ]]; then
    FINAL_STATUS=$(docker inspect --format='{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${CONTAINER_NAME}" 2>/dev/null || echo "unknown")
    if [[ "${FINAL_STATUS}" == "healthy" ]]; then
        HEALTH_STATUS="healthy"
        echo ""
        echo "✅ Transit Tracker is healthy and serving dashboard."
    else
        echo ""
        echo "❌ Error: Container '${CONTAINER_NAME}' did not report healthy within ${MAX_WAIT_SECS}s (status: ${HEALTH_STATUS})." >&2
        echo "Check logs: docker logs ${CONTAINER_NAME}" >&2
        exit 1
    fi
fi

echo "----------------------------------------------------------------------"
echo "✅ Deployed transit-tracker:${IMAGE_TAG} (and latest) successfully!"
echo "======================================================================"
