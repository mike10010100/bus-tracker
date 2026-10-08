#!/bin/sh
# Name: 126 Bus Tracker
# Author: Antigravity
# Permanent OTA Bootstrap Launcher - Downloads & launches native Go binary

exec 2>/dev/null

SERVER="http://192.168.86.193:8000"
BINARY="/tmp/tracker"
BACKUP="/mnt/us/documents/tracker_backup"

# 1. Check for binary update over Wi-Fi
curl -s -m 20 -z "$BINARY" "$SERVER/tracker-arm" -o "/tmp/tracker.dl"
if [ -s "/tmp/tracker.dl" ]; then
    mv "/tmp/tracker.dl" "$BINARY"
    chmod +x "$BINARY"
    cp "$BINARY" "$BACKUP" 2>/dev/null
else
    rm -f "/tmp/tracker.dl"
fi

# 2. If valid binary in RAM, execute it
if [ -x "$BINARY" ]; then
    exec "$BINARY"
fi

# 3. If Mac server is offline, launch cached backup from storage
if [ -s "$BACKUP" ]; then
    cp "$BACKUP" "$BINARY" 2>/dev/null
    chmod +x "$BINARY"
    exec "$BINARY"
fi

# 4. If completely offline with no cache, show clean notification and exit
eips -c
eips 15 18 "Cannot connect to Bus Tracker server at:"
eips 15 20 "$SERVER"
eips 15 23 "Please start server on Mac and retry."
sleep 8
eips -c
lipc-set-prop -i com.lab126.appmgrd start app://com.lab126.booklet.home 2>/dev/null
exit 1
