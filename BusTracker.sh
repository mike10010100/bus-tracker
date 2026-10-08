#!/bin/sh
# Name: 126 Bus Tracker
# Author: Antigravity

# Prevent Kindle from sleeping / activating screensaver
lipc-set-prop -i com.lab126.powerd preventScreenSaver 1

# Optional: set frontlight (0 = off, 12 = medium, or leave as current)
# lipc-set-prop -i com.lab126.powerd flWorkflow 0

# Your Mac's local network IP and server endpoint
SERVER="http://192.168.86.193:8000/dashboard.png?kindle=pw5"
DEST="/tmp/dashboard.png"

# Clear the screen once on launch
eips -c

while true; do
    # Fetch latest dashboard from your Mac server
    curl -s -m 15 "$SERVER" -o "$DEST"

    if [ -s "$DEST" ]; then
        # Push image directly to Kindle e-ink screen
        eips -g "$DEST"
    fi

    # Auto-refresh every 45 seconds
    sleep 45
done
