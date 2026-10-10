.PHONY: all check test test-go test-py vet fmt fmt-check coverage coverage-go coverage-py check-sh build keygen pubkey verify-release clean

SHELL := /bin/bash

COVERAGE_MIN_GO ?= 93
COVERAGE_MIN_PY ?= 92
PYTHON ?= python3

LAUNCHER := client-go/launcher/TransitTracker.sh

all: check build

check: fmt-check vet test-go test-py coverage check-sh

test: test-go test-py

test-go:
	@echo "==> Running Go unit tests with data race detector..."
	@cd client-go && go test -v -race ./...

test-py:
	@echo "==> Running Python unit tests..."
	@PYTHONPATH=server $(PYTHON) -m unittest discover -s tests -p "test_*.py" -v

vet:
	@echo "==> Running go vet static analysis..."
	@cd client-go && go vet ./...

fmt:
	@echo "==> Formatting Go files with gofmt -s..."
	@gofmt -s -w client-go

fmt-check:
	@echo "==> Checking Go formatting..."
	@DIFF=$$(gofmt -s -d client-go); \
	if [ -n "$$DIFF" ]; then \
		echo "$$DIFF"; \
		echo "ERROR: Go files are not formatted. Run 'make fmt' to fix."; \
		exit 1; \
	fi

check-sh:
	@echo "==> Checking shell scripts syntax..."
	@bash -n scripts/check_coverage_go.sh
	@bash -n scripts/deploy.sh
	@sh -n $(LAUNCHER)
	@if command -v shellcheck >/dev/null 2>&1; then \
		echo "==> Running shellcheck..."; \
		shellcheck --severity=warning -s bash scripts/check_coverage_go.sh scripts/deploy.sh; \
		shellcheck --severity=warning -s sh $(LAUNCHER); \
	fi

coverage: coverage-go coverage-py

coverage-go:
	@echo "==> Enforcing Go coverage gate ($(COVERAGE_MIN_GO)%)..."
	@bash scripts/check_coverage_go.sh $(COVERAGE_MIN_GO)

coverage-py:
	@echo "==> Enforcing Python coverage gate ($(COVERAGE_MIN_PY)%)..."
	@$(PYTHON) -m coverage erase
	@PYTHONPATH=server $(PYTHON) -m coverage run --source=server -m unittest discover -s tests -p "test_*.py" >/dev/null
	@$(PYTHON) -m coverage report -m --fail-under=$(COVERAGE_MIN_PY)

# ------------------------------------------------------------------------------
# Signed OTA releases (see README "Security model").
#
# The release key signs every Kindle binary. Its public half is baked into the
# binary at build time, so a device only ever installs updates signed by you.
# Keep the private key out of git (secrets/ is gitignored and dockerignored).
# ------------------------------------------------------------------------------
VERSION := $(strip $(shell tr -d '[:space:]' < VERSION 2>/dev/null || echo "0.0.0"))
OTA_SIGNING_KEY ?= secrets/ota_ed25519.key
# Set ALLOW_UNSIGNED=1 to build without a key. Such a binary has OTA disabled
# and the server will not advertise it to devices.
ALLOW_UNSIGNED ?= 0
OTASIGN := go -C client-go run ./cmd/otasign

keygen:
	@if [ -e "$(OTA_SIGNING_KEY)" ]; then \
		echo "ERROR: $(OTA_SIGNING_KEY) already exists; refusing to overwrite (devices trust its public key)."; \
		exit 1; \
	fi
	@echo "==> Generating Ed25519 release signing key at $(OTA_SIGNING_KEY)..."
	@$(OTASIGN) keygen -out "$(abspath $(OTA_SIGNING_KEY))" >/dev/null
	@echo "==> Public key (embedded into every build):"
	@$(OTASIGN) pubkey -key "$(abspath $(OTA_SIGNING_KEY))"
	@echo "    Back up $(OTA_SIGNING_KEY) somewhere safe: losing it means re-flashing every Kindle over USB."

pubkey:
	@$(OTASIGN) pubkey -key "$(abspath $(OTA_SIGNING_KEY))"

# Build static ARM binary for Kindle Paperwhite (PW5 / Linux ARMv7), then sign
# it (tracker-arm.manifest.json) and mint a server identity certificate if one
# does not exist yet. Artifacts are staged and moved into place last so the
# server never sees a binary paired with a stale manifest for long (and treats
# a mismatched pair as "no release" in the meantime).
build:
	@set -euo pipefail; \
	PUB=""; \
	if [ -s "$(OTA_SIGNING_KEY)" ]; then \
		PUB=$$($(OTASIGN) pubkey -key "$(abspath $(OTA_SIGNING_KEY))"); \
	elif [ "$(ALLOW_UNSIGNED)" = "1" ]; then \
		echo "WARNING: building UNSIGNED (no $(OTA_SIGNING_KEY)); OTA will be disabled in this binary."; \
	else \
		echo "ERROR: no release signing key at $(OTA_SIGNING_KEY)."; \
		echo "       Run 'make keygen' (or: openssl genpkey -algorithm ed25519 -out $(OTA_SIGNING_KEY))"; \
		echo "       or build with ALLOW_UNSIGNED=1 for a dev binary that can never be OTA-updated."; \
		exit 1; \
	fi; \
	echo "==> Cross-compiling static ARM binary for Kindle (v$(VERSION))..."; \
	(cd client-go && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build \
		-ldflags="-s -w -X main.Version=$(VERSION) -X main.OTAPublicKey=$$PUB" -o ../tracker-arm.new .); \
	if [ -n "$$PUB" ]; then \
		echo "==> Signing release manifest..."; \
		$(OTASIGN) sign -key "$(abspath $(OTA_SIGNING_KEY))" -version "$(VERSION)" \
			-in "$(CURDIR)/tracker-arm.new" -out "$(CURDIR)/tracker-arm.manifest.json.new"; \
		if [ ! -s server_identity.key ] || [ ! -s server_identity.cert.json ]; then \
			echo "==> Minting server identity certificate..."; \
			$(OTASIGN) server-cert -key "$(abspath $(OTA_SIGNING_KEY))" \
				-out-key "$(CURDIR)/server_identity.key" -out-cert "$(CURDIR)/server_identity.cert.json"; \
		fi; \
		mv -f tracker-arm.new tracker-arm; \
		mv -f tracker-arm.manifest.json.new tracker-arm.manifest.json; \
	else \
		mv -f tracker-arm.new tracker-arm; \
		rm -f tracker-arm.manifest.json; \
	fi; \
	echo "==> Build complete: tracker-arm ($$(ls -lh tracker-arm | awk '{print $$5}'))"

verify-release:
	@$(OTASIGN) verify -pub "$$($(OTASIGN) pubkey -key "$(abspath $(OTA_SIGNING_KEY))")" \
		-manifest "$(CURDIR)/tracker-arm.manifest.json" -in "$(CURDIR)/tracker-arm" && echo "OK: release signature valid"

clean:
	@rm -f tracker-arm tracker-arm.new tracker-arm.manifest.json tracker-arm.manifest.json.new server/tracker-arm client-go/client-go client-go/cover.out
	@rm -rf htmlcov .coverage
	@find . -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true
	@find . -name "*.pyc" -delete 2>/dev/null || true
