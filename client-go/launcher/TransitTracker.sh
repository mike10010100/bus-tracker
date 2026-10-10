#!/bin/sh
# Name: Transit Tracker
# Author: Antigravity
# Permanent OTA Bootstrap Launcher - Launches native Go binary

set -eu

LOG_FILE="/tmp/tracker_bootstrap.log"
exec 2>>"$LOG_FILE" || true

SERVER="${SERVER:-http://192.168.1.100:8000}"
SERVER_CONFIG="/mnt/us/documents/tracker_server.txt"

if [ -f "$SERVER_CONFIG" ]; then
    SERVER_VAL="$(tr -d '\r\n ' < "$SERVER_CONFIG")"
    if [ -n "$SERVER_VAL" ]; then
        SERVER="$SERVER_VAL"
    fi
fi

case "$SERVER" in
    http://*|https://*) ;;
    *) SERVER="http://$SERVER" ;;
esac

BINARY="/tmp/tracker"
BACKUP="/mnt/us/documents/tracker_backup"
DL_TMP="/tmp/tracker.dl"

cleanup() {
    rm -f "$DL_TMP"
}
trap cleanup EXIT INT TERM

is_elf() {
    target_file="$1"
    [ -f "$target_file" ] && [ "$(dd if="$target_file" bs=4 count=1 2>/dev/null)" = "$(printf '\177ELF')" ]
}

# 1. Wait for Wi-Fi (up to ~30s)
i=0
while [ "$i" -lt 15 ] && ! curl -fs -m 3 -o /dev/null "$SERVER/healthz"; do
    sleep 2
    i=$((i + 1))
done

# 2. If /tmp/tracker is missing or not executable, and backup passes ELF check, restore it
if [ ! -x "$BINARY" ] && is_elf "$BACKUP"; then
    cp "$BACKUP" "$BINARY" 2>/dev/null || true
    chmod +x "$BINARY" 2>/dev/null || true
fi

# 3. If /tmp/tracker passes ELF check, exec it
if is_elf "$BINARY" && [ -x "$BINARY" ]; then
    exec "$BINARY" -server "$SERVER" -launcher "$0"
fi

# 4. First-install bootstrap only (no trusted binary on device)
if [ ! -s "$BACKUP" ]; then
    echo "First-install bootstrap: downloading initial tracker-arm (trust-on-first-use)..." >&2
    if curl -fs -m 30 --max-filesize 33554432 "$SERVER/tracker-arm" -o "$DL_TMP" 2>/dev/null; then
        if is_elf "$DL_TMP"; then
            chmod 0755 "$DL_TMP"
            mv "$DL_TMP" "$BINARY"
            exec "$BINARY" -server "$SERVER" -launcher "$0"
        else
            echo "Downloaded binary failed ELF check" >&2
            rm -f "$DL_TMP"
        fi
    fi
fi

# 5. Otherwise show cannot connect message
if command -v eips >/dev/null 2>&1; then
    eips -c
    eips 15 18 "Cannot connect to Transit Tracker server at:"
    eips 15 20 "$SERVER"
    eips 15 23 "Please start server and retry."
    sleep 8
    eips -c
else
    echo "Cannot connect to Transit Tracker server at: $SERVER" >&2
fi

if command -v lipc-set-prop >/dev/null 2>&1; then
    lipc-set-prop -i com.lab126.appmgrd start app://com.lab126.booklet.home 2>/dev/null || true
fi
