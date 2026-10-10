#!/usr/bin/env bash
# ==============================================================================
# Script: deploy.sh
# Purpose: Auto-extracts version, compiles static Kindle ARM client (if Go present),
#          builds version-tagged & latest Docker images, and launches the transit-tracker stack.
# ==============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

COMPOSE_CMD=()
if docker compose version >/dev/null 2>&1; then
    COMPOSE_CMD=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE_CMD=(docker-compose)
elif [[ -x "/opt/homebrew/bin/docker-compose" ]]; then
    COMPOSE_CMD=(/opt/homebrew/bin/docker-compose)
elif [[ -x "/home/linuxbrew/.linuxbrew/bin/docker-compose" ]]; then
    COMPOSE_CMD=(/home/linuxbrew/.linuxbrew/bin/docker-compose)
else
    echo "Error: Neither 'docker compose' nor 'docker-compose' found!" >&2
    exit 1
fi

VERSION=""
if [[ -f "VERSION" ]]; then
    VERSION=$(tr -d '[:space:]' < VERSION)
elif [[ -f "server/version.py" ]]; then
    VERSION=$(python3 -c "import server.version; print(server.version.VERSION)" 2>/dev/null || true)
elif [[ -f "server/server.py" ]]; then
    VERSION=$(python3 -c "import server.server; print(server.server.SERVER_VERSION)" 2>/dev/null || true)
fi

if [[ -z "${VERSION}" ]]; then
    echo "Error: Could not determine version from VERSION or server/version.py!" >&2
    exit 1
fi

IMAGE_TAG="v${VERSION}"
echo "======================================================================"
echo "🚌 Deploying Transit Tracker (${IMAGE_TAG})"
echo "======================================================================"

# The Kindle binary is signed during the image build with the release key,
# passed as a BuildKit secret (see docker-compose.yml). Fail early and clearly
# rather than with an opaque compose "secret file not found" error.
OTA_SIGNING_KEY_FILE="${OTA_SIGNING_KEY_FILE:-./secrets/ota_ed25519.key}"
export OTA_SIGNING_KEY_FILE
if [[ ! -s "${OTA_SIGNING_KEY_FILE}" ]]; then
    if [[ "${ALLOW_UNSIGNED_OTA:-0}" == "1" ]]; then
        echo "⚠️  No signing key at ${OTA_SIGNING_KEY_FILE}; building UNSIGNED (devices will not be offered this build)."
        OTA_SIGNING_KEY_FILE=/dev/null
        export OTA_SIGNING_KEY_FILE ALLOW_UNSIGNED_OTA
    else
        echo "Error: no OTA release signing key at ${OTA_SIGNING_KEY_FILE}." >&2
        echo "  Create one once with:  make keygen" >&2
        echo "  (or: mkdir -p secrets && openssl genpkey -algorithm ed25519 -out secrets/ota_ed25519.key)" >&2
        echo "  Back it up: devices only accept updates signed by this key." >&2
        exit 1
    fi
fi

# Docker multi-stage compiles and signs tracker-arm automatically
export IMAGE_TAG
DOCKER_BUILDKIT=1 "${COMPOSE_CMD[@]}" build transit-tracker

if docker image inspect "transit-tracker:${IMAGE_TAG}" >/dev/null 2>&1; then
    docker tag "transit-tracker:${IMAGE_TAG}" "transit-tracker:latest" || true
fi

"${COMPOSE_CMD[@]}" up -d --force-recreate transit-tracker

echo "⏳ Waiting for Transit Tracker to pass healthcheck..."
CONTAINER_NAME="transit-tracker"
MAX_WAIT_SECS=60
ELAPSED=0
HEALTH_STATUS="unknown"

while (( ELAPSED < MAX_WAIT_SECS )); do
    HEALTH_STATUS=$(docker inspect --format='{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${CONTAINER_NAME}" 2>/dev/null || echo "starting")
    if [[ "${HEALTH_STATUS}" == "healthy" ]]; then
        echo ""
        echo "✅ Transit Tracker is healthy and serving dashboard (took ${ELAPSED}s)."
        break
    fi
    if [[ "${HEALTH_STATUS}" == "exited" || "${HEALTH_STATUS}" == "dead" ]]; then
        echo ""
        echo "❌ Error: Container '${CONTAINER_NAME}' stopped unexpectedly (status: ${HEALTH_STATUS})." >&2
        docker logs --tail 50 "${CONTAINER_NAME}" >&2 || true
        exit 1
    fi
    sleep 2
    (( ELAPSED += 2 ))
    printf "."
done

if [[ "${HEALTH_STATUS}" != "healthy" ]]; then
    FINAL_STATUS=$(docker inspect --format='{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${CONTAINER_NAME}" 2>/dev/null || echo "unknown")
    if [[ "${FINAL_STATUS}" == "healthy" ]]; then
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
