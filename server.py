import os
import sys
import time
import io
import json
import urllib.parse
from datetime import datetime
from http.server import HTTPServer, BaseHTTPRequestHandler
import socket
import threading
from PIL import Image

try:
    from zeroconf import Zeroconf, ServiceInfo
    ZEROCONF_AVAILABLE = True
except ImportError:
    ZEROCONF_AVAILABLE = False

from bus_tracker import NJTransitBusTracker
from citibike import CitiBikeTracker
from render_dashboard import render_dashboard, STOPS, get_mock_data, resolve_view

PORT = int(os.environ.get("PORT", 8000))
DISCOVERY_PORT = 8001
SERVER_VERSION = "1.6.1"
CACHE_TTL = 30  # Re-fetch from NJ Transit at most once every 30 seconds
cached_image_bytes = None
last_render_time = 0
tracker = None
cb_tracker = CitiBikeTracker(cache_ttl=30)


last_batt_level = None
last_is_charging = False
last_view = "auto"
last_width = 800
last_height = 480


def get_fresh_dashboard_image(use_mock=False, batt_level=None, is_charging=False, view="auto", width=800, height=480):
    global cached_image_bytes, last_render_time, tracker, last_batt_level, last_is_charging, last_view, last_width, last_height
    now = time.time()

    # Return cached image if fresh and battery status/view/dimensions unchanged
    if (
        cached_image_bytes
        and (now - last_render_time < CACHE_TTL)
        and not use_mock
        and (batt_level == last_batt_level)
        and (is_charging == last_is_charging)
        and (view == last_view)
        and (width == last_width)
        and (height == last_height)
    ):
        return Image.open(io.BytesIO(cached_image_bytes))

    stops_data = {}
    if use_mock:
        stops_data = get_mock_data()
    else:
        try:
            if tracker is None:
                tracker = NJTransitBusTracker()
            for stop in STOPS:
                sid = stop["id"]
                trips = tracker.get_arrivals(stop_id=sid, route="126")
                arrivals = []
                for t in trips:
                    status = (t.get("departurestatus") or "").strip()
                    dep_time = (t.get("departuretime") or "").strip()
                    eta_str = f"{status} ({dep_time})" if status and dep_time else (status or dep_time or "Scheduled")
                    arrivals.append({
                        "route": t.get("public_route"),
                        "destination": (t.get("header") or "").strip(),
                        "eta": eta_str,
                        "occupancy": t.get("passload"),
                        "vehicle_id": t.get("vehicle_id"),
                    })
                stops_data[sid] = arrivals
        except Exception as e:
            print(f"[Server] API fetch error ({e}), showing offline state...")
            stops_data = {stop["id"]: [] for stop in STOPS}

    # Fetch live Citi Bike station status
    cb_data = []
    if use_mock:
        cb_data = cb_tracker.get_mock_data()
    else:
        try:
            cb_data = cb_tracker.get_station_status()
        except Exception as e:
            print(f"[Server] Citi Bike fetch error ({e}), falling back to cached/mock...")
            cb_data = cb_tracker.get_mock_data()

    # Render dashboard
    img_path = "/tmp/server_dashboard.png"
    render_dashboard(
        stops_data,
        citibike_data=cb_data,
        output_path=img_path,
        view=view,
        is_mock=use_mock,
        batt_level=batt_level,
        is_charging=is_charging,
        width=width,
        height=height,
    )

    with open(img_path, "rb") as f:
        cached_image_bytes = f.read()
    last_render_time = now
    last_batt_level = batt_level
    last_is_charging = is_charging
    last_view = view
    last_width = width
    last_height = height

    return Image.open(io.BytesIO(cached_image_bytes))


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


def get_astronomical_lighting(dt=None):
    """
    Returns (brightness, warmth) based on Hoboken, NJ local time.
    Cozy ambient glow (8, 12) during peak morning & evening commute windows.
    Off (0, 0) during off-peak and overnight hours to save battery.
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
            tracker_stopped = True
            msg = b"<h1>Signal Sent: Kindle Tracker Stopping</h1><p>On next poll, Kindle will exit to Home Screen.</p><p><a href='/resume'>Click here to Resume / Re-enable</a></p>"
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)
            return

        if parsed.path == "/resume" or parsed.path == "/start":
            tracker_stopped = False
            msg = b"<h1>Kindle Tracker Resumed</h1><p><a href='/'>Back to Dashboard</a></p>"
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(msg)))
            self.end_headers()
            self.wfile.write(msg)
            return

        if parsed.path in ["/tracker-arm", "/client.sh"]:
            filename = "tracker-arm" if parsed.path == "/tracker-arm" else "client.sh"
            content_type = "application/octet-stream" if filename == "tracker-arm" else "text/x-sh"
            file_path = os.path.join(os.path.dirname(__file__), filename)
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
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(stat.st_size))
            self.send_header("Last-Modified", last_mod)
            self.send_header("X-Tracker-Version", SERVER_VERSION)
            self.send_header("X-Tracker-Server", f"http://{get_local_ip()}:{PORT}")
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

            brightness, warmth = get_astronomical_lighting()
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
            status_badge = '<span style="color:#ff6b6b;">STOPPED</span>' if tracker_stopped else '<span style="color:#51cf66;">ACTIVE</span>'
            toggle_link = '<a href="/resume" style="color:#51cf66;">Resume Tracker</a>' if tracker_stopped else '<a href="/stop" style="color:#ff6b6b;">Stop Kindle Tracker</a>'
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
    Listens on UDP 8001 for BUS_TRACKER_DISCOVER broadcasts
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
                if "BUS_TRACKER_DISCOVER" in msg or "TRANSIT_TRACKER_DISCOVER" in msg:
                    resp_ip = get_local_ip()
                    reply = f"BUS_TRACKER_OFFER http://{resp_ip}:{http_port} {version}\n".encode("utf-8")
                    sock.sendto(reply, addr)
                    print(f"[Discovery] Answered probe from {addr[0]}:{addr[1]} -> http://{resp_ip}:{http_port}")
            except Exception:
                pass

    t = threading.Thread(target=responder_loop, daemon=True, name="DiscoveryResponder")
    t.start()
    return t


def start_mdns_advertiser(http_port=PORT, version=SERVER_VERSION):
    """
    Registers _bustracker._tcp.local. service with Zeroconf / mDNS.
    """
    if not ZEROCONF_AVAILABLE:
        print("[mDNS] Zeroconf library not installed; skipping mDNS advertisement.")
        return None, None

    try:
        local_ip = get_local_ip()
        ip_bytes = socket.inet_aton(local_ip)
        service_type = "_bustracker._tcp.local."
        service_name = f"BusTracker._bustracker._tcp.local."
        desc = {"version": version, "endpoint": "/dashboard.png"}

        info = ServiceInfo(
            service_type,
            service_name,
            addresses=[ip_bytes],
            port=http_port,
            properties=desc,
            server="bustracker.local.",
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
    httpd = HTTPServer(server_address, DashboardHandler)
    local_ip = get_local_ip()
    print(f"==================================================")
    print(f"  Hoboken Transit Tracker Server Running on Port {PORT}")
    print(f"  Local View:      http://localhost:{PORT}")
    print(f"  Kindle Endpoint: http://{local_ip}:{PORT}/dashboard.png?kindle=pw5")
    print(f"  Auto-Discovery:  UDP Port {DISCOVERY_PORT} & mDNS (_bustracker._tcp.local)")
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
