import os
import re
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
from render_dashboard import render_dashboard, STOPS, get_mock_data, resolve_view, WIDTH
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


BINARY_PATH = os.path.join(os.path.dirname(__file__), "tracker-arm")
_binary_info_cache = {"mtime": None, "sha256": "", "size": 0}
_binary_info_lock = threading.Lock()


def get_binary_info():
    """
    Returns (exists, mtime, sha256, size) for the OTA binary, caching the
    (expensive) SHA-256 digest and keying the cache on mtime so repeated
    dashboard requests don't re-hash the 6MB binary every cycle.
    """
    try:
        stat = os.stat(BINARY_PATH)
    except OSError:
        return False, 0, "", 0

    with _binary_info_lock:
        if _binary_info_cache["mtime"] == stat.st_mtime:
            return True, stat.st_mtime, _binary_info_cache["sha256"], _binary_info_cache["size"]
        digest = sha256_file(BINARY_PATH)
        _binary_info_cache.update({"mtime": stat.st_mtime, "sha256": digest, "size": stat.st_size})
    return True, stat.st_mtime, _binary_info_cache["sha256"], _binary_info_cache["size"]


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


# Last device diagnostics report uploaded by a client, plus a one-shot flag that
# asks the next polling client to upload a fresh one.
_diag_lock = threading.Lock()
_last_diagnostics = {"text": "", "time": 0.0, "battery": None, "charging": None}
_diag_requested = ""

# The run mode the server wants clients to be in ("" = don't care, else one of
# VALID_RUN_MODES). This is STICKY: it is re-asserted on every poll where the
# client reports a different mode, so a client that restarts (e.g. after an OTA)
# falls back into the desired mode automatically. Set at startup from
# TRACKER_DEFAULT_MODE and changed at runtime via /mode?set=...
VALID_RUN_MODES = ("resident", "oneshot", "sleep", "sleep-suspend")
_mode_requested = os.environ.get("TRACKER_DEFAULT_MODE", "").strip().lower()
if _mode_requested not in VALID_RUN_MODES:
    _mode_requested = ""

# A named device action to forward to the next polling client (one-shot).
# The set of valid actions is enforced on the client (allowlist in actions.go);
# the server only forwards the string.
_device_action = ""


def parse_diag_battery(text):
    """
    Extracts (level, charging) from a diagnostics report's machine-readable
    'battery_level=<n> charging=<bool>' line. Returns (None, None) if absent.
    """
    m = re.search(r"battery_level=(-?\d+)\s+charging=(true|false)", text)
    if not m:
        return None, None
    level = int(m.group(1))
    if level < 0:
        return None, None
    return level, m.group(2) == "true"


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


def get_fresh_dashboard_image(use_mock=False, batt_level=None, is_charging=False, view="auto", width=800, height=480, scale=1.0):
    stops_data, stop_status, cb_data = get_fresh_data(use_mock=use_mock)

    cache_key = (use_mock, view, width, height, scale, batt_level, is_charging, _data_cache["time"])
    with _render_lock:
        cached = _render_cache.get(cache_key)
        if cached is not None:
            return Image.open(io.BytesIO(cached))

    # Render dashboard natively at the requested scale (no bitmap upscaling).
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
        scale=scale,
    )

    with open(img_path, "rb") as f:
        img_bytes = f.read()

    with _render_lock:
        _render_cache.clear()
        _render_cache[cache_key] = img_bytes

    return Image.open(io.BytesIO(img_bytes))


# Kindle Paperwhite 5 native framebuffer (portrait). Landscape = 1648x1236.
PW5_NATIVE = (1236, 1648)
PW5_LANDSCAPE = (1648, 1236)


def native_render_scale(landscape_w=1648, landscape_h=1236, logical_w=800):
    """
    Computes the scale factor that maps the logical 800px design space onto a
    native landscape panel. We render at this scale so glyphs are rasterized
    natively rather than upscaled from an 800px bitmap.
    """
    return landscape_w / logical_w


def sanitize_kindle_panel(w, h):
    """
    Validates client-reported landscape panel dimensions, falling back to the
    PW5 default when they are implausible. Guards against a client that reports
    a backing-buffer size (double-buffered / height-aligned, e.g. 3296x1248)
    rather than the true visible resolution, which would render a distorted,
    needlessly huge canvas.
    """
    try:
        w = int(w)
        h = int(h)
    except (TypeError, ValueError):
        return PW5_LANDSCAPE
    if not (600 <= w <= 2200 and 400 <= h <= 1800):
        return PW5_LANDSCAPE
    ratio = w / h
    if not (1.2 <= ratio <= 1.6):
        return PW5_LANDSCAPE
    return (w, h)


def format_for_kindle(base_img, orientation="landscape", rotation=90, target=None):
    """
    Prepares an already-rendered dashboard for the Kindle Paperwhite 5.

    When the server renders natively (the base image is already the exact
    landscape panel size) this only rotates it to portrait and converts to
    8-bit grayscale -- no resampling. For backward compatibility with callers
    that pass an 800x480/800x600 base image, it falls back to a single LANCZOS
    fit into the target landscape size.
    """
    if orientation == "landscape":
        target_w, target_h = target or PW5_LANDSCAPE
        if (base_img.width, base_img.height) == (target_w, target_h):
            canvas = base_img
        else:
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


def _parse_hour_env(name, default):
    """Parses a decimal-hour env var (e.g. '9.5' or '10'), falling back to
    default on any error."""
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        return float(raw)
    except ValueError:
        return default


# Peak commute windows are configurable so the schedule can be tuned without a
# code change (e.g. extending the morning window while testing on the device).
PEAK_AM_START = _parse_hour_env("PEAK_AM_START", 7.5)
PEAK_AM_END = _parse_hour_env("PEAK_AM_END", 9.5)
PEAK_PM_START = _parse_hour_env("PEAK_PM_START", 16.5)
PEAK_PM_END = _parse_hour_env("PEAK_PM_END", 19.0)


def is_peak_commute_hours(dt=None):
    """
    Returns True during peak commute windows in Hoboken, NJ. Defaults:
    - Morning commute: 7:30 AM - 9:30 AM
    - Evening commute: 4:30 PM - 7:00 PM (16:30 - 19:00)
    Override with PEAK_AM_START/PEAK_AM_END/PEAK_PM_START/PEAK_PM_END.
    """
    if dt is None:
        dt = datetime.now()
    hour = dt.hour + dt.minute / 60.0
    return (PEAK_AM_START <= hour < PEAK_AM_END) or (PEAK_PM_START <= hour < PEAK_PM_END)


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


# When set (any non-empty value), the server always advertises the fast poll
# interval regardless of the time of day. For testing only.
FORCE_FAST_POLL = os.environ.get("FORCE_FAST_POLL", "").strip().lower() in ("1", "true", "yes", "on")

# Overnight "deep eco" window: a long poll interval while nobody is commuting.
# The window may wrap past midnight (start > end).
OVERNIGHT_START = _parse_hour_env("OVERNIGHT_START", 22.0)
OVERNIGHT_END = _parse_hour_env("OVERNIGHT_END", 6.0)
OVERNIGHT_INTERVAL = int(_parse_hour_env("OVERNIGHT_INTERVAL", 3600))
OFFPEAK_INTERVAL = int(_parse_hour_env("OFFPEAK_INTERVAL", 600))


def is_overnight_hours(dt=None):
    """
    Returns True inside the overnight deep-eco window (default 22:00-06:00).
    Handles a window that wraps past midnight.
    """
    if dt is None:
        dt = datetime.now()
    hour = dt.hour + dt.minute / 60.0
    if OVERNIGHT_START <= OVERNIGHT_END:
        return OVERNIGHT_START <= hour < OVERNIGHT_END
    return hour >= OVERNIGHT_START or hour < OVERNIGHT_END


def get_target_poll_interval(dt=None):
    """
    Returns target Kindle poll interval in seconds:
    - 60s during peak commute rush (the client aligns this to the top of each
      minute, so the on-screen clock rolls exactly when the new data lands)
    - 3600s (1 hour) overnight deep-eco mode
    - 600s (10 min) off-peak Eco Mode

    FORCE_FAST_POLL=1 forces the fast interval at all times (testing aid).
    """
    if FORCE_FAST_POLL:
        return 60
    if is_peak_commute_hours(dt=dt):
        return 60
    if is_overnight_hours(dt=dt):
        return OVERNIGHT_INTERVAL
    return OFFPEAK_INTERVAL


tracker_stopped = False


class DashboardHandler(BaseHTTPRequestHandler):
    # HTTP/1.1 enables keep-alive so the client can reuse one connection across
    # its periodic requests instead of waking the radio + rebuilding a TCP
    # connection for each one.
    protocol_version = "HTTP/1.1"
    # Reap idle keep-alive connections so they don't hold worker threads open
    # indefinitely (each connection is handled by its own thread).
    timeout = 30

    def _send_forbidden(self):
        msg = b"<h1>403 Forbidden</h1><p>Control endpoint requires a token or a private-network origin.</p>"
        self.send_response(403)
        self.send_header("Content-Type", "text/html")
        self.send_header("Content-Length", str(len(msg)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(msg)

    def _send_empty(self, code):
        # Content-Length is required to keep an HTTP/1.1 connection usable.
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def _write_body(self, data: bytes):
        """Writes a response body, suppressing it for HEAD requests so the
        HTTP/1.1 connection framing (Content-Length) stays valid."""
        if self.command != "HEAD":
            self.wfile.write(data)

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
            self._send_empty(200)
            return
        if parsed.path == "/diag":
            try:
                length = int(self.headers.get("Content-Length", 0))
                body = self.rfile.read(length).decode("utf-8", errors="replace")
                level, charging = parse_diag_battery(body)
                with _diag_lock:
                    _last_diagnostics["text"] = body
                    _last_diagnostics["time"] = time.time()
                    if level is not None:
                        _last_diagnostics["battery"] = level
                        _last_diagnostics["charging"] = charging
                extra = f" battery={level}%{'⚡' if charging else ''}" if level is not None else ""
                print(f"[Diagnostics] received {len(body)} bytes from {self.address_string()}{extra}")
            except Exception as e:
                print(f"[Server] Error reading diagnostics: {e}")
            self._send_empty(200)
            return
        self._send_empty(404)

    def do_GET(self):
        global tracker_stopped, _diag_requested, _mode_requested, _device_action
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
            self._write_body(payload)
            return

        if parsed.path == "/log":
            msg = params.get("msg", [""])[0]
            print(f"[Kindle Log] {msg}")
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return

        if parsed.path == "/diag":
            # Fetch the most recent device diagnostics report. Add ?request=1 to
            # ask the next polling client for a passive dump, or ?request=full for
            # one that also runs the active capability probe.
            req = params.get("request", ["0"])[0].lower()
            if req in ("1", "true", "yes", "full"):
                with _diag_lock:
                    _diag_requested = "full" if req == "full" else "1"
                print(f"[Diagnostics] requested a '{_diag_requested}' dump from the next poll")
            with _diag_lock:
                text = _last_diagnostics["text"]
                ts = _last_diagnostics["time"]
            if not text:
                self._send_empty(404)
                return
            payload = f"# diagnostics captured {time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(ts))}\n{text}".encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self._write_body(payload)
            return

        if parsed.path == "/mode":
            # Request that the next Kindle poll relaunch the client in a given
            # run mode (resident/oneshot/sleep). GET with no ?set= reports the
            # pending request.
            want = params.get("set", [""])[0].lower()
            if want in VALID_RUN_MODES:
                with _diag_lock:
                    _mode_requested = want
                print(f"[Mode] requested client mode '{want}' on the next poll")
            with _diag_lock:
                pending = _mode_requested
            payload = json.dumps({"pending": pending, "valid": list(VALID_RUN_MODES)}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self._write_body(payload)
            return

        if parsed.path == "/action":
            # Queue a named, allowlisted device action for the next Kindle poll.
            # GET with no ?do= reports the pending action. The action name is
            # validated on the client against its allowlist.
            global _device_action
            do = params.get("do", [""])[0].strip().lower()
            if do:
                with _diag_lock:
                    _device_action = do
                print(f"[Action] queued device action '{do}' for the next poll")
            with _diag_lock:
                pending = _device_action
            payload = json.dumps({"pending": pending}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self._write_body(payload)
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
            self._write_body(msg)
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
            self._write_body(msg)
            return

        if parsed.path == "/tracker-arm":
            exists, mtime, digest, size = get_binary_info()
            if not exists:
                self.send_response(404)
                self.end_headers()
                return

            last_mod = time.strftime("%a, %d %b %Y %H:%M:%S GMT", time.gmtime(mtime))
            print(f"[Server OTA] client={self.address_string()} cmd={self.command} version={SERVER_VERSION} ims={self.headers.get('If-Modified-Since')}")

            # Handle conditional request (If-Modified-Since)
            ims = self.headers.get("If-Modified-Since")
            if ims == last_mod:
                self.send_response(304)
                self.end_headers()
                return

            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(size))
            self.send_header("Last-Modified", last_mod)
            self.send_header("X-Tracker-Version", SERVER_VERSION)
            self.send_header("X-Tracker-Server", f"http://{get_local_ip()}:{PORT}")
            self.send_header("X-Tracker-SHA256", digest)
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()

            if self.command == "GET":
                try:
                    with open(BINARY_PATH, "rb") as f:
                        self._write_body(f.read())
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
            render_h = 480

            if is_kindle:
                # Render natively at the panel's true resolution. The client
                # reports its landscape framebuffer dimensions (?w=&h=); older
                # clients fall back to the PW5 default. All layout math stays in
                # the logical 800px design space and is scaled at draw time, so
                # glyphs are rasterized natively rather than upscaled.
                land_w, land_h = PW5_LANDSCAPE
                if "w" in params and "h" in params:
                    land_w, land_h = sanitize_kindle_panel(params["w"][0], params["h"][0])

                logical_w = WIDTH
                logical_h = max(1, int(round(logical_w * land_h / land_w)))
                scale = land_w / logical_w
                render_w, render_h = logical_w, logical_h

                img = get_fresh_dashboard_image(
                    use_mock=use_mock,
                    batt_level=batt_level,
                    is_charging=is_charging,
                    view=view_param,
                    width=render_w,
                    height=render_h,
                    scale=scale,
                )
                img = format_for_kindle(img, orientation="landscape", rotation=rot_val,
                                        target=(land_w, land_h))
            else:
                img = get_fresh_dashboard_image(
                    use_mock=use_mock,
                    batt_level=batt_level,
                    is_charging=is_charging,
                    view=view_param,
                    width=render_w,
                    height=render_h,
                )

            buf = io.BytesIO()
            img.save(buf, format="PNG")
            img_bytes = buf.getvalue()

            # Fingerprint the rendered image so an unchanged dashboard can be
            # answered with 304 Not Modified: the client then skips both the
            # ~90KB transfer and the eips refresh, saving radio + panel power.
            etag = '"%s"' % hashlib.sha256(img_bytes).hexdigest()[:16]

            brightness, warmth = get_commute_lighting()
            poll_interval = get_target_poll_interval()
            _exists, _mtime, bin_sha, _size = get_binary_info()

            # Consume pending one-shot requests so a *Kindle* client acts on them
            # this poll. Desktop/web requests for /dashboard.png must not consume
            # them. diag is "" (none), "1" (passive) or "full" (passive + active).
            diag_header = ""
            mode_header = ""
            # The client reports the mode it's actually running via
            # X-Tracker-Mode. If it matches the desired mode, the switch landed
            # and we stop requesting. If the client reports a *different* mode we
            # keep requesting (self-healing retry). A client that reports nothing
            # (older build) is treated as one-shot to avoid a relaunch loop.
            client_mode = self.headers.get("X-Tracker-Mode", "")
            action_header = ""
            if is_kindle:
                with _diag_lock:
                    diag_header = _diag_requested
                    _diag_requested = ""
                    action_header = _device_action
                    _device_action = ""
                    # Sticky desired mode: request it whenever the client isn't
                    # already in it (covers fresh OTA restarts). Don't clear on
                    # confirmation -- it must outlive the client process. A
                    # client that reports nothing (legacy) gets it once.
                    if _mode_requested and client_mode != _mode_requested:
                        mode_header = _mode_requested

            if self.headers.get("If-None-Match") == etag:
                self.send_response(304)
                self.send_header("ETag", etag)
                self.send_header("X-Tracker-Version", SERVER_VERSION)
                self.send_header("X-Tracker-SHA256", bin_sha)
                self.send_header("X-Kindle-Poll-Interval", str(poll_interval))
                self.send_header("X-Tracker-Server", f"http://{get_local_ip()}:{PORT}")
                self.send_header("X-Resolved-View", resolve_view(view_param))
                if diag_header:
                    self.send_header("X-Tracker-Diag", diag_header)
                if mode_header:
                    self.send_header("X-Tracker-Mode", mode_header)
                if action_header:
                    self.send_header("X-Tracker-Action", action_header)
                self.end_headers()
                return

            self.send_response(200)
            self.send_header("Content-Type", "image/png")
            self.send_header("Content-Length", str(len(img_bytes)))
            self.send_header("ETag", etag)
            self.send_header("X-Kindle-Brightness", str(brightness))
            self.send_header("X-Kindle-Warmth", str(warmth))
            self.send_header("X-Kindle-Poll-Interval", str(poll_interval))
            self.send_header("X-Tracker-Server", f"http://{get_local_ip()}:{PORT}")
            self.send_header("X-Tracker-Version", SERVER_VERSION)
            self.send_header("X-Tracker-SHA256", bin_sha)
            self.send_header("X-Tracker-View", view_param)
            self.send_header("X-Resolved-View", resolve_view(view_param))
            if diag_header:
                self.send_header("X-Tracker-Diag", diag_header)
            if mode_header:
                self.send_header("X-Tracker-Mode", mode_header)
            if action_header:
                self.send_header("X-Tracker-Action", action_header)
            self.send_header("Cache-Control", "no-cache, no-store, must-revalidate")
            self.end_headers()
            self._write_body(img_bytes)

        elif parsed.path in ["/", "/index.html"]:
            current_view = params.get("view", ["auto"])[0]
            token_qs = f"?token={urllib.parse.quote(CONTROL_TOKEN)}" if CONTROL_TOKEN else ""
            status_badge = '<span style="color:#ff6b6b;">STOPPED</span>' if tracker_stopped else '<span style="color:#51cf66;">ACTIVE</span>'
            if tracker_stopped:
                toggle_link = f'<a href="/resume{token_qs}" style="color:#51cf66;">Resume Tracker</a>'
            else:
                toggle_link = f'<a href="/stop{token_qs}" style="color:#ff6b6b;">Stop Kindle Tracker</a>'

            # Kindle battery, as last reported by the device diagnostics dump.
            with _diag_lock:
                batt_level = _last_diagnostics["battery"]
                batt_charging = _last_diagnostics["charging"]
            if batt_level is not None:
                bolt = "⚡ " if batt_charging else ""
                batt_html = f' | Kindle: {bolt}<strong>{batt_level}%</strong>'
            else:
                batt_html = " | Kindle: <em>no report</em>"
            diag_link = f'<a href="/diag">diagnostics</a> | <a href="/diag?request=full">request full dump</a>'
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
    <div class="status">Status: {status_badge} | {toggle_link}{batt_html}</div>
    <div class="links">{diag_link}</div>
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
            self._write_body(html.encode("utf-8"))

        else:
            self._send_empty(404)

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
