# 🤖 Agent Coding & Engineering Handover Guide

Welcome, Agent! This document is designed specifically for AI coding assistants (Antigravity, Claude, Cursor, Copilot, etc.) interacting with this codebase.

---

## 🎯 Repository Standards & Engineering Architecture

This project implements a dedicated transit dashboard ecosystem composed of:
1. **Kindle ARM Client (`client-go/`)**: Static Go binary cross-compiled for Linux ARMv7 (Kindle Paperwhite 5), interfacing directly with framebuffer, e-ink controller, touch events, and power management (`rtcWakeup`, `wakealarm`, `powerd`).
2. **Backend Server (`server/`)**: Python application serving dynamic e-ink dashboard PNGs, NJ Transit departure predictions, GTFS realtime updates, and OTA updates.
3. **Deployment Stack**: Multi-architecture Docker image, systemd / shell launcher, and zero-config mDNS/UDP discovery.

When working in this repository:
- Treat all safety gates, linters, and verification checks as strict non-negotiable requirements.
- Never lower coverage gates (Go **93%**, Python **92%**), weaken lint rules, or bypass defensive error handling.
- Preserve documentation integrity and update `docs/` alongside any architectural changes.

---

## 🛡️ Core Non-Negotiable Invariants

### 1. Zero External Dependencies for Go Client
- The Kindle client (`client-go/`) uses **100% Go standard library** (plus internal cryptographic signing packages).
- Never introduce third-party Go dependencies to keep the binary small, static, memory-efficient, and secure on embedded hardware.

### 2. Cryptographic OTA & Server Identity Integrity
- All Over-The-Air (OTA) client updates must be cryptographically signed using Ed25519 (`client-go/cmd/otasign`).
- Client binaries verify SHA256 hashes, version increments, and signature validity before staging updates.
- Server identity certificates and manifests must maintain cryptographic integrity.

### 3. Defensive Concurrency, Gestures, & Touch Loop
- **Non-blocking Event Pump**: Input loops and gesture detection must never block on network I/O or long-running mutexes.
- **Race Safety**: Run tests with `-race` (`make test-go`) to guarantee zero data races.
- **Debounced Interaction**: Physical buttons and touch gestures must implement debouncing to prevent event queue flooding.

### 4. Robust Fallbacks & Graceful Degradation
- If live NJ Transit API feeds fail, fallback to GTFS static schedule cache.
- Multi-tier discovery: probe mDNS, then default gateway IP, then `/24` subnet scan.

### 5. Semantic Versioning, CHANGELOG & Automated Release Invariants
- **Manual CHANGELOG Curation Required**: Contributors and AI agents are strictly required to curate all changes for every PR in [`CHANGELOG.md`](CHANGELOG.md) under `## [X.Y.Z] - YYYY-MM-DD` following [Keep a Changelog](https://keepachangelog.com/).
- **Semantic Versioning Bumps**: Every Pull Request modifying application code, features, or bug fixes **must bump the version in [`VERSION`](VERSION)** according to [Semantic Versioning (SemVer 2.0.0)](https://semver.org/):
  - **Patch** (`1.35.x` → `1.35.y`): Backward-compatible bug fixes, security patches, performance tuning, and minor refactors.
  - **Minor** (`1.x.0` → `1.y.0`): New dashboard modes, hardware support, or significant new features.
  - **Major** (`x.0.0` → `y.0.0`): Breaking architectural overhauls or wire protocol changes.
- **Automated Version & CHANGELOG Verification**: `./scripts/check_version_bump.sh` is enforced in CI on every PR. CI fails closed if `VERSION` is not incremented or if a matching `CHANGELOG.md` entry is missing.
- **Automated GitHub Release Lifecycle on Merge**: When a PR is merged into `main`, GitHub Actions automatically:
  1. Runs and requires 100% pass rate across all verification gates (`go-client`, `python-server`, `shell`, `docker`).
  2. Verifies release tag uniqueness.
  3. Automatically creates the GitHub Release tag `vX.Y.Z` titled `vX.Y.Z - Production Release` populated from [`CHANGELOG.md`](CHANGELOG.md).

---

## ⚡ Mandatory Pre-Completion Checklist

Before reporting any work as complete, you **must execute and pass every step** of this verification pipeline:

```bash
# 1. Check code formatting (Go, Python)
make fmt-check

# 2. Run static analysis and linting (go vet, golangci-lint, mypy, ruff, shellcheck)
make lint

# 3. Run all test suites (Go with -race, Python unittests, Shell launcher tests)
make test

# 4. Enforce strict coverage gates (Go >= 93%, Python >= 92%)
make coverage

# 5. Verify Semantic Version bump & CHANGELOG entry
./scripts/check_version_bump.sh
```
