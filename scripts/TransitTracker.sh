#!/bin/sh
# Name: Transit Tracker
# Author: Antigravity
# Permanent OTA Bootstrap Launcher - Downloads & launches native Go binary

exec 2>/dev/null

SERVER="${SERVER:-http://192.168.1.100:8000}"
if [ -f /mnt/us/documents/tracker_server.txt ]; then
    SERVER=$(cat /mnt/us/documents/tracker_server.txt | tr -d '\r\n ')
fi
BINARY="/tmp/tracker"
BACKUP="/mnt/us/documents/tracker_backup"

# 0. Wait for Wi-Fi (up to ~30s) so the download below can succeed after a boot.
i=0
while [ $i -lt 15 ] && ! curl -s -m 3 -o /dev/null "$SERVER/healthz"; do
    sleep 2
    i=$((i+1))
done

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
    exec "$BINARY" -server "$SERVER"
fi

# 3. If Mac server is offline, launch cached backup from storage
if [ -s "$BACKUP" ]; then
    cp "$BACKUP" "$BINARY" 2>/dev/null
    chmod +x "$BINARY"
    exec "$BINARY" -server "$SERVER"
fi

# 4. If completely offline with no cache, show clean notification and exit
eips -c
eips 15 18 "Cannot connect to Transit Tracker server at:"
eips 15 20 "$SERVER"
eips 15 23 "Please start server and retry."
sleep 8
eips -c
lipc-set-prop -i com.lab126.appmgrd start app://com.lab126.booklet.home 2>/dev/null
exit 1
