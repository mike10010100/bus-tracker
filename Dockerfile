# syntax=docker/dockerfile:1.7
# Multi-architecture Dockerfile for Transit Tracker Server
# Supports linux/amd64 (x86_64 PC/servers) and linux/arm64 (Raspberry Pi 3/4/5, Apple Silicon)

# Stage 1: Compile + sign the static Kindle ARM client (tracker-arm)
#
# The release signing key is provided as a BuildKit secret (never a build arg,
# never copied into a layer). Its public half is embedded in the binary, the
# binary is signed (tracker-arm.manifest.json), and a per-build server identity
# certificate is minted so devices can authenticate this server's responses.
FROM golang:1.26-alpine AS builder
ARG ALLOW_UNSIGNED_OTA=0
WORKDIR /build
COPY client-go/ .
COPY VERSION .
RUN --mount=type=secret,id=ota_signing_key \
    set -eu; \
    VERSION="$(tr -d '[:space:]' < VERSION)"; \
    KEY=/run/secrets/ota_signing_key; \
    mkdir -p /out; \
    PUB=""; \
    if [ -s "$KEY" ]; then \
        PUB="$(go run ./cmd/otasign pubkey -key "$KEY")"; \
    elif [ "$ALLOW_UNSIGNED_OTA" = "1" ]; then \
        echo "WARNING: building UNSIGNED Kindle binary; OTA is disabled and devices will not be offered it."; \
    else \
        echo "ERROR: no OTA signing key secret. Run 'make keygen' on the host (or set ALLOW_UNSIGNED_OTA=1)." >&2; \
        exit 1; \
    fi; \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
        go build -ldflags="-s -w -X main.Version=${VERSION} -X main.OTAPublicKey=${PUB}" -o /out/tracker-arm .; \
    if [ -n "$PUB" ]; then \
        go run ./cmd/otasign sign -key "$KEY" -version "$VERSION" -in /out/tracker-arm -out /out/tracker-arm.manifest.json; \
        go run ./cmd/otasign server-cert -key "$KEY" -out-key /out/server_identity.key -out-cert /out/server_identity.cert.json; \
        go run ./cmd/otasign verify -pub "$PUB" -manifest /out/tracker-arm.manifest.json -in /out/tracker-arm; \
    fi

# Stage 2: Runtime image
FROM python:3.11-slim

# Install system fonts and curl for container health checks
RUN apt-get update && apt-get install -y --no-install-recommends \
    fonts-dejavu-core \
    tzdata \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Set default timezone to US Eastern for accurate schedule countdowns
ENV TZ=America/New_York
RUN ln -snf /usr/share/zoneinfo/$TZ /etc/localtime && echo $TZ > /etc/timezone

# Unprivileged runtime user. The container uses host networking, so the server
# process should not hold root.
RUN groupadd --system tracker && useradd --system --gid tracker --home-dir /app --no-create-home tracker

WORKDIR /app

# Install Python requirements
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy server and dashboard rendering application files.
# VERSION must be present at runtime: server.py reports it to clients.
COPY VERSION .
COPY server/ ./

# Signed Kindle client, its release manifest and (when signed) the server
# identity key + certificate from the builder stage.
COPY --from=builder /out/ ./

# The identity key must be readable only by the runtime user. The cache dir is
# a named volume; volumes created by older (root) images are re-owned at start.
RUN mkdir -p /app/cache \
    && chown -R tracker:tracker /app/cache \
    && if [ -f server_identity.key ]; then chown tracker:tracker server_identity.key && chmod 0400 server_identity.key; fi

# Expose HTTP port and Auto-Discovery UDP port
EXPOSE 8000/tcp
EXPOSE 8001/udp
EXPOSE 5353/udp

ENV PYTHONUNBUFFERED=1
ENV PORT=8000
ENV CACHE_DIR=/app/cache

# Start as root only long enough to fix ownership of a pre-existing cache
# volume, then drop to the unprivileged user for the server itself.
CMD ["sh", "-c", "chown -R tracker:tracker /app/cache 2>/dev/null || true; exec setpriv --reuid=tracker --regid=tracker --init-groups python3 server.py"]
