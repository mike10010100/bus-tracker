#!/bin/sh
# Name: 126 Bus Tracker
# Author: Antigravity
# Permanent OTA Bootstrap Launcher - Downloads & launches native Go binary

exec 2>/dev/null

SERVER="http://192.168.86.193:8000"
BINARY="/tmp/tracker"
BACKUP="/mnt/us/documents/tracker_backup"

# 1. Download compiled binary if updated (conditional curl -z)
curl -s -m 15 -z "$BINARY" "$SERVER/tracker-arm" -o "$BINARY"

# 2. If valid binary in RAM, cache it to storage and execute it
if [ -s "$BINARY" ]; then
    chmod +x "$BINARY"
    cp "$BINARY" "$BACKUP" 2>/dev/null
    exec "$BINARY"
fi

# 3. If Mac server is offline, launch cached backup
if [ -s "$BACKUP" ]; then
    cp "$BACKUP" "$BINARY" 2>/dev/null
    chmod +x "$BINARY"
    exec "$BINARY"
fi

# 4. Offline message
eips -c
eips 15 18 "Cannot connect to Bus Tracker server at:"
eips 15 20 "$SERVER"
eips 15 23 "Please start server on Mac and retry."
sleep 8
eips -c
lipc-set-prop -i com.lab126.appmgrd start app://com.lab126.booklet.home 2>/dev/null
exit 1
