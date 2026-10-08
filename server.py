import os
import hmac
import time
import io
import json
import hashlib
import urllib.parse
from datetime import datetime
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler
import socket
import threading
from PIL import Image

try:
    from zeroconf import Zeroconf, ServiceInfo
    ZEROCONF_AVAILABLE = True
except ImportError:
    ZEROCONF_AVAILABLE = False

from bus_tracker import NJTransitBusTracker, normalize_arrival
from citibike import CitiBikeTracker
from render_dashboard import render_dashboard, STOPS, get_mock_data, resolve_view
from version import VERSION

PORT = int(os.environ.get("PORT", 8000))
DISCOVERY_PORT = 8001
CACHE_TTL = 30  # Re-fetch upstream data at most once every 30 seconds
# Optional shared secret protecting the /stop and /resume control endpoints.
# When unset (default), control endpoints are only reachable from private
# (RFC1918 / loopback / link-local) addresses.
CONTROL_TOKEN = os.environ.get("TRACKER_CONTROL_TOKEN", "")
SERVER_VERSION = VERSION

tracker = None
cb_tracker = CitiBikeTracker(cache_ttl=30)


def sha256_file(path: str) -> str:
    """Returns the lowercase hex SHA-256 digest of a file."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def is_private_address(addr: str) -> bool:
    """Returns True for loopback, link-local and RFC1918 private addresses."""
    import ipaddress

    try:
        ip = ipaddress.ip_address(addr)
    except ValueError:
        return False
    return ip.is_loopback or ip.is_link_local or ip.is_private


def check_control_auth(handler) -> bool:
    """
    Authorizes a state-changing control request.
    If TRACKER_CONTROL_TOKEN is set, the request must present a matching token
    via the X-Tracker-Token header or ?token= query param. Otherwise the request
    is only accepted from a private/loopback address.
    """
    if CONTROL_TOKEN:
        supplied = handler.headers.get("X-Tracker-Token", "")
        if not supplied:
            supplied = urllib.parse.parse_qs(urllib.parse.urlparse(handler.path).query).get("token", [""])[0]
        return hmac.compare_digest(supplied, CONTROL_TOKEN)
    return is_private_address(handler.client_address[0])


# Upstream data cache (bus arrivals + Citi Bike status), decoupled from render.
# Keying the image cache on battery status previously forced a network refetch
# on every battery change; now the network fetch has its own TTL and rendering
# is cached separately by visual parameters.
_data_lock = threading.Lock()
_data_cache = {"time": 0.0, "stops": None, "status": {}, "cb": None}
_render_cache = {}
_render_lock = threading.Lock()


def get_fresh_data(use_mock=False):
    """
    Returns (stops_data, stop_status, cb_data), refreshing upstream sources at
    most once per CACHE_TTL. Shared by all render requests.
    """
    now = time.time()
    with _data_lock:
        fresh = _data_cache["stops"] is not None and (now - _data_cache["time"] < CACHE_TTL)
        if fresh and not use_mock:
            return _data_cache["stops"], _data_cache["status"], _data_cache["cb"]

    stops_data = {}
    stop_status = {}
    if use_mock:
        stops_data = get_mock_data()
        stop_status = {stop["id"]: NJTransitBusTracker.STATUS_OK for stop in STOPS}
    else:
        global tracker
        if tracker is None:
            tracker = NJTransitBusTracker()
        for stop in STOPS:
            sid = stop["id"]
            status, trips = tracker.get_arrivals_with_status(stop_id=sid, route="126")
            stops_data[sid] = [normalize_arrival(t) for t in trips]
            stop_status[sid] = status

    if use_mock:
        cb_data = cb_tracker.get_mock_data()
    else:
        try:
            cb_data = cb_tracker.get_station_status()
        except Exception as e:
            print(f"[Server] Citi Bike fetch error ({e}), falling back to cached/mock...")
            cb_data = cb_tracker.get_mock_data()

    with _data_lock:
        _data_cache["time"] = now
        _data_cache["stops"] = stops_data
        _data_cache["status"] = stop_status
        _data_cache["cb"] = cb_data

    return stops_data, stop_status, cb_data


def get_fresh_dashboard_image(use_mock=False, batt_level=None, is_charging=False, view="auto", width=800, height=480):
    stops_data, stop_status, cb_data = get_fresh_data(use_mock=use_mock)

    cache_key = (use_mock, view, width, height, batt_level, is_charging, _data_cache["time"])
    with _render_lock:
        cached = _render_cache.get(cache_key)
        if cached is not None:
            return Image.open(io.BytesIO(cached))

    # Render dashboard
    img_path = f"/tmp/server_dashboard_{threading.get_ident()}.png"
    render_dashboard(
        stops_data,
        citibike_data=cb_data,
        stop_status=stop_status,
        output_path=img_path,
        view=view,
        is_mock=use_mock,
        batt_level=batt_level,
        is_charging=is_charging,
        width=width,
        height=height,
    )

    with open(img_path, "rb") as f:
        img_bytes = f.read()

    with _render_lock:
        _render_cache.clear()
        _render_cache[cache_key] = img_bytes

    return Image.open(io.BytesIO(img_bytes))


def format_for_kindle(base_img, orientation="landscape", rotation=90):
    """
    Scales and rotates the base 800x480 dashboard for Kindle Paperwhite 5
    (Native resolution 1236 x 1648).
    """
    if orientation == "landscape":
        # Fit into 1648 x 1236
        target_w, target_h = 1648, 1236
        ratio = min(target_w / base_img.width, target_h / base_img.height)
        new_w = int(base_img.width * ratio)
        new_h = int(base_img.height * ratio)
        resized = base_img.resize((new_w, new_h), Image.Resampling.LANCZOS)

        canvas = Image.new("RGB", (target_w, target_h), "white")
        offset_x = (target_w - new_w) // 2
        offset_y = (target_h - new_h) // 2
        canvas.paste(resized, (offset_x, offset_y))

        # Rotate to match Kindle's portrait framebuffer
        if rotation != 0:
            canvas = canvas.rotate(rotation, expand=True)

        # Kindle's eips expects 8-bit grayscale ('L')
        # If given RGB, eips reads 3 bytes per pixel, squishing the image by 3x!
        return canvas.convert("L")
    return base_img.convert("L")


def is_peak_commute_hours(dt=None):
    """
    Returns True during peak commute windows in Hoboken, NJ:
    - Morning commute: 7:30 AM - 9:30 AM
    - Evening commute: 4:30 PM - 7:00 PM (16:30 - 19:00)
    """
    if dt is None:
        dt = datetime.now()
    hour = dt.hour + dt.minute / 60.0
    return (7.5 <= hour < 9.5) or (16.5 <= hour < 19.0)


def get_commute_lighting(dt=None):
    """
    Returns (brightness, warmth) for the Hoboken, NJ local time.
    A cozy ambient glow (8, 12) is used during the peak morning and evening
    commute windows; the frontlight is off (0, 0) off-peak and overnight to
    save battery. This is a fixed schedule, not sunrise/sunset calculation.
    """
    if is_peak_commute_hours(dt=dt):
        return 8, 12
    return 0, 0


def get_target_poll_interval(dt=None):
    """
    Returns target Kindle poll interval in seconds:
    - 45s during peak commute rush
    - 600s (10 min) off-peak Eco Mode
    """
    if is_peak_commute_hours(dt=dt):
        return 45
    return 600


tracker_stopped = False


class DashboardHandler(BaseHTTPRequestHandler):
    def _send_forbidden(self):
        msg = b"<h1>403 Forbidden</h1><p>Control endpoint requires a token or a private-network origin.</p>"
        self.send_response(403)
        self.send_header("Content-Type", "text/html")
        self.send_header("Content-Length", str(len(msg)))
        self.end_headers()
        self.wfile.write(msg)

    def do_HEAD(self):
        self.do_GET()

    def do_POST(self):
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path == "/log":
            try:
                length = int(self.headers.get("Content-Length", 0))
                body = self.rfile.read(length).decode("utf-8", errors="replace").strip()
                print(f"[Kindle Log] {body}")
            except Exception as e:
                print(f"[Server] Error reading log: {e}")
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        self.send_response(404)
        self.end_headers()

    def do_GET(self):
        global tracker_stopped
        parsed = urllib.parse.urlparse(self.path)
        params = urllib.parse.parse_qs(parsed.query)

        if parsed.path in ["/healthz", "/health"]:
            payload = json.dumps({
                "status": "ok",
                "version": SERVER_VERSION,
                "stopped": tracker_stopped,
            }).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return

        if parsed.path == "/log":
            msg = params.get("msg", [""])[0]
            print(f"[Kindle Log] {msg}")
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return

        if parsed.path == "/stop":
            if not check_control_auth(self):
                self._send_forbidden()
                return
            tracker_stopped = True
            msg = b"<h1>Signal Sent: Kindle Tracker Stopping</h1><p>On next poll, Kindle will exit to Home Screen.</p><p><a href='/resume'>Click here to Resume / Re-enable</a></p>"
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)
            return

        if parsed.path == "/resume" or parsed.path == "/start":
            if not check_control_auth(self):
                self._send_forbidden()
                return
            tracker_stopped = False
            msg = b"<h1>Kindle Tracker Resumed</h1><p><a href='/'>Back to Dashboard</a></p>"
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)
            return

        if parsed.path == "/tracker-arm":
            file_path = os.path.join(os.path.dirname(__file__), "tracker-arm")
            if not os.path.exists(file_path):
                self.send_response(404)
                self.end_headers()
                return

            stat = os.stat(file_path)
            last_mod = time.strftime("%a, %d %b %Y %H:%M:%S GMT", time.gmtime(stat.st_mtime))
            print(f"[Server OTA] client={self.address_string()} cmd={self.command} version={SERVER_VERSION} ims={self.headers.get('If-Modified-Since')}")

            # Handle conditional request (If-Modified-Since)
            ims = self.headers.get("If-Modified-Since")
            if ims == last_mod:
                self.send_response(304)
                self.end_headers()
                return

            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(stat.st_size))
            self.send_header("Last-Modified", last_mod)
            self.send_header("X-Tracker-Version", SERVER_VERSION)
            self.send_header("X-Tracker-Server", f"http://{get_local_ip()}:{PORT}")
            self.send_header("X-Tracker-SHA256", sha256_file(file_path))
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()

            if self.command == "GET":
                try:
                    with open(file_path, "rb") as f:
                        self.wfile.write(f.read())
                except (BrokenPipeError, ConnectionResetError):
                    pass
            return

        if parsed.path in ["/dashboard.png", "/bus.png"]:
            if tracker_stopped:
                # 205 Reset Content signals the Kindle scriptlet to exit cleanly
                self.send_response(205)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return

            use_mock = "mock" in params
            kindle_mode = params.get("kindle", [None])[0]
            rot_val = int(params.get("rotate", [90])[0])
            view_param = params.get("view", [self.headers.get("X-Tracker-View", "auto")])[0]

            # Extract battery and charging status from query params or headers
            batt_param = params.get("batt", params.get("battery", [self.headers.get("X-Kindle-Battery")]))[0]
            charging_param = params.get("charging", [self.headers.get("X-Kindle-Charging")])[0]

            batt_level = None
            if batt_param and str(batt_param).strip().lstrip("-").isdigit():
                val = int(batt_param)
                if 0 <= val <= 100:
                    batt_level = val

            is_charging = str(charging_param).lower() in ["1", "true", "yes"]

            is_kindle = kindle_mode == "pw5" or "kindle" in params
            render_w = 800
            render_h = 600 if is_kindle else 480

            img = get_fresh_dashboard_image(
                use_mock=use_mock,
                batt_level=batt_level,
                is_charging=is_charging,
                view=view_param,
                width=render_w,
                height=render_h,
            )

            if is_kindle:
                img = format_for_kindle(img, orientation="landscape", rotation=rot_val)

            buf = io.BytesIO()
            img.save(buf, format="PNG")
            img_bytes = buf.getvalue()

            brightness, warmth = get_commute_lighting()
            poll_interval = get_target_poll_interval()
            self.send_response(200)
            self.send_header("Content-Type", "image/png")
            self.send_header("Content-Length", str(len(img_bytes)))
            self.send_header("X-Kindle-Brightness", str(brightness))
            self.send_header("X-Kindle-Warmth", str(warmth))
            self.send_header("X-Kindle-Poll-Interval", str(poll_interval))
            self.send_header("X-Tracker-Server", f"http://{get_local_ip()}:{PORT}")
            self.send_header("X-Tracker-View", view_param)
            self.send_header("X-Resolved-View", resolve_view(view_param))
            self.send_header("Cache-Control", "no-cache, no-store, must-revalidate")
            self.end_headers()
            self.wfile.write(img_bytes)

        elif parsed.path in ["/", "/index.html"]:
            current_view = params.get("view", ["auto"])[0]
            token_qs = f"?token={urllib.parse.quote(CONTROL_TOKEN)}" if CONTROL_TOKEN else ""
            status_badge = '<span style="color:#ff6b6b;">STOPPED</span>' if tracker_stopped else '<span style="color:#51cf66;">ACTIVE</span>'
            if tracker_stopped:
                toggle_link = f'<a href="/resume{token_qs}" style="color:#51cf66;">Resume Tracker</a>'
            else:
                toggle_link = f'<a href="/stop{token_qs}" style="color:#ff6b6b;">Stop Kindle Tracker</a>'
            html = f"""<!DOCTYPE html>
<html>
<head>
    <title>NJ Transit 126 & Citi Bike Tracker</title>
    <meta http-equiv="refresh" content="30">
    <style>
        body {{
            background: #222;
            display: flex;
            flex-direction: column;
            align-items: center;
            justify-content: center;
            min-height: 100vh;
            margin: 0;
            font-family: -apple-system, sans-serif;
            color: #ddd;
        }}
        img {{
            max-width: 95vw;
            box-shadow: 0 8px 24px rgba(0,0,0,0.5);
            border-radius: 8px;
        }}
        .status {{
            margin-top: 12px;
            font-size: 16px;
        }}
        .links {{
            margin-top: 10px;
            font-size: 14px;
        }}
        a {{ color: #4da6ff; text-decoration: none; margin: 0 8px; }}
        a:hover {{ text-decoration: underline; }}
    </style>
</head>
<body>
    <img src="/dashboard.png?view={current_view}&t={int(time.time())}" alt="Transit Dashboard" />
    <div class="status">Status: {status_badge} | {toggle_link}</div>
    <div class="links">
        <strong>View Mode:</strong>
        <a href="/?view=auto">Auto (AM Citi / PM Bus)</a> |
        <a href="/?view=morning">Morning (Citi Bike Hero)</a> |
        <a href="/?view=evening">Evening (Bus Hero)</a>
    </div>
    <div class="links">
        <a href="/dashboard.png?view={current_view}" target="_blank">Standard (800x480)</a> |
        <a href="/dashboard.png?kindle=pw5&rotate=90&view={current_view}" target="_blank">Kindle PW5 (Rotated 90°)</a> |
        <a href="/dashboard.png?mock=1&view={current_view}" target="_blank">Mock Preview</a>
    </div>
</body>
</html>"""
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(html.encode("utf-8"))))
            self.end_headers()
            self.wfile.write(html.encode("utf-8"))

        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, format, *args):
        # Concise logging
        print(f"[Server] {self.address_string()} - {args[0]}")


def get_local_ip():
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.connect(("8.8.8.8", 80))
        ip = s.getsockname()[0]
        s.close()
        return ip
    except Exception:
        return "localhost"


def start_discovery_responder(http_port=PORT, version=SERVER_VERSION):
    """
    Listens on UDP 8001 for TRANSIT_TRACKER_DISCOVER broadcasts
    and replies with the server URL and version.
    """
    def responder_loop():
        sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEPORT, 1)
        except (AttributeError, OSError):
            pass
        try:
            sock.bind(("", DISCOVERY_PORT))
            print(f"[Discovery] UDP broadcast responder active on port {DISCOVERY_PORT}")
        except Exception as e:
            print(f"[Discovery] Could not bind UDP {DISCOVERY_PORT}: {e}")
            return

        while True:
            try:
                data, addr = sock.recvfrom(1024)
                msg = data.decode("utf-8", errors="ignore").strip()
                # Accept the legacy BUS_TRACKER_DISCOVER probe so devices running
                # an older binary can still locate the server and OTA-upgrade.
                if "TRANSIT_TRACKER_DISCOVER" in msg or "BUS_TRACKER_DISCOVER" in msg:
                    resp_ip = get_local_ip()
                    reply = f"TRANSIT_TRACKER_OFFER http://{resp_ip}:{http_port} {version}\n".encode("utf-8")
                    sock.sendto(reply, addr)
                    print(f"[Discovery] Answered probe from {addr[0]}:{addr[1]} -> http://{resp_ip}:{http_port}")
            except Exception:
                pass

    t = threading.Thread(target=responder_loop, daemon=True, name="DiscoveryResponder")
    t.start()
    return t


def start_mdns_advertiser(http_port=PORT, version=SERVER_VERSION):
    """
    Registers _transittracker._tcp.local. service with Zeroconf / mDNS.
    """
    if not ZEROCONF_AVAILABLE:
        print("[mDNS] Zeroconf library not installed; skipping mDNS advertisement.")
        return None, None

    try:
        local_ip = get_local_ip()
        ip_bytes = socket.inet_aton(local_ip)
        service_type = "_transittracker._tcp.local."
        service_name = f"TransitTracker._transittracker._tcp.local."
        desc = {"version": version, "endpoint": "/dashboard.png"}

        info = ServiceInfo(
            service_type,
            service_name,
            addresses=[ip_bytes],
            port=http_port,
            properties=desc,
            server="transittracker.local.",
        )
        zc = Zeroconf()
        zc.register_service(info)
        print(f"[mDNS] Registered service {service_name} at {local_ip}:{http_port}")
        return zc, info
    except Exception as e:
        print(f"[mDNS] Failed to register Zeroconf service: {e}")
        return None, None


if __name__ == "__main__":
    server_address = ("", PORT)
    httpd = ThreadingHTTPServer(server_address, DashboardHandler)
    local_ip = get_local_ip()
    print(f"==================================================")
    print(f"  Hoboken Transit Tracker Server v{SERVER_VERSION} on Port {PORT}")
    print(f"  Local View:      http://localhost:{PORT}")
    print(f"  Kindle Endpoint: http://{local_ip}:{PORT}/dashboard.png?kindle=pw5")
    print(f"  Auto-Discovery:  UDP Port {DISCOVERY_PORT} & mDNS (_transittracker._tcp.local)")
    print(f"==================================================")

    start_discovery_responder(http_port=PORT, version=SERVER_VERSION)
    zc, mdns_info = start_mdns_advertiser(http_port=PORT, version=SERVER_VERSION)

    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\nStopping server...")
    finally:
        if zc and mdns_info:
            try:
                zc.unregister_service(mdns_info)
                zc.close()
            except Exception:
                pass
        httpd.server_close()
