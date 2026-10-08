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
elif [[ -f "server.py" ]]; then
    VERSION=$(grep -m1 '^SERVER_VERSION\s*=' server.py | sed -E 's/SERVER_VERSION\s*=\s*"([^"]+)".*/\1/')
fi

if [[ -z "${VERSION}" ]]; then
    echo "Error: Could not parse version from VERSION or server.py!" >&2
    exit 1
fi

IMAGE_TAG="v${VERSION}"
echo "======================================================================"
echo "🚌 Deploying NJ Transit Bus Tracker (${IMAGE_TAG})"
echo "======================================================================"

# Recompile Kindle ARM client only if missing or if Go source files are newer
NEEDS_ARM_BUILD=false
if [[ ! -f "tracker-arm" ]]; then
    NEEDS_ARM_BUILD=true
elif [[ -d "client-go" ]] && find client-go -type f -newer tracker-arm | grep -q .; then
    NEEDS_ARM_BUILD=true
fi

if [[ "${NEEDS_ARM_BUILD}" == "true" ]] && command -v go >/dev/null 2>&1 && [[ -d "client-go" ]]; then
    echo "🔨 Building static Kindle ARM client (tracker-arm)..."
    (cd client-go && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o ../tracker-arm .) || true
fi

# Build versioned image
export IMAGE_TAG
${COMPOSE_CMD} build bus-tracker

# Maintain 'latest' tag pointing to the new versioned build
if docker image inspect "bus-tracker:${IMAGE_TAG}" >/dev/null 2>&1; then
    docker tag "bus-tracker:${IMAGE_TAG}" "bus-tracker:latest" || true
fi

# Launch the stack
${COMPOSE_CMD} up -d --force-recreate bus-tracker

# Wait for bus-tracker to pass healthcheck
echo "⏳ Waiting for Bus Tracker to pass healthcheck..."
CONTAINER_NAME="bus-tracker"
MAX_WAIT_SECS=60
ELAPSED=0
HEALTH_STATUS="unknown"

while [[ ${ELAPSED} -lt ${MAX_WAIT_SECS} ]]; do
    HEALTH_STATUS=$(docker inspect --format='{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${CONTAINER_NAME}" 2>/dev/null || echo "starting")
    if [[ "${HEALTH_STATUS}" == "healthy" ]]; then
        echo ""
        echo "✅ Bus Tracker is healthy and serving dashboard (took ${ELAPSED}s)."
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
        echo "✅ Bus Tracker is healthy and serving dashboard."
    else
        echo ""
        echo "❌ Error: Container '${CONTAINER_NAME}' did not report healthy within ${MAX_WAIT_SECS}s (status: ${HEALTH_STATUS})." >&2
        echo "Check logs: docker logs ${CONTAINER_NAME}" >&2
        exit 1
    fi
fi

echo "----------------------------------------------------------------------"
echo "✅ Deployed bus-tracker:${IMAGE_TAG} (and latest) successfully!"
echo "======================================================================"
