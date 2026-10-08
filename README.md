# NJ Transit Bus Tracker (Route 126)

A real-time bus arrival tracker for NJ Transit Route 126 in Hoboken, NJ, tracking NYC-bound buses at:
- **Washington St at 9th St** (Stop `#20512`)
- **Clinton St at 9th St** (Stop `#20494`)

Built to eventually power an e-ink wall display or smart home dashboard.

---

## How It Works

This client interfaces directly with NJ Transit's official **Bus DepartureVision (BUSDV2)** API (`https://pcsdata.njtransit.com/api/BUSDV2`).

- **Automated Authentication**: NJ Transit issues 24-hour API tokens. The client handles token generation and renewal automatically in the background without any manual interaction.
- **Real-Time Data**: Fetches live arrival estimates (`"in 4 mins"`, `"APPROACHING"`), passenger occupancy load, and vehicle IDs for each stop.

---

## Setup

### 1. Developer Account
Register for a free account at the [NJ Transit Developer Portal](https://developer.njtransit.com/registration). Once approved, you will receive your API username and password.

### 2. Installation
Install dependencies:
```bash
pip install -r requirements.txt
```

### 3. Configuration
Copy `.env.example` to `.env` and fill in your credentials:
```bash
cp .env.example .env
```
Edit `.env`:
```ini
NJT_USERNAME=your_username
NJT_PASSWORD=your_password
```

### 4. Running the Tracker (CLI)
```bash
python bus_tracker.py
```

### 5. Generating E-Ink Dashboard Images
Generate a crisp 800×480 black-and-white image formatted for low-power displays (TRMNL, Waveshare 7.5", LilyGO):
```bash
# Render using live NJ Transit data:
python render_dashboard.py

# Render with mock peak-commute data for previewing:
python render_dashboard.py --mock
```
This saves `dashboard.png` in the project root.

### 5. Running the Local Dashboard Server
Start the local server that generates and serves images over your Wi-Fi network:
```bash
python server.py
```
* **Web View (Auto-reloading):** `http://localhost:8000`
* **Kindle Endpoint:** `http://<YOUR_MAC_IP>:8000/dashboard.png?kindle=pw5`

---

## Kindle Paperwhite Dashboard Setup

If using a jailbroken **Kindle Paperwhite (PW5)**:

1. Copy `BusTracker.sh` to your Kindle's `documents/` folder.
2. In your Kindle Library, tap **"126 Bus Tracker"**.
3. The script will:
   * Disable the screensaver timeout.
   * Auto-fetch `dashboard.png?kindle=pw5` from your Mac server every 45 seconds.
   * Render it directly to the e-ink screen using Kindle's native `eips` framebuffer tool.

---

## Roadmap

- [x] Feasibility research & API reverse-engineering
- [x] Core Python API client with automatic token refresh
- [x] High-contrast 800×480 E-Ink graphic renderer ([render_dashboard.py](file:///Users/mike10010100/git/bus-tracker/render_dashboard.py))
- [x] Local HTTP image server with caching and Kindle PW5 rotation ([server.py](file:///Users/mike10010100/git/bus-tracker/server.py))
- [x] Native Kindle Paperwhite scriptlet ([BusTracker.sh](file:///Users/mike10010100/git/bus-tracker/BusTracker.sh))
- [ ] Smart scheduling (e.g. active refreshes during 6:30 AM – 9:30 AM commute hours)
