.PHONY: all check test test-go test-py vet fmt fmt-check coverage coverage-go coverage-py build clean

# Coverage gates (percentage of statements). Enforced by `make coverage`.
COVERAGE_MIN_GO ?= 93
COVERAGE_MIN_PY ?= 92

# Default target
all: check build

# Run complete verification suite (format, vet, tests, coverage gate)
check: fmt-check vet test-go test-py coverage

test: test-go test-py

# Run Go tests with race detection and verbose reporting
test-go:
	@echo "==> Running Go unit tests with data race detector..."
	@cd client-go && go test -v -race ./...

# Run Python unit tests
test-py:
	@echo "==> Running Python unit tests..."
	@PYTHONPATH=server python3 -m unittest discover -s tests -p "test_*.py" -v

# Run static analysis
vet:
	@echo "==> Running go vet static analysis..."
	@cd client-go && go vet ./...

# Format all Go source files according to Go best practices
fmt:
	@echo "==> Formatting Go files with gofmt -s..."
	@gofmt -s -w client-go

# Verify that all Go source files are formatted
fmt-check:
	@echo "==> Checking Go formatting..."
	@DIFF=$$(gofmt -s -d client-go); \
	if [ -n "$$DIFF" ]; then \
		echo "$$DIFF"; \
		echo "ERROR: Go files are not formatted. Run 'make fmt' to fix."; \
		exit 1; \
	fi

# Enforce coverage gates for both stacks
coverage: coverage-go coverage-py

coverage-go:
	@echo "==> Enforcing Go coverage gate ($(COVERAGE_MIN_GO)%)..."
	@bash scripts/check_coverage_go.sh $(COVERAGE_MIN_GO)

coverage-py:
	@echo "==> Enforcing Python coverage gate ($(COVERAGE_MIN_PY)%)..."
	@python3 -m coverage erase
	@PYTHONPATH=server python3 -m coverage run --source=server -m unittest discover -s tests -p "test_*.py" >/dev/null
	@python3 -m coverage report -m

# Build static ARM binary for Kindle Paperwhite (PW5 / Linux ARMv7)
VERSION := $(shell cat VERSION)
LDFLAGS := -s -w -X main.Version=$(VERSION)

build:
	@echo "==> Cross-compiling static ARM binary for Kindle (v$(VERSION))..."
	@cd client-go && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="$(LDFLAGS)" -o ../tracker-arm .
	@echo "==> Build complete: tracker-arm ($$(ls -lh tracker-arm | awk '{print $$5}'))"

# Clean build artifacts
clean:
	@rm -f tracker-arm server/tracker-arm client-go/client-go client-go/cover.out /tmp/server_dashboard*.png
	@rm -rf htmlcov .coverage
