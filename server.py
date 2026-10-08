import os
import sys
import time
import io
import urllib.parse
from http.server import HTTPServer, BaseHTTPRequestHandler
from PIL import Image

from bus_tracker import NJTransitBusTracker
from render_dashboard import render_dashboard, STOPS, get_mock_data

PORT = 8000
CACHE_TTL = 30  # Re-fetch from NJ Transit at most once every 30 seconds
cached_image_bytes = None
last_render_time = 0
tracker = None


def get_fresh_dashboard_image(use_mock=False):
    global cached_image_bytes, last_render_time, tracker
    now = time.time()

    # Return cached image if fresh
    if cached_image_bytes and (now - last_render_time < CACHE_TTL) and not use_mock:
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
            print(f"[Server] API fetch error ({e}), falling back to mock data...")
            stops_data = get_mock_data()
            use_mock = True

    # Render base 800x480 dashboard
    img_path = "/tmp/server_dashboard.png"
    render_dashboard(stops_data, output_path=img_path, is_mock=use_mock)

    with open(img_path, "rb") as f:
        cached_image_bytes = f.read()
    last_render_time = now

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


tracker_stopped = False


class DashboardHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        global tracker_stopped
        parsed = urllib.parse.urlparse(self.path)
        params = urllib.parse.parse_qs(parsed.query)

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

            img = get_fresh_dashboard_image(use_mock=use_mock)

            if kindle_mode == "pw5" or "kindle" in params:
                img = format_for_kindle(img, orientation="landscape", rotation=rot_val)

            buf = io.BytesIO()
            img.save(buf, format="PNG")
            img_bytes = buf.getvalue()

            self.send_response(200)
            self.send_header("Content-Type", "image/png")
            self.send_header("Content-Length", str(len(img_bytes)))
            self.send_header("Cache-Control", "no-cache, no-store, must-revalidate")
            self.end_headers()
            self.wfile.write(img_bytes)

        elif parsed.path in ["/", "/index.html"]:
            status_badge = '<span style="color:#ff6b6b;">STOPPED</span>' if tracker_stopped else '<span style="color:#51cf66;">ACTIVE</span>'
            toggle_link = '<a href="/resume" style="color:#51cf66;">Resume Tracker</a>' if tracker_stopped else '<a href="/stop" style="color:#ff6b6b;">Stop Kindle Tracker</a>'
            html = f"""<!DOCTYPE html>
<html>
<head>
    <title>NJ Transit 126 Bus Tracker</title>
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
            margin-top: 12px;
            font-size: 14px;
        }}
        a {{ color: #4da6ff; text-decoration: none; margin: 0 8px; }}
    </style>
</head>
<body>
    <img src="/dashboard.png?t={int(time.time())}" alt="Bus Tracker Dashboard" />
    <div class="status">Status: {status_badge} | {toggle_link}</div>
    <div class="links">
        <a href="/dashboard.png" target="_blank">Standard (800x480)</a> |
        <a href="/dashboard.png?kindle=pw5&rotate=90" target="_blank">Kindle PW5 (Rotated 90°)</a> |
        <a href="/dashboard.png?kindle=pw5&rotate=270" target="_blank">Kindle PW5 (Rotated 270°)</a> |
        <a href="/dashboard.png?mock=1" target="_blank">Mock Preview</a>
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


if __name__ == "__main__":
    server_address = ("", PORT)
    httpd = HTTPServer(server_address, DashboardHandler)
    print(f"==================================================")
    print(f"  NJ Transit Bus Tracker Server Running on Port {PORT}")
    print(f"  Local Mac View:  http://localhost:{PORT}")
    print(f"  Kindle Endpoint: http://192.168.86.193:{PORT}/dashboard.png?kindle=pw5")
    print(f"==================================================")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\nStopping server...")
        httpd.server_close()
