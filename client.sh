#!/bin/sh
# 126 Bus Tracker - Live Client Runner (Hotloaded from Mac Server)
# Runs in RAM (/tmp) on Kindle. Automatically hot-reloads when updated on Mac.

exec 2>/dev/null

VERSION="1.0.0"
SERVER="http://192.168.86.193:8000"
DEST="/tmp/dashboard.png"
SCRIPT_PATH="/tmp/client.sh"
SCRIPT_NEW="/tmp/client_new.sh"
STOP_FILE="/tmp/stop_tracker"
LIGHT_OVERRIDE_FILE="/tmp/manual_light_override"
HEADERS_FILE="/tmp/headers.txt"

# Ensure clean exit and restore Kindle UI on any exit signal
cleanup() {
    [ -n "$POWER_PID" ] && kill "$POWER_PID" 2>/dev/null
    [ -n "$TOUCH_PID" ] && kill "$TOUCH_PID" 2>/dev/null
    rm -f "$STOP_FILE" "$LIGHT_OVERRIDE_FILE" "$HEADERS_FILE"

    # Re-enable screensaver so Kindle can sleep normally again
    lipc-set-prop -i com.lab126.powerd preventScreenSaver 0 2>/dev/null

    # Clear e-ink screen so old bus image does not linger
    eips -c >/dev/null 2>&1

    # Restore Kindle Home / Library screen
    lipc-set-prop -i com.lab126.appmgrd start app://com.lab126.booklet.home 2>/dev/null
    exit 0
}
trap cleanup INT TERM EXIT

# Prevent Kindle from sleeping while running the tracker
lipc-set-prop -i com.lab126.powerd preventScreenSaver 1 2>/dev/null

# Clean up any leftover temporary files
rm -f "$STOP_FILE" "$LIGHT_OVERRIDE_FILE" "$HEADERS_FILE"

# Background power button listener
(
    lipc-wait-event com.lab126.powerd goingToScreenSaver >/dev/null 2>&1
    touch "$STOP_FILE"
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

    while [ ! -f "$STOP_FILE" ]; do
        dd if="$TOUCH_DEV" bs=64 count=1 >/dev/null 2>&1
        [ -f "$STOP_FILE" ] && break

        NOW=$(date +%s)
        DIFF=$((NOW - LAST_TAP))

        # Debounce
        usleep 400000 2>/dev/null || sleep 1 2>/dev/null
        dd if="$TOUCH_DEV" bs=2048 count=1 >/dev/null 2>&1

        # Double tap within 2 seconds = EXIT!
        if [ $DIFF -le 2 ] && [ $DIFF -ge 0 ]; then
            touch "$STOP_FILE"
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
        date +%s > "$LIGHT_OVERRIDE_FILE"
    done
) &
TOUCH_PID=$!

# Clear screen once on startup
eips -c >/dev/null 2>&1

while [ ! -f "$STOP_FILE" ]; do
    # 1. Check for remote code update from Mac server (Hot-Reloading!)
    curl -s -m 8 "$SERVER/client.sh" -o "$SCRIPT_NEW" 2>/dev/null
    if [ -s "$SCRIPT_NEW" ]; then
        if ! cmp -s "$SCRIPT_NEW" "$SCRIPT_PATH" 2>/dev/null; then
            # New code detected! Replace and hot-reload in-place
            mv "$SCRIPT_NEW" "$SCRIPT_PATH"
            chmod +x "$SCRIPT_PATH"
            [ -n "$POWER_PID" ] && kill "$POWER_PID" 2>/dev/null
            [ -n "$TOUCH_PID" ] && kill "$TOUCH_PID" 2>/dev/null
            exec sh "$SCRIPT_PATH"
        fi
        rm -f "$SCRIPT_NEW"
    fi

    # 2. Fetch latest dashboard image and HTTP headers
    HTTP_CODE=$(curl -s -m 15 -D "$HEADERS_FILE" -w "%{http_code}" "$SERVER/dashboard.png?kindle=pw5" -o "$DEST")

    # If Mac server signals stop (HTTP 205), exit cleanly
    if [ "$HTTP_CODE" = "205" ]; then
        break
    fi

    if [ -s "$DEST" ] && [ "$HTTP_CODE" = "200" ]; then
        # Push image directly to Kindle e-ink screen
        eips -f -g "$DEST" >/dev/null 2>&1

        # Check for auto-dimming lighting headers if no manual override active
        MANUAL_OVERRIDE=0
        if [ -f "$LIGHT_OVERRIDE_FILE" ]; then
            LAST_MANUAL=$(cat "$LIGHT_OVERRIDE_FILE" 2>/dev/null || echo 0)
            NOW=$(date +%s)
            DIFF=$((NOW - LAST_MANUAL))
            if [ $DIFF -lt 2700 ]; then
                MANUAL_OVERRIDE=1
            fi
        fi

        if [ $MANUAL_OVERRIDE -eq 0 ]; then
            AUTO_B=$(grep -i '^X-Kindle-Brightness:' "$HEADERS_FILE" | tr -dc '0-9')
            AUTO_W=$(grep -i '^X-Kindle-Warmth:' "$HEADERS_FILE" | tr -dc '0-9')
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
        if [ -f "$STOP_FILE" ]; then
            break 2
        fi
        sleep 1
        i=$((i + 1))
    done
done

cleanup
