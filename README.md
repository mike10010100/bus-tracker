# Hoboken Transit Tracker (E-Ink Dashboard & Kindle Client)

A real-time transit arrival and dock dashboard for Hoboken, NJ, tracking:
- **NJ Transit Route 126** NYC-bound buses at Washington St & 9th St (`#20512`) and Clinton St & 9th St (`#20494`).
- **Citi Bike** live dock & e-bike availability at nearby stations (Clinton & 9th, Washington & 11th, Willow & 12th, Washington & 8th, Clinton & 7th, Grand & 6th).

Built for low-power e-ink wall displays and jailbroken Amazon Kindle devices (tested on Kindle Paperwhite 5 / PW5).

---

## Architecture Overview

```mermaid
flowchart TD
    subgraph Cloud["External APIs"]
        NJT["NJ Transit BUSDV2 API"]
        GQL["NJ Transit GraphQL Fallback"]
        GBFS["Citi Bike GBFS Feed"]
    end

    subgraph Host["Host Server (Mac / Linux / Raspberry Pi)"]
        Tracker["bus_tracker.py & citibike.py"]
        Renderer["render_dashboard.py (8-bit Grayscale Pillow Canvas)"]
        Server["server.py (ThreadingHTTPServer on Port 8000)"]
        Tracker --> Renderer --> Server
    end

    subgraph Kindle["Kindle Paperwhite (PW5)"]
        Launcher["TransitTracker.sh (Bootstrap Launcher)"]
        GoClient["tracker-arm (Native Go Client)"]
        EIPS["eips (Native E-Ink Framebuffer)"]
        Touch["pt_mt Multi-Touch Digitizer (/dev/input/event1)"]
        Power["bd71828-pwrkey Power Key (/dev/input/event0)"]
        
        Launcher -->|OTA Hot-Reload| GoClient
        GoClient -->|Push Framebuffer| EIPS
        Touch -->|Tap: Cycle Light / Switch View\nDouble Tap: Exit| GoClient
        Power -->|Hardware Press: Exit| GoClient
    end

    NJT --> Tracker
    GQL --> Tracker
    GBFS --> Tracker
    Server -->|dashboard.png?kindle=pw5| GoClient
    Server -->|tracker-arm (OTA Updates)| Launcher
```

The application version is defined once in [`VERSION`](VERSION). The Python
server and Citi Bike user agent read it directly; the Go client receives it at
build time via `-ldflags "-X main.Version=..."`.

---

## Features

- **Dual-Redundancy Arrival Engine:** Primary polling against NJ Transit DepartureVision (BUSDV2) with instant automatic fallback to public GraphQL API. Upstream failures are surfaced distinctly from a genuine "no buses" state.
- **Citi Bike Dock Telemetry:** Live tracking of nearby Citi Bike docks with real-time e-bike availability prioritization.
- **Native Kindle Paperwhite 5 Support:** Standalone statically linked Go ARM client running in memory (`/tmp/tracker`).
- **Touch Gestures:**
  - **Bottom button bar:** `BUSES`, `CITI BIKE`, `LIGHT`, `REFRESH`, `EXIT` tactile buttons along the bottom edge.
  - **Bottom-Left Corner Tap:** Cycles views between Citi Bike and NJ Transit Bus departures (outside the button bar).
  - **Single Tap Anywhere:** Cycles frontlight brightness (**Off** $\rightarrow$ **Cozy 8** $\rightarrow$ **Bright 18** $\rightarrow$ **Off**) instantly without flickering the e-ink screen.
  - **Double Tap Anywhere (< 380ms):** Clean exit back to the Kindle Library / Home booklet.
  - **Hardware Power Button:** Clean exit to Kindle Library.
  - **Top-Right Corner Tap:** Instant exit shortcut.
  - **Top-Left Corner Tap:** Immediate arrival refresh shortcut.
- **Scheduled Commute Dimming:** A fixed schedule (not solar calculation) adjusts frontlight brightness and warmth during the peak Hoboken commute windows (Morning 7:30–9:30 AM, Evening 4:30–7:00 PM). Manual tap overrides hold for 45 minutes.
- **Battery Telemetry & Indicator:** Real-time hardware battery percentage and charging state (`⚡`) queried directly via Kindle `lipc` and displayed in the top header and footer status bar.
- **LAN Auto-Discovery (Zero-Config):** Automatically discovers the running server across the local network via UDP broadcast (`TRANSIT_TRACKER_DISCOVER` on port 8001) and a /24 subnet sweep. Only loopback/link-local/private addresses are auto-adopted. `BUS_TRACKER_*` legacy probes are still accepted for older clients.
- **Wireless Over-The-Air (OTA) Hot-Reloading:** The Kindle polls the server and automatically self-updates its running Go binary in RAM via `syscall.Exec` when a new build is available. Downloads are verified against the server's `X-Tracker-SHA256` header before execution.
- **Local Fallback Mode:** Caches the last valid binary and offline notification if the server is unreachable.

---

## Setup & Usage

### Option A: Docker Compose (Recommended for Home Servers / Raspberry Pi)

Run the server 24/7 as an appliance with automatic restarts on reboot:

```bash
# 1. Clone repository
git clone https://github.com/mike10010100/transit-tracker.git
cd transit-tracker

# 2. (Optional) Configure NJ Transit credentials
cp .env.example .env
# Edit .env with your credentials if desired

# 3. Start in background
docker compose up -d
```

`network_mode: host` is enabled in `docker-compose.yml`, which lets the container seamlessly broadcast mDNS service records and respond to Kindle UDP discovery packets without NAT hurdles.

### Option B: Local Python Server

```bash
pip install -r requirements.txt
python server.py
```
- **Web UI (Auto-reloading):** `http://localhost:8000`
- **Kindle Image Endpoint:** `http://<SERVER_IP>:8000/dashboard.png?kindle=pw5`

### Control Endpoints

`/stop` and `/resume` change tracker state. By default they are only accepted
from private/loopback addresses. To expose them across a network, set a shared
secret and pass it as a header or query parameter:

```bash
export TRACKER_CONTROL_TOKEN=my-secret
curl -H "X-Tracker-Token: my-secret" http://<SERVER_IP>:8000/stop
```

---

## Kindle Paperwhite Setup

1. Copy `TransitTracker.sh` to your Kindle's `documents/` directory:
   ```bash
   cp TransitTracker.sh /Volumes/Kindle/documents/
   ```
2. In your Kindle Library, tap **"Transit Tracker"**.
   - **Auto-Discovery:** The Go client will automatically scan your Wi-Fi network via UDP broadcast, locate the running server, and persist its IP address.
   - *(Optional Manual Override)*: You can force a specific server address by creating `/Volumes/Kindle/documents/tracker_server.txt` containing your server URL (e.g. `http://192.168.1.100:8000`).

---

## Building the Go Client

To compile the ARM binary for Kindle:
```bash
make build    # Cross-compiles tracker-arm with the version from VERSION
```
Any running Kindle connected to your server will detect the new build on its next 45-second poll cycle and update itself over Wi-Fi.

---

## Development & Verification Suite

Install the development dependencies (includes `coverage`):
```bash
pip install -r requirements-dev.txt
```

Run the full test and verification suite:
```bash
make test        # Run Go + Python unit tests (Go with race detection)
make vet         # Static analysis with go vet
make fmt         # Format Go sources with gofmt
make coverage    # Enforce the 90% coverage gate for both stacks
make check       # Run complete suite: fmt-check, vet, tests, coverage gate
```

### Test Coverage

Both stacks are gated at **90% statement coverage** and enforced automatically:

| Stack  | Tool                       | Gate | Current |
|--------|----------------------------|------|---------|
| Go     | `go test -coverprofile`    | 90%  | 90.6%   |
| Python | `coverage.py` (`.coveragerc`) | 90% | 96%    |

- **Go gate:** `scripts/check_coverage_go.sh [threshold]` (default 90).
- **Python gate:** `[report] fail_under = 90` in `.coveragerc`. Test files and `__main__` demo blocks are excluded.
- `make check` fails the build if coverage drops below the gate; CI runs the same gates on every push/PR.

Tests target real behavior rather than line-count: upstream parsing and the BUSDV2→GraphQL fallback, the `ok`/`empty`/`error` status model, HTTP handlers and control-route auth, OTA SHA-256 verification, private-network server adoption, discovery (UDP + subnet sweep), the touch gesture state machine, and the poll/refresh loop.

---

## License

MIT License. See [LICENSE](LICENSE) for details.
