# NJ Transit 126 Bus Tracker (E-Ink Dashboard & Kindle Client)

A real-time bus arrival tracker for NJ Transit Route 126 in Hoboken, NJ, tracking NYC-bound buses at:
- **Washington St at 9th St** (Stop `#20512`)
- **Clinton St at 9th St** (Stop `#20494`)

Built for low-power e-ink wall displays and jailbroken Amazon Kindle devices (tested on Kindle Paperwhite 5 / PW5).

---

## Architecture Overview

```mermaid
flowchart TD
    subgraph Cloud["External APIs"]
        NJT["NJ Transit BUSDV2 API"]
        GQL["NJ Transit GraphQL Fallback"]
    end

    subgraph Host["Host Server (Mac / Linux / Raspberry Pi)"]
        Tracker["bus_tracker.py (Dual-Source Poller)"]
        Renderer["render_dashboard.py (8-bit Grayscale Pillow Canvas)"]
        Server["server.py (HTTP Server on Port 8000)"]
        Tracker --> Renderer --> Server
    end

    subgraph Kindle["Kindle Paperwhite (PW5)"]
        Launcher["BusTracker.sh (Bootstrap Launcher)"]
        GoClient["tracker-arm (Native Go Client v1.2.1)"]
        EIPS["eips (Native E-Ink Framebuffer)"]
        Touch["pt_mt Multi-Touch Digitizer (/dev/input/event1)"]
        Power["bd71828-pwrkey Power Key (/dev/input/event0)"]
        
        Launcher -->|OTA Hot-Reload| GoClient
        GoClient -->|Push Framebuffer| EIPS
        Touch -->|Single Tap: Cycle Light\nDouble Tap: Exit| GoClient
        Power -->|Hardware Press: Exit| GoClient
    end

    NJT --> Tracker
    GQL --> Tracker
    Server -->|dashboard.png?kindle=pw5| GoClient
    Server -->|tracker-arm (OTA Updates)| Launcher
```

---

## Features

- **Dual-Redundancy Arrival Engine:** Primary polling against NJ Transit DepartureVision (BUSDV2) with instant automatic fallback to public GraphQL API.
- **Native Kindle Paperwhite 5 Support:** Standalone statically linked Go ARM client running in memory (`/tmp/tracker`).
- **Touch Gestures:**
  - **Single Tap Anywhere:** Cycles frontlight brightness (**Off** $\rightarrow$ **Cozy 8** $\rightarrow$ **Bright 18** $\rightarrow$ **Off**) instantly without flickering the e-ink screen.
  - **Double Tap Anywhere (< 380ms):** Clean exit back to the Kindle Library / Home booklet.
  - **Hardware Power Button:** Clean exit to Kindle Library.
  - **Top-Right Corner Tap:** Instant exit shortcut.
  - **Top-Left Corner Tap:** Immediate arrival refresh shortcut.
- **Astronomical Auto-Dimming:** Automatically adjusts frontlight brightness and warmth based on local astronomical time in Hoboken, NJ (Daytime: Off, Evening: Cozy Amber, Overnight: Dark). Manual tap overrides hold for 45 minutes.
- **Battery Telemetry & Indicator:** Real-time hardware battery percentage and charging state (`⚡`) queried directly via Kindle `lipc` and displayed in the top header and footer status bar.
- **LAN Auto-Discovery (Zero-Config):** Automatically discovers the running server across the local network via mDNS (`_bustracker._tcp.local`) and UDP broadcast (`BUS_TRACKER_DISCOVER` on port 8001), saving the discovered IP to storage.
- **Wireless Over-The-Air (OTA) Hot-Reloading:** The Kindle polls the server and automatically self-updates its running Go binary in RAM via `syscall.Exec` when a new build is available on the server.
- **Local Fallback Mode:** Caches the last valid binary and offline notification if the server is unreachable.

---

## Setup & Usage

### Option A: Docker Compose (Recommended for Home Servers / Raspberry Pi)

Run the server 24/7 as an appliance with automatic restarts on reboot:

```bash
# 1. Clone repository
git clone https://github.com/mike10010100/bus-tracker.git
cd bus-tracker

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

---

## Kindle Paperwhite Setup

1. Copy `BusTracker.sh` to your Kindle's `documents/` directory:
   ```bash
   cp BusTracker.sh /Volumes/Kindle/documents/
   ```
2. In your Kindle Library, tap **"126 Bus Tracker"**.
   - **Auto-Discovery:** The Go client will automatically scan your Wi-Fi network via UDP broadcast and mDNS, locate the running server, and persist its IP address.
   - *(Optional Manual Override)*: You can force a specific server address by creating `/Volumes/Kindle/documents/tracker_server.txt` containing your server URL (e.g. `http://192.168.1.100:8000`).

---

## Building the Go Client

To compile the ARM binary for Kindle:
```bash
cd client-go
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o ../tracker-arm main.go
```
Any running Kindle connected to your server will detect the new build on its next 45-second poll cycle and update itself over Wi-Fi.

---

## Development & Verification Suite

Run the full Go test and verification suite:
```bash
make test    # Run unit tests with race detection
make vet     # Static analysis with go vet
make fmt     # Format check with gofmt
make check   # Run complete verification suite
```

---

## License

MIT License. See [LICENSE](LICENSE) for details.
