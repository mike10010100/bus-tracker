# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [1.35.5] - 2026-10-10

### Added

- **Unified Verification Pipeline**: Added automated verification scripts and gates across Go (93% statement coverage gate) and Python (92% branch coverage gate).
- **Consolidated Documentation**: Organized full technical documentation architecture under `docs/` (`architecture.md`, `security.md`, `api.md`, `hardware_kindle.md`, `development.md`).
- **Comprehensive Quality Tooling**: Enforced `gofmt`, `go vet`, `golangci-lint`, `mypy`, `ruff`, `shellcheck`, and `hadolint` across all codebases.
- **Armv7 Signed Release Verification**: Cryptographic Ed25519 signing validation pipeline for OTA manifests and server identity certificates.

### Changed

- **CI Workflow Modernization**: Upgraded GitHub Actions verification suite to Go 1.26 toolchain and hardened container smoke tests.
- **Docker Non-Root Hardening**: Runtime execution under unprivileged `tracker` system user and strict format compatibility in `docker top` inspections.
- **Gesture and Power Management Cleanup**: Cleaned up gesture detector callbacks, uptime parsers, and powerd suspend helpers for zero linter warnings.
