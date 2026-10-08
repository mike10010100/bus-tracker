#!/bin/sh
# Name: 126 Bus Tracker
# Author: Antigravity

# Silence all standard error so nothing ever prints over the e-ink display
exec 2>/dev/null

# Ensure clean exit and restore Kindle UI on any exit signal
cleanup() {
    # Stop background watchers
    if [ -n "$POWER_PID" ]; then
        kill "$POWER_PID" 2>/dev/null
    fi
    if [ -n "$TOUCH_PID" ]; then
        kill "$TOUCH_PID" 2>/dev/null
    fi
    rm -f /tmp/stop_tracker /tmp/manual_light_override /tmp/headers.txt

    # Re-enable screensaver so Kindle can sleep normally again
    lipc-set-prop -i com.lab126.powerd preventScreenSaver 0 2>/dev/null

    # Clear the e-ink screen so old bus image does not linger
    eips -c >/dev/null 2>&1

    # Tell Kindle UI manager to bring back the Home / Library screen
    lipc-set-prop -i com.lab126.appmgrd start app://com.lab126.booklet.home 2>/dev/null
    exit 0
}
trap cleanup INT TERM EXIT

# Prevent Kindle from sleeping while running the tracker
lipc-set-prop -i com.lab126.powerd preventScreenSaver 1 2>/dev/null

# Clean up any leftover temporary files
rm -f /tmp/stop_tracker /tmp/manual_light_override /tmp/headers.txt

# Start background watcher: power button press exits cleanly!
(
    lipc-wait-event com.lab126.powerd goingToScreenSaver >/dev/null 2>&1
    touch /tmp/stop_tracker
    kill -TERM $$ 2>/dev/null
) &
POWER_PID=$!

# Detect touchscreen input device dynamically
find_touch_dev() {
    dev=""
    if [ -f /proc/bus/input/devices ]; then
        dev=$(awk '
            BEGIN { dev="" }
            tolower($0) ~ /touch|ts|mxt|zforce|cyttsp/ { is_touch=1 }
            is_touch && /Handlers=/ {
                for (i=1; i<=NF; i++) {
                    if ($i ~ /^event[0-9]+$/) {
                        dev="/dev/input/" $i
                        print dev
                        exit
                    }
                }
            }
            /^$/ { is_touch=0 }
        ' /proc/bus/input/devices 2>/dev/null)
    fi

    if [ -z "$dev" ] || [ ! -e "$dev" ]; then
        if [ -e "/dev/input/event1" ]; then
            dev="/dev/input/event1"
        else
            dev="/dev/input/event0"
        fi
    fi
    echo "$dev"
}

# Start background touch listener:
# Single tap = cycle frontlight (Off -> Cozy -> Bright -> Off)
# Double tap (within 2 seconds) = EXIT to Home Screen!
(
    TOUCH_DEV=$(find_touch_dev)
    LAST_TAP=0

    while [ ! -f /tmp/stop_tracker ]; do
        # Block until touch detected
        dd if="$TOUCH_DEV" bs=64 count=1 >/dev/null 2>&1
        [ -f /tmp/stop_tracker ] && break

        NOW=$(date +%s)
        DIFF=$((NOW - LAST_TAP))

        # Debounce: drain event stream so one tap isn't multi-triggered
        usleep 400000 2>/dev/null || sleep 1 2>/dev/null
        dd if="$TOUCH_DEV" bs=2048 count=1 >/dev/null 2>&1

        # Double tap within 2 seconds = EXIT!
        if [ $DIFF -le 2 ] && [ $DIFF -ge 0 ]; then
            touch /tmp/stop_tracker
            kill -TERM $$ 2>/dev/null
            break
        fi

        LAST_TAP=$NOW

        # Single tap: cycle frontlight brightness
        CURR=$(lipc-get-prop com.lab126.powerd flIntensity 2>/dev/null)
        case "$CURR" in
            0|"")
                NEXT=8
                WARM=12
                ;;
            [1-9]|1[0-2])
                NEXT=18
                WARM=8
                ;;
            *)
                NEXT=0
                WARM=0
                ;;
        esac

        lipc-set-prop -i com.lab126.powerd flIntensity "$NEXT" 2>/dev/null
        lipc-set-prop -i com.lab126.powerd schedAmberLevel "$WARM" 2>/dev/null

        # Record manual override timestamp (prevents auto-dim override for 45 minutes)
        date +%s > /tmp/manual_light_override
    done
) &
TOUCH_PID=$!

# Server endpoint and temporary destination
SERVER="http://192.168.86.193:8000/dashboard.png?kindle=pw5"
DEST="/tmp/dashboard.png"

# Clear the screen once on launch (silencing output)
eips -c >/dev/null 2>&1

while [ ! -f /tmp/stop_tracker ]; do
    # Fetch latest dashboard and dump headers
    HTTP_CODE=$(curl -s -m 15 -D /tmp/headers.txt -w "%{http_code}" "$SERVER" -o "$DEST")

    # If Mac server signals stop (HTTP 205), exit cleanly
    if [ "$HTTP_CODE" = "205" ]; then
        break
    fi

    if [ -s "$DEST" ] && [ "$HTTP_CODE" = "200" ]; then
        # Push image directly to Kindle e-ink screen (silence debug text)
        eips -f -g "$DEST" >/dev/null 2>&1

        # Check for auto-dimming lighting headers if no manual override active
        MANUAL_OVERRIDE=0
        if [ -f /tmp/manual_light_override ]; then
            LAST_MANUAL=$(cat /tmp/manual_light_override 2>/dev/null || echo 0)
            NOW=$(date +%s)
            DIFF=$((NOW - LAST_MANUAL))
            # 2700 seconds = 45 minutes of manual hold
            if [ $DIFF -lt 2700 ]; then
                MANUAL_OVERRIDE=1
            fi
        fi

        if [ $MANUAL_OVERRIDE -eq 0 ]; then
            AUTO_B=$(grep -i '^X-Kindle-Brightness:' /tmp/headers.txt | tr -dc '0-9')
            AUTO_W=$(grep -i '^X-Kindle-Warmth:' /tmp/headers.txt | tr -dc '0-9')
            if [ -n "$AUTO_B" ]; then
                lipc-set-prop -i com.lab126.powerd flIntensity "$AUTO_B" 2>/dev/null
            fi
            if [ -n "$AUTO_W" ]; then
                lipc-set-prop -i com.lab126.powerd schedAmberLevel "$AUTO_W" 2>/dev/null
            fi
        fi
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
