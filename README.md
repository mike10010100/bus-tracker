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

### 4. Running the Tracker
```bash
python bus_tracker.py
```

Sample output:
```text
=== Washington St at 9th St (Stop #20512) ===
  [126] NEW YORK VIA PORT AUTHORITY -> in 4 mins (Load: LIGHT)
  [126] NEW YORK VIA PORT AUTHORITY -> in 16 mins (Load: EMPTY)

=== Clinton St at 9th St (Stop #20494) ===
  [126] NEW YORK VIA CLINTON -> in 9 mins (Load: SEATS AVAILABLE)
```

---

## Roadmap

- [x] Feasibility research & API reverse-engineering
- [x] Core Python API client with automatic token refresh
- [ ] Lightweight local web server / JSON endpoint
- [ ] Hardware integration: E-ink wall display (TRMNL / Waveshare e-Paper on Raspberry Pi Zero or ESP32)
- [ ] Home Assistant sensor integration
