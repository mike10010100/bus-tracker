.PHONY: all check test test-go test-py vet fmt fmt-check coverage coverage-go coverage-py check-sh build clean

SHELL := /bin/bash

COVERAGE_MIN_GO ?= 93
COVERAGE_MIN_PY ?= 92

all: check build

check: fmt-check vet test-go test-py coverage check-sh

test: test-go test-py

test-go:
	@echo "==> Running Go unit tests with data race detector..."
	@cd client-go && go test -v -race ./...

test-py:
	@echo "==> Running Python unit tests..."
	@PYTHONPATH=server python3 -m unittest discover -s tests -p "test_*.py" -v

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
	@sh -n scripts/TransitTracker.sh
	@if command -v shellcheck >/dev/null 2>&1; then \
		echo "==> Running shellcheck..."; \
		shellcheck --severity=warning -s bash scripts/check_coverage_go.sh scripts/deploy.sh; \
		shellcheck --severity=warning -s sh scripts/TransitTracker.sh; \
	fi

coverage: coverage-go coverage-py

coverage-go:
	@echo "==> Enforcing Go coverage gate ($(COVERAGE_MIN_GO)%)..."
	@bash scripts/check_coverage_go.sh $(COVERAGE_MIN_GO)

coverage-py:
	@echo "==> Enforcing Python coverage gate ($(COVERAGE_MIN_PY)%)..."
	@python3 -m coverage erase
	@PYTHONPATH=server python3 -m coverage run --source=server -m unittest discover -s tests -p "test_*.py" >/dev/null
	@python3 -m coverage report -m --fail-under=$(COVERAGE_MIN_PY)

# Build static ARM binary for Kindle Paperwhite (PW5 / Linux ARMv7)
VERSION := $(strip $(shell tr -d '[:space:]' < VERSION 2>/dev/null || echo "0.0.0"))
LDFLAGS := -s -w -X main.Version=$(VERSION)

build:
	@echo "==> Cross-compiling static ARM binary for Kindle (v$(VERSION))..."
	@cd client-go && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="$(LDFLAGS)" -o ../tracker-arm .
	@echo "==> Build complete: tracker-arm ($$(ls -lh tracker-arm | awk '{print $$5}'))"

clean:
	@rm -f tracker-arm server/tracker-arm client-go/client-go client-go/cover.out /tmp/server_dashboard*.png
	@rm -rf htmlcov .coverage
	@find . -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true
	@find . -name "*.pyc" -delete 2>/dev/null || true
