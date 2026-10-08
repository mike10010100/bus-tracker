#!/bin/sh
# Name: 126 Bus Tracker
# Author: Antigravity

# Ensure clean exit and restore Kindle UI on any exit signal
cleanup() {
    # Stop background power button watcher if still running
    if [ -n "$POWER_PID" ]; then
        kill "$POWER_PID" 2>/dev/null
    fi
    rm -f /tmp/stop_tracker

    # Re-enable screensaver so Kindle can sleep normally again
    lipc-set-prop -i com.lab126.powerd preventScreenSaver 0 2>/dev/null

    # Tell Kindle UI manager to bring back the Home / Library screen
    lipc-set-prop -i com.lab126.appmgrd start app://com.lab126.booklet.home 2>/dev/null
    exit 0
}
trap cleanup INT TERM EXIT

# Prevent Kindle from sleeping while running the tracker
lipc-set-prop -i com.lab126.powerd preventScreenSaver 1 2>/dev/null

# Clean up any leftover stop file
rm -f /tmp/stop_tracker

# Start background watcher: single click of the power button exits cleanly!
(
    lipc-wait-event com.lab126.powerd PowerButtonQuickPress >/dev/null 2>&1
    touch /tmp/stop_tracker
    kill -TERM $$ 2>/dev/null
) &
POWER_PID=$!

# Server endpoint and temporary destination
SERVER="http://192.168.86.193:8000/dashboard.png?kindle=pw5"
DEST="/tmp/dashboard.png"

# Clear the screen once on launch (silencing output)
eips -c >/dev/null 2>&1

while [ ! -f /tmp/stop_tracker ]; do
    # Fetch latest dashboard from your Mac server
    HTTP_CODE=$(curl -s -m 15 -w "%{http_code}" "$SERVER" -o "$DEST")

    # If Mac server signals stop (HTTP 205), exit cleanly
    if [ "$HTTP_CODE" = "205" ]; then
        break
    fi

    if [ -s "$DEST" ] && [ "$HTTP_CODE" = "200" ]; then
        # Push image directly to Kindle e-ink screen (silence debug text)
        eips -f -g "$DEST" >/dev/null 2>&1
    fi

    # Interruptible sleep for 45 seconds (checks stop flag every second)
    i=0
    while [ $i -lt 45 ]; do
        if [ -f /tmp/stop_tracker ]; then
            break 2
        fi
        sleep 1
        i=$((i + 1))
    done
done

cleanup
