# Hardware & Kindle Paperwhite Guide

This guide covers deployment, hardware interfacing, gesture controls, and display management on jailbroken Amazon Kindle devices, focusing on the **Kindle Paperwhite 5 (PW5, 11th Generation)**.

---

## 1. Hardware Specifications

| Specification | Kindle Paperwhite 5 (PW5) | Standard Kindle (PW3 / PW4) |
|---|---|---|
| **SoC Architecture** | NXP i.MX7Dual (ARM Cortex-A7, 32-bit ARMv7) | NXP i.MX6SoloLite / i.MX6SLL |
| **Panel Size** | 6.8" E-Ink Carta 1200 | 6.0" E-Ink Carta |
| **Panel Resolution**| 1236 × 1648 (Portrait) / 1648 × 1236 (Landscape) | 1072 × 1448 or 600 × 800 |
| **Pixel Density** | 300 PPI | 212–300 PPI |
| **Digitizer** | FocalTech `pt_mt` Multi-Touch (`/dev/input/event1`) | Neonode zForce IR or Synaptics |
| **Power Key** | ROHM BD71828 PMIC Key (`/dev/input/event0`) | GPIO / PMIC Key (`/dev/input/event0`) |
| **Frontlight** | 17 LEDs (White + Amber Warmth) via `lipc` | Single White Channel |
| **OS Environment** | Custom Linux 4.9.x kernel with Lab126 userland | Linux 3.x / 4.x |

---

## 2. Jailbreak Prerequisites

1. The Kindle must have a working software jailbreak (e.g. **LanguageBreak** or **WinterBreak** for FW 5.14.x–5.16.x).
2. The device must have developer keystores installed to allow shell scripts to execute from the user documents directory.
3. Wi-Fi must be configured and connected to the same LAN subnet as the Transit Tracker server.

---

## 3. Installation via Launcher Script

The bootstrap script [`client-go/launcher/TransitTracker.sh`](file:///home/mike10010100/git/transit-tracker/client-go/launcher/TransitTracker.sh) provides zero-touch onboarding:

1. Connect the Kindle to your computer over USB.
2. Copy `client-go/launcher/TransitTracker.sh` to the Kindle documents partition:
   ```bash
   cp client-go/launcher/TransitTracker.sh /Volumes/Kindle/documents/
   ```
3. Safely eject the Kindle over USB.
4. In your Kindle Library, tap the new **"Transit Tracker"** booklet.

### 3.1 First-Run Boot Sequence
```mermaid
sequenceDiagram
    participant K as Launcher (TransitTracker.sh)
    participant W as Kindle Wi-Fi
    participant S as Transit Tracker Server
    participant B as tracker-arm Binary

    K->>W: Wait for wlan0 IP address
    K->>S: Cascading LAN Discovery (mDNS -> Gateway -> /24 Subnet Sweep)
    S-->>K: 200 OK (Server Verified via /identity)
    K->>K: Atomically write /mnt/us/documents/tracker_server.txt
    K->>S: Download tracker-arm (verify ELF \x7fELF header)
    K->>K: Create /tmp/tracker-arm.bak
    K->>B: exec /tmp/tracker-arm -server http://... -launcher TransitTracker.sh
```

---

## 4. Touch Gestures & Button Zones

The Go client directly parses raw evdev input events from `/dev/input/event1` (`pt_mt` digitizer).

### 4.1 On-Screen Tactile Button Bar
Along the bottom edge of the landscape display, five distinct tactile touch zones are mapped:

| Zone | Action | Function |
|---|---|---|
| **BUSES** | Switch View | Activates Route 126 NJ Transit Bus departures view. |
| **CITI BIKE**| Switch View | Activates Citi Bike dock & e-bike availability view. |
| **LIGHT** | Frontlight Cycle | Toggles frontlight: **Off (0)** $\rightarrow$ **Cozy (8)** $\rightarrow$ **Bright (18)** $\rightarrow$ **Off (0)**. |
| **REFRESH** | Immediate Fetch | Bypasses remaining poll countdown and forces an immediate arrival update. |
| **EXIT** | Clean Exit | Restores standard Kindle Framework (`lipc-set-prop com.lab126.appmgrd start app://com.lab126.booklet.home`). |

### 4.2 Corner & Surface Gestures
Outside the bottom button bar:
- **Single Tap Anywhere**: Keeps the current interaction session awake without flickering the e-ink screen.
- **Double Tap Anywhere (< 380ms)**: Fast exit shortcut back to Kindle Home.
- **Top-Left Corner**: Immediate arrival refresh shortcut.
- **Top-Right Corner**: Instant exit shortcut.
- **Bottom-Left Corner**: Cycle views between Citi Bike and Bus departures.

### 4.3 Hardware Power Button
Pressing the physical Kindle power button generates `KEY_POWER` (`116`) events on `/dev/input/event0`:
- **When Screen is Dormant/Off**: Wakes the SoC and starts an interactive viewing session with frontlight illumination.
- **When Running Interactive**: Gracefully exits the application back to the Kindle Home booklet.

---

## 5. Frontlight & Power Management

### 5.1 Hardware Lipc Commands
The Go client communicates with the Kindle power daemon (`powerd`) over the Lab126 Inter-Process Communication bus (`lipc`):

```bash
# Query current battery percentage
lipc-get-prop com.lab126.powerd battLevel

# Query charging state (1 = charging, 0 = discharging)
lipc-get-prop com.lab126.powerd isCharging

# Set frontlight intensity (0 to 24)
lipc-set-prop com.lab126.powerd flIntensity 8

# Set frontlight color warmth (0 = pure white, 24 = deep amber)
lipc-set-prop com.lab126.powerd flWarmth 12
```

### 5.2 Battery Protection & Bound Timeouts
All calls to `lipc` in `client-go/main.go` are strictly bounded with a 2-second timeout (`lipcCallTimeout`). If `powerd` stalls or deadlocks after an OS sleep resume, the input dispatcher automatically recovers rather than freezing touch interaction.

---

## 6. E-Ink Framebuffer Pipeline

1. **Resolution Detection**: The client reads `/sys/class/graphics/fb0/virtual_size` to determine the native panel dimensions.
2. **Server-Side Native Rendering**: The server renders images at the exact pixel geometry (1648×1236 landscape) and performs a pure coordinate transpose to portrait before sending.
3. **Hardware Waveform Ingestion**:
   - The client invokes the Kindle native binary `/usr/sbin/eips`:
     ```bash
     /usr/sbin/eips -g /tmp/dashboard.png
     ```
   - Hardware waveform controllers on the Kindle drive the micro-capsules directly, preventing ghosting while avoiding slow full-screen flashing during daytime operation.
