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
- **Wireless Over-The-Air (OTA) Hot-Reloading:** The Kindle polls the server and automatically self-updates its running Go binary in RAM via `syscall.Exec` when a new build is available on the server.
- **Local Fallback Mode:** Caches the last valid binary and offline notification if the server is unreachable.

---

## Setup & Usage

### 1. Host Server Requirements
- Python 3.9+
- Go 1.20+ (optional, only needed if rebuilding the Kindle ARM client)

Install Python dependencies:
```bash
pip install -r requirements.txt
```

### 2. NJ Transit API Configuration (Optional)
NJ Transit DepartureVision credentials can be configured via environment variables or a `.env` file:
```bash
cp .env.example .env
```
Edit `.env`:
```ini
NJT_USERNAME=your_username
NJT_PASSWORD=your_password
```
*(Note: If credentials are not provided, the tracker automatically falls back to NJ Transit's public arrival API without authentication).*

### 3. Running the Server
```bash
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
2. *(Optional)* Configure your server address:
   - Either set `SERVER="http://<YOUR_IP>:8000"` inside `BusTracker.sh`, OR
   - Create a text file `/Volumes/Kindle/documents/tracker_server.txt` containing your server URL (e.g. `http://192.168.1.100:8000`).
3. Safely eject the Kindle.
4. In your Kindle Library, tap **"126 Bus Tracker"**.

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
