#!/bin/sh
# Name: 126 Bus Tracker
# Author: Antigravity
# Ultra-lightweight OTA Bootstrap Loader - Never needs to be updated via USB!

exec 2>/dev/null

SERVER="http://192.168.86.193:8000"
RUNNER="/tmp/client.sh"
FALLBACK="/mnt/us/documents/client_backup.sh"

# 1. Fetch latest runner script directly from Mac into RAM
curl -s -m 8 "$SERVER/client.sh" -o "$RUNNER"

# 2. If successfully downloaded, back it up locally and run it
if [ -s "$RUNNER" ]; then
    cp "$RUNNER" "$FALLBACK" 2>/dev/null
    chmod +x "$RUNNER"
    exec sh "$RUNNER"
fi

# 3. If Mac server is offline, run cached local backup if available
if [ -s "$FALLBACK" ]; then
    cp "$FALLBACK" "$RUNNER"
    chmod +x "$RUNNER"
    exec sh "$RUNNER"
fi

# 4. If completely offline with no cache, show clean notification and exit
eips -c
eips 15 18 "Cannot reach Bus Tracker server at:"
eips 15 20 "$SERVER"
eips 15 23 "Please check Wi-Fi or start server on Mac."
sleep 8
eips -c
lipc-set-prop -i com.lab126.appmgrd start app://com.lab126.booklet.home 2>/dev/null
exit 1
