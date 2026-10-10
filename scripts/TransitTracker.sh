#!/bin/sh
# Name: Transit Tracker
# Author: Antigravity
# Permanent OTA Bootstrap Launcher - Downloads & launches native Go binary

set -eu

# Optional bootstrap log file to prevent completely silencing stderr while keeping clean output
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

# Ensure http:// or https:// prefix
case "$SERVER" in
    http://*|https://*) ;;
    *) SERVER="http://$SERVER" ;;
esac

BINARY="/tmp/tracker"
BACKUP="/mnt/us/documents/tracker_backup"
DL_TMP="/tmp/tracker.dl"

# shellcheck disable=SC2329
cleanup() {
    rm -f "$DL_TMP"
}
trap cleanup EXIT INT TERM

# 0. Wait for Wi-Fi (up to ~30s) so download can succeed after boot
i=0
while [ "$i" -lt 15 ] && ! curl -fs -m 3 -o /dev/null "$SERVER/healthz"; do
    sleep 2
    i=$((i + 1))
done

# 1. Determine reference file for If-Modified-Since (-z)
# If RAM binary is missing (e.g. after reboot), use persistent backup as reference to allow HTTP 304.
REF_FILE=""
if [ -s "$BINARY" ]; then
    REF_FILE="$BINARY"
elif [ -s "$BACKUP" ]; then
    REF_FILE="$BACKUP"
fi

DOWNLOAD_SUCCESS=0
if [ -n "$REF_FILE" ]; then
    if curl -fs -m 20 -z "$REF_FILE" "$SERVER/tracker-arm" -o "$DL_TMP" 2>/dev/null; then
        DOWNLOAD_SUCCESS=1
    fi
else
    if curl -fs -m 20 "$SERVER/tracker-arm" -o "$DL_TMP" 2>/dev/null; then
        DOWNLOAD_SUCCESS=1
    fi
fi

if [ "$DOWNLOAD_SUCCESS" -eq 1 ]; then
    if [ -s "$DL_TMP" ]; then
        chmod +x "$DL_TMP"
        mv "$DL_TMP" "$BINARY"
        cp "$BINARY" "$BACKUP" 2>/dev/null || true
    elif [ ! -x "$BINARY" ] && [ -s "$BACKUP" ]; then
        # HTTP 304 Not Modified returned and binary was in backup
        cp "$BACKUP" "$BINARY" 2>/dev/null || true
        chmod +x "$BINARY" 2>/dev/null || true
    fi
fi

# 2. If valid binary in RAM, execute it
if [ -x "$BINARY" ]; then
    exec "$BINARY" -server "$SERVER"
fi

# 3. If server was unreachable, launch cached backup from storage
if [ -s "$BACKUP" ]; then
    cp "$BACKUP" "$BINARY" 2>/dev/null || true
    chmod +x "$BINARY" 2>/dev/null || true
    if [ -x "$BINARY" ]; then
        exec "$BINARY" -server "$SERVER"
    fi
fi

# 4. If completely offline with no cache, show notification and exit
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

exit 1
