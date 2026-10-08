.PHONY: all check test vet fmt fmt-check build clean

# Default target
all: check build

# Run complete verification suite
check: fmt-check vet test

# Run Go tests with race detection and verbose reporting
test:
	@echo "==> Running Go unit tests with data race detector..."
	@cd client-go && go test -v -race ./...

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

# Build static ARM binary for Kindle Paperwhite (PW5 / Linux ARMv7)
build:
	@echo "==> Cross-compiling static ARM binary for Kindle..."
	@cd client-go && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o ../tracker-arm .
	@echo "==> Build complete: tracker-arm ($$(ls -lh tracker-arm | awk '{print $$5}'))"

# Clean build artifacts
clean:
	@rm -f tracker-arm /tmp/server_dashboard.png
