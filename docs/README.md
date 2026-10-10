# Transit Tracker Documentation Index

Welcome to the **Hoboken Transit Tracker** technical documentation. This directory provides in-depth architecture, security, API, hardware, and developer specifications.

---

## Documentation Directory

| Document | Focus & Audience | Key Topics |
|---|---|---|
| **[Architecture & System Design](architecture.md)** | System Engineers & Maintainers | Topology, dual-redundancy arrival engine, canvas rendering, schedule governor, fleet registry, cascading discovery. |
| **[Security Specification & Cryptographic Model](security.md)** | Security Auditors & Developers | Ed25519 root trust chain, signed OTA manifests, authenticated responses, constant-time verification, CSP, zero-bypass token policy. |
| **[HTTP API & Protocol Reference](api.md)** | Client & Backend Developers | Endpoint specifications, query parameters, request/response headers, JSON schemas, control plane commands. |
| **[Hardware & Kindle Paperwhite Guide](hardware_kindle.md)** | Hardware Deployers & End Users | PW5 specifications, jailbreak prerequisites, bootstrap launcher, touch gestures, power key, lipc frontlight commands, e-ink framebuffer. |
| **[Developer Guide & Quality Verification](development.md)** | Contributors & CI/CD Engineers | Prerequisites, Makefile targets, linters (Go/Python/Shell/Docker), test suites, coverage gates, release builds, pre-commit hooks. |

---

## Quick Navigation by Task

- **Deploying a new Kindle device?**  
  See the **[Kindle Installation Guide](hardware_kindle.md#3-installation-via-launcher-script)**.
- **Auditing cryptographic signatures and OTA security?**  
  See the **[Security Trust Chain](security.md#1-cryptographic-trust-chain)**.
- **Calling server control endpoints or inspecting responses?**  
  See the **[HTTP API Reference](api.md)**.
- **Running tests or checking coverage gates?**  
  See the **[Developer Verification Guide](development.md#2-makefile-automation-reference)**.
- **Understanding how bus arrival feeds fall back during outages?**  
  See the **[Dual-Redundancy Arrival Engine](architecture.md#21-dual-redundancy-arrival-engine)**.
