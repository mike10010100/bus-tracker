import base64
import hashlib
import hmac
import html
import io
import json
import os
import re
import secrets
import socket
import sys
import threading
import time
import urllib.parse
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict, List, NamedTuple, Optional, Tuple, Union
from PIL import Image

try:
    from zeroconf import Zeroconf, ServiceInfo
except ImportError:
    Zeroconf = None
    ServiceInfo = None

from bus_tracker import NJTransitBusTracker, normalize_arrival, ArrivalRecord
from citibike import (
    CitiBikeTracker,
    StationConfig,
    StationStatus,
    CitiBikeSnapshot,
    CitiBikeUnavailable,
    CB_STATUS_OK,
    CB_STATUS_STALE,
    CB_STATUS_ERROR,
)
from gtfs_bus import GTFSBusTracker
from render_dashboard import render_dashboard, STOPS, get_mock_data, resolve_view, WIDTH
from version import VERSION

from discovery import (
    ZEROCONF_AVAILABLE,
    DISCOVERY_PORT,
    is_private_address,
    get_local_ip,
    start_discovery_responder,
    start_mdns_advertiser,
)
from ota import (
    BINARY_PATH,
    BINARY_NAME,
    MANIFEST_NAME,
    sha256_file,
    get_binary_info,
    get_valid_manifest,
    load_binary,
    BinaryInfo,
)
from kindle_image import (
    PW5_NATIVE,
    PW5_LANDSCAPE,
    native_render_scale,
    sanitize_kindle_panel,
    format_for_kindle,
)
from schedule import (
    _parse_hour_env,
    PEAK_AM_START,
    PEAK_AM_END,
    PEAK_PM_START,
    PEAK_PM_END,
    is_peak_commute_hours,
    get_commute_lighting,
    CommuteLighting,
    FORCE_FAST_POLL,
    OVERNIGHT_START,
    OVERNIGHT_END,
    OVERNIGHT_INTERVAL,
    OFFPEAK_INTERVAL,
    is_overnight_hours,
    get_presentation,
    get_status_note,
    get_dormant_note,
    get_target_poll_interval,
)
from paths import resolve_cache_dir
from logsafe import redact, strip_controls, safe_log_lines
from identity import (
    load_identity,
    NONCE_HEADER,
    CERT_HEADER,
    AUTH_HEADER,
    is_valid_nonce,
)

PORT = int(os.environ.get("PORT", 8000))
INTERACTIVE_TTL = 30
SERVER_VERSION = VERSION

# Control token management (security spec §9)
def init_control_token() -> str:
    env_tok = os.environ.get("TRACKER_CONTROL_TOKEN", "").strip()
    if env_tok:
        return env_tok
    cache_dir = resolve_cache_dir()
    try:
        os.makedirs(cache_dir, exist_ok=True)
    except OSError:
        pass
    token_file = os.path.join(cache_dir, "control_token")
    if os.path.exists(token_file):
        try:
            with open(token_file, "r", encoding="utf-8") as f:
                tok = f.read().strip()
                if tok:
                    return tok
        except OSError:
            pass
    tok = secrets.token_hex(16)
    try:
        fd = os.open(token_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(tok)
    except OSError:
        pass
    return tok

CONTROL_TOKEN = init_control_token()
_identity, _identity_status = load_identity()

tracker = None
gtfs_tracker = None
cb_tracker = CitiBikeTracker(cache_ttl=30)


def check_control_auth(handler) -> bool:
    """
    Authorizes a control request (security spec §9).
    Requires matching X-Tracker-Token header. No query-param or private-IP bypass.
    """
    supplied = handler.headers.get("X-Tracker-Token", "").strip()
    return bool(CONTROL_TOKEN) and hmac.compare_digest(supplied, CONTROL_TOKEN)


# Last device diagnostics report uploaded by a client
_diag_lock = threading.Lock()
_last_diagnostics = {"text": "", "time": 0.0, "battery": None, "charging": None}
_diag_requested = ""

VALID_RUN_MODES = ("resident", "oneshot", "sleep", "sleep-suspend")
_mode_requested = os.environ.get("TRACKER_DEFAULT_MODE", "").strip().lower()
if _mode_requested not in VALID_RUN_MODES:
    _mode_requested = ""

_device_action = ""


class BatteryDiag(NamedTuple):
    level: Optional[int]
    charging: Optional[bool]


def parse_diag_battery(text: str) -> BatteryDiag:
    """
    Extracts (level, charging) from a diagnostics report's machine-readable
    'battery_level=<n> charging=<bool>' line. Returns (None, None) if absent.
    """
    m = re.search(r"battery_level=(-?\d+)\s+charging=(true|false)", text)
    if not m:
        return BatteryDiag(None, None)
    level = int(m.group(1))
    if level < 0:
        return BatteryDiag(None, None)
    return BatteryDiag(level, m.group(2) == "true")


# Upstream data cache (bus arrivals + Citi Bike status), decoupled from render.
_data_lock = threading.Lock()
_data_fetch_lock = threading.Lock()
_data_cache = {"time": 0.0, "stops": None, "status": {}, "cb": None}
_render_cache: Dict[Any, bytes] = {}
_render_lock = threading.Lock()


def data_cache_ttl(interactive: bool = False) -> int:
    if interactive:
        return INTERACTIVE_TTL
    return get_target_poll_interval()


def get_fresh_data(use_mock: bool = False, interactive: bool = False) -> Tuple[Dict[str, Any], Dict[str, str], Any]:
    """
    Returns (stops_data, stop_status, cb_data), refreshing upstream sources at
    most once per data_cache_ttl(). Shared by all render requests.
    Mock data requests never poison the shared cache.
    """
    if use_mock:
        return (
            get_mock_data(),
            {stop["id"]: NJTransitBusTracker.STATUS_OK for stop in STOPS},
            cb_tracker.get_mock_data(),
        )

    now = time.time()
    with _data_lock:
        fresh = _data_cache["stops"] is not None and (now - _data_cache["time"] < data_cache_ttl(interactive))
        if fresh:
            return _data_cache["stops"], _data_cache["status"], _data_cache["cb"]

    # Single-flight upstream fetch so concurrent misses wait for a single fetch
    with _data_fetch_lock:
        now = time.time()
        with _data_lock:
            fresh = _data_cache["stops"] is not None and (now - _data_cache["time"] < data_cache_ttl(interactive))
            if fresh:
                return _data_cache["stops"], _data_cache["status"], _data_cache["cb"]

        stops_data = {}
        stop_status = {}
        global tracker, gtfs_tracker
        if tracker is None:
            tracker = NJTransitBusTracker()
        if gtfs_tracker is None:
            gtfs_tracker = GTFSBusTracker(route="126", stops=[stop["id"] for stop in STOPS])

        allow_realtime = not is_overnight_hours()
        for stop in STOPS:
            sid = stop["id"]
            arrivals = None
            try:
                arrivals = gtfs_tracker.get_upcoming(sid, limit=3, allow_realtime=allow_realtime)
            except Exception as e:
                print(f"[Server] GTFS-BUS fetch error ({redact(e)}); falling back to public API.")
            if arrivals:
                stops_data[sid] = arrivals
                stop_status[sid] = NJTransitBusTracker.STATUS_OK
            else:
                status, trips = tracker.get_arrivals_with_status(stop_id=sid, route="126")
                stops_data[sid] = [normalize_arrival(t) for t in trips]
                stop_status[sid] = status

        try:
            snap = cb_tracker.get_snapshot()
            if snap.status == CB_STATUS_ERROR:
                cb_data = []
            else:
                cb_data = snap.stations
        except Exception as e:
            print(f"[Server] Citi Bike fetch error ({redact(e)})")
            cb_data = []

        with _data_lock:
            _data_cache["time"] = now
            _data_cache["stops"] = stops_data
            _data_cache["status"] = stop_status
            _data_cache["cb"] = cb_data

        return stops_data, stop_status, cb_data


def warm_up_gtfs() -> None:
    """Builds the GTFS static index ahead of the first client render."""
    global gtfs_tracker
    try:
        if gtfs_tracker is None:
            gtfs_tracker = GTFSBusTracker(route="126", stops=[stop["id"] for stop in STOPS])
        gtfs_tracker.ensure_index(wait=True)
    except Exception as e:
        print(f"[GTFS] warm-up failed ({redact(e)})")


def get_fresh_dashboard_image(
    use_mock: bool = False,
    batt_level: Optional[int] = None,
    is_charging: bool = False,
    view: str = "auto",
    width: int = 800,
    height: int = 480,
    scale: float = 1.0,
    presentation: str = "interactive",
    status_note: str = "",
    interactive: bool = False,
) -> Image.Image:
    stops_data, stop_status, cb_data = get_fresh_data(use_mock=use_mock, interactive=interactive)

    cache_key = (
        use_mock,
        view,
        width,
        height,
        scale,
        batt_level,
        is_charging,
        presentation,
        status_note,
        _data_cache["time"] if not use_mock else 0,
    )
    with _render_lock:
        cached = _render_cache.get(cache_key)
        if cached is not None:
            return Image.open(io.BytesIO(cached))

    buf = io.BytesIO()
    render_dashboard(
        stops_data,
        citibike_data=cb_data,
        output_path=buf,
        view=view,
        is_mock=use_mock,
        batt_level=batt_level,
        is_charging=is_charging,
        stop_status=stop_status,
        width=width,
        height=height,
        scale=scale,
        presentation=presentation,
        status_note=status_note,
    )

    img_bytes = buf.getvalue()
    with _render_lock:
        if len(_render_cache) > 16:
            _render_cache.clear()
        _render_cache[cache_key] = img_bytes

    return Image.open(io.BytesIO(img_bytes))


tracker_stopped = False


class DashboardHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    timeout = 30

    def send_header(self, keyword: str, value: Any) -> None:
        str_val = str(value)
        if "\r" in str_val or "\n" in str_val:
            raise ValueError(f"CR/LF detected in header {keyword}: {str_val!r}")
        try:
            str_val.encode("latin-1")
        except UnicodeEncodeError as e:
            raise ValueError(f"Non-latin1 character in header {keyword}: {str_val!r}") from e
        super().send_header(keyword, str_val)

    def _send_forbidden(self) -> None:
        msg = b"<h1>403 Forbidden</h1><p>Control endpoint requires a valid X-Tracker-Token header.</p>"
        self.send_response(403)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(msg)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(msg)

    def _send_empty(self, code: int) -> None:
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def _send_tracker_headers(self, status: int, headers: List[Tuple[str, str]]) -> None:
        self.send_response(status)
        for name, value in headers:
            self.send_header(name, value)
        self.end_headers()

    def _write_body(self, data: bytes) -> None:
        if self.command != "HEAD":
            try:
                self.wfile.write(data)
            except (BrokenPipeError, ConnectionResetError, OSError):
                pass

    def _read_body(self, max_bytes: int = 65536) -> Optional[bytes]:
        try:
            length_hdr = self.headers.get("Content-Length")
            if length_hdr is None:
                self._send_empty(411)  # Length Required
                return None
            length = int(length_hdr)
            if length < 0:
                self._send_empty(400)
                return None
            if length > max_bytes:
                self._send_empty(413)  # Payload Too Large
                return None
            return self.rfile.read(length)
        except (ValueError, OSError):
            self._send_empty(400)
            return None

    def do_HEAD(self):
        self.do_GET()

    def do_POST(self):
        parsed = urllib.parse.urlparse(self.path)

        if parsed.path == "/stop":
            if not check_control_auth(self):
                self._send_forbidden()
                return
            global tracker_stopped
            tracker_stopped = True
            payload = json.dumps({"status": "stopped"}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self._write_body(payload)
            return

        if parsed.path in ["/resume", "/start"]:
            if not check_control_auth(self):
                self._send_forbidden()
                return
            tracker_stopped = False
            payload = json.dumps({"status": "resumed"}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self._write_body(payload)
            return

        if parsed.path == "/mode":
            if not check_control_auth(self):
                self._send_forbidden()
                return
            body = self._read_body(4096)
            if body is None:
                return
            want = ""
            try:
                data = json.loads(body.decode("utf-8"))
                if isinstance(data, dict):
                    want = str(data.get("set", "")).lower().strip()
            except Exception:
                qs = urllib.parse.parse_qs(body.decode("utf-8", errors="ignore"))
                want = qs.get("set", [""])[0].lower().strip()
            global _mode_requested
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
            if not check_control_auth(self):
                self._send_forbidden()
                return
            body = self._read_body(4096)
            if body is None:
                return
            do = ""
            try:
                data = json.loads(body.decode("utf-8"))
                if isinstance(data, dict):
                    do = str(data.get("do", "")).strip().lower()
            except Exception:
                qs = urllib.parse.parse_qs(body.decode("utf-8", errors="ignore"))
                do = qs.get("do", [""])[0].strip().lower()
            global _device_action
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

        if parsed.path == "/diag/request":
            if not check_control_auth(self):
                self._send_forbidden()
                return
            body = self._read_body(4096)
            if body is None:
                return
            level = "1"
            try:
                data = json.loads(body.decode("utf-8"))
                if isinstance(data, dict):
                    level = str(data.get("level", "1")).strip().lower()
            except Exception:
                qs = urllib.parse.parse_qs(body.decode("utf-8", errors="ignore"))
                level = qs.get("level", ["1"])[0].strip().lower()
            req = "full" if level == "full" else "1"
            global _diag_requested
            with _diag_lock:
                _diag_requested = req
            print(f"[Diagnostics] requested a '{req}' dump from the next poll")
            payload = json.dumps({"requested": req}).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self._write_body(payload)
            return

        if parsed.path == "/log":
            body = self._read_body(65536)
            if body is None:
                return
            text = body.decode("utf-8", errors="replace")
            for line in safe_log_lines(text):
                print(f"[Kindle Log] {line}")
            self._send_empty(200)
            return

        if parsed.path == "/diag":
            body = self._read_body(65536)
            if body is None:
                return
            text = body.decode("utf-8", errors="replace")
            level, charging = parse_diag_battery(text)
            with _diag_lock:
                _last_diagnostics["text"] = text
                _last_diagnostics["time"] = time.time()
                if level is not None:
                    _last_diagnostics["battery"] = level
                    _last_diagnostics["charging"] = charging
            extra = f" battery={level}%{'⚡' if charging else ''}" if level is not None else ""
            print(f"[Diagnostics] received {len(body)} bytes from {self.address_string()}{extra}")
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

        if parsed.path == "/identity":
            nonce = self.headers.get(NONCE_HEADER, "").strip()
            payload = json.dumps({
                "service": "transit-tracker",
                "version": SERVER_VERSION,
            }).encode("utf-8")
            headers = [
                ("Content-Type", "application/json"),
                ("Content-Length", str(len(payload))),
                ("Cache-Control", "no-cache"),
            ]
            if is_valid_nonce(nonce) and _identity is not None:
                auth_hdrs = _identity.sign_response(
                    nonce=nonce,
                    path="/identity",
                    status=200,
                    body=payload,
                    headers={"content-type": "application/json", "content-length": str(len(payload))},
                )
                headers.extend(auth_hdrs)
            self._send_tracker_headers(200, headers)
            self._write_body(payload)
            return

        if parsed.path == "/tracker-arm.manifest":
            info = get_valid_manifest()
            if info is None:
                self._send_empty(404)
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(info.raw)))
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            self._write_body(info.raw)
            return

        if parsed.path == "/diag":
            if not check_control_auth(self):
                self._send_forbidden()
                return
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
            safe_text = "\n".join(safe_log_lines(text, max_lines=1000))
            payload = f"# diagnostics captured {time.strftime('%Y-%m-%d %H:%M:%S', time.localtime(ts))}\n{safe_text}".encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self._write_body(payload)
            return

        if parsed.path == "/mode":
            if not check_control_auth(self):
                self._send_forbidden()
                return
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
            if not check_control_auth(self):
                self._send_forbidden()
                return
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

        if parsed.path in ["/stop", "/resume", "/start"]:
            # State-changing endpoints are POST-only
            self.send_response(405)
            self.send_header("Allow", "POST")
            self.send_header("Content-Length", "0")
            self.end_headers()
            return

        if parsed.path == "/tracker-arm":
            blob = load_binary()
            if blob is None:
                self._send_empty(404)
                return

            last_mod = time.strftime("%a, %d %b %Y %H:%M:%S GMT", time.gmtime(blob.mtime))
            ims = self.headers.get("If-Modified-Since")
            if ims == last_mod:
                self._send_empty(304)
                return

            try:
                local_ip = self.connection.getsockname()[0]
            except Exception:
                local_ip = get_local_ip()

            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(blob.size))
            self.send_header("Last-Modified", last_mod)
            self.send_header("X-Tracker-Version", SERVER_VERSION)
            self.send_header("X-Tracker-Server", f"http://{local_ip}:{PORT}")
            self.send_header("X-Tracker-SHA256", blob.sha256)
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()

            if self.command == "GET":
                self._write_body(blob.data)
            return

        if parsed.path in ["/dashboard.png", "/bus.png"]:
            nonce = self.headers.get(NONCE_HEADER, "").strip()

            if tracker_stopped:
                resp_headers = [("Content-Length", "0")]
                if is_valid_nonce(nonce) and _identity is not None:
                    auth_hdrs = _identity.sign_response(
                        nonce=nonce,
                        path=parsed.path,
                        status=205,
                        body=b"",
                        headers={"content-length": "0"},
                    )
                    resp_headers.extend(auth_hdrs)
                self.send_response(205)
                for k, v in resp_headers:
                    self.send_header(k, v)
                self.end_headers()
                return

            use_mock = "mock" in params
            kindle_mode = params.get("kindle", [None])[0]

            rot_str = params.get("rotate", ["90"])[0]
            try:
                rot_val = int(rot_str)
                if rot_val not in (0, 90, 180, 270):
                    raise ValueError()
            except ValueError:
                self._send_empty(400)
                return

            raw_view = params.get("view", [self.headers.get("X-Tracker-View", "auto")])[0].lower().strip()
            if raw_view in ("morning", "citi", "citibike", "am"):
                view_param = "morning"
            elif raw_view in ("evening", "bus", "pm", "afternoon", "night"):
                view_param = "evening"
            else:
                view_param = "auto"

            batt_param = params.get("batt", [None])[0] or params.get("battery", [None])[0] or self.headers.get("X-Kindle-Battery")
            charging_param = params.get("charging", [None])[0] or self.headers.get("X-Kindle-Charging")

            batt_level = None
            if batt_param and str(batt_param).strip().lstrip("-").isdigit():
                val = int(batt_param)
                if 0 <= val <= 100:
                    batt_level = val

            is_charging = str(charging_param).lower() in ["1", "true", "yes"]
            is_kindle = kindle_mode == "pw5" or "kindle" in params
            interactive_override = params.get("present", [""])[0] == "interactive"
            presentation = "interactive" if (interactive_override or not is_kindle) else get_presentation()
            status_note = get_status_note(presentation)
            render_w = 800
            render_h = 480

            if is_kindle:
                land_w, land_h = PW5_LANDSCAPE
                if "w" in params and "h" in params:
                    land_w, land_h = sanitize_kindle_panel(params["w"][0], params["h"][0])

                logical_w = WIDTH
                logical_h = max(1, int(round(logical_w * land_h / land_w)))
                scale = native_render_scale(land_w, land_h, logical_w)
                render_w, render_h = logical_w, logical_h

                img = get_fresh_dashboard_image(
                    use_mock=use_mock,
                    batt_level=batt_level,
                    is_charging=is_charging,
                    view=view_param,
                    width=render_w,
                    height=render_h,
                    scale=scale,
                    presentation=presentation,
                    status_note=status_note,
                    interactive=interactive_override,
                )
                img = format_for_kindle(img, orientation="landscape", rotation=rot_val, target=(land_w, land_h))
            else:
                img = get_fresh_dashboard_image(
                    use_mock=use_mock,
                    batt_level=batt_level,
                    is_charging=is_charging,
                    view=view_param,
                    width=render_w,
                    height=render_h,
                    presentation=presentation,
                    status_note=status_note,
                )

            buf = io.BytesIO()
            img.save(buf, format="PNG")
            img_bytes = buf.getvalue()

            etag = '"%s"' % hashlib.sha256(img_bytes).hexdigest()[:16]
            brightness, warmth = get_commute_lighting()
            poll_interval = get_target_poll_interval()

            diag_header = ""
            mode_header = ""
            client_mode = self.headers.get("X-Tracker-Mode", "")
            action_header = ""
            if is_kindle:
                with _diag_lock:
                    diag_header = _diag_requested
                    _diag_requested = ""
                    action_header = _device_action
                    _device_action = ""
                    if _mode_requested and client_mode != _mode_requested:
                        mode_header = _mode_requested

            try:
                local_ip = self.connection.getsockname()[0]
            except Exception:
                local_ip = get_local_ip()

            common_headers: List[Tuple[str, str]] = [
                ("ETag", etag),
                ("X-Kindle-Poll-Interval", str(poll_interval)),
                ("X-Tracker-Presentation", presentation),
                ("X-Tracker-Server", f"http://{local_ip}:{PORT}"),
                ("X-Resolved-View", resolve_view(view_param)),
                ("X-Tracker-View", view_param),
            ]

            valid_m = get_valid_manifest()
            if valid_m is not None:
                common_headers.append(("X-Tracker-Version", valid_m.version))
                common_headers.append(("X-Tracker-SHA256", valid_m.sha256))

            if diag_header:
                common_headers.append(("X-Tracker-Diag", diag_header))
            if mode_header:
                common_headers.append(("X-Tracker-Mode", mode_header))
            if action_header:
                common_headers.append(("X-Tracker-Action", action_header))

            # Not Modified check (304)
            if self.headers.get("If-None-Match") == etag:
                resp_304_headers = list(common_headers)
                resp_304_headers.append(("Content-Length", "0"))
                if is_valid_nonce(nonce) and _identity is not None:
                    hdr_map = {k.lower(): v for k, v in resp_304_headers}
                    auth_hdrs = _identity.sign_response(
                        nonce=nonce,
                        path=parsed.path,
                        status=304,
                        body=b"",
                        headers=hdr_map,
                    )
                    resp_304_headers.extend(auth_hdrs)
                self._send_tracker_headers(304, resp_304_headers)
                return

            full_headers = [
                ("Content-Type", "image/png"),
                ("Content-Length", str(len(img_bytes))),
            ] + common_headers + [
                ("X-Kindle-Brightness", str(brightness)),
                ("X-Kindle-Warmth", str(warmth)),
                ("Cache-Control", "no-cache, no-store, must-revalidate"),
            ]

            if is_valid_nonce(nonce) and _identity is not None:
                hdr_map = {k.lower(): v for k, v in full_headers}
                auth_hdrs = _identity.sign_response(
                    nonce=nonce,
                    path=parsed.path,
                    status=200,
                    body=img_bytes,
                    headers=hdr_map,
                )
                full_headers.extend(auth_hdrs)

            self._send_tracker_headers(200, full_headers)
            self._write_body(img_bytes)

        elif parsed.path in ["/", "/index.html"]:
            current_view = html.escape(params.get("view", ["auto"])[0])
            status_badge = '<span style="color:#ff6b6b;">STOPPED</span>' if tracker_stopped else '<span style="color:#51cf66;">ACTIVE</span>'

            with _diag_lock:
                batt_level = _last_diagnostics["battery"]
                batt_charging = _last_diagnostics["charging"]
            if batt_level is not None:
                bolt = "⚡ " if batt_charging else ""
                batt_html = f' | Kindle: {bolt}<strong>{int(batt_level)}%</strong>'
            else:
                batt_html = " | Kindle: <em>no report</em>"

            script_nonce = secrets.token_hex(16)
            csp = (
                "default-src 'self'; "
                "img-src 'self' data:; "
                f"script-src 'self' 'nonce-{script_nonce}'; "
                "style-src 'unsafe-inline'"
            )

            html_content = f"""<!DOCTYPE html>
<html>
<head>
    <title>NJ Transit 126 &amp; Citi Bike Tracker</title>
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
        .controls {{
            margin-top: 14px;
            display: flex;
            gap: 8px;
            align-items: center;
        }}
        button {{
            background: #444;
            color: #fff;
            border: 1px solid #666;
            padding: 6px 12px;
            border-radius: 4px;
            cursor: pointer;
        }}
        button:hover {{ background: #555; }}
        input[type="password"] {{
            background: #333;
            color: #fff;
            border: 1px solid #555;
            padding: 6px;
            border-radius: 4px;
        }}
        a {{ color: #4da6ff; text-decoration: none; margin: 0 8px; }}
        a:hover {{ text-decoration: underline; }}
    </style>
</head>
<body>
    <img src="/dashboard.png?view={current_view}&amp;t={int(time.time())}" alt="Transit Dashboard" />
    <div class="status">Status: {status_badge}{batt_html}</div>
    <div class="controls">
        <input type="password" id="tokenInput" placeholder="Control Token" />
        <button id="saveTokenBtn">Save Token</button>
        <button id="toggleBtn">{"Resume Tracker" if tracker_stopped else "Stop Kindle Tracker"}</button>
    </div>
    <div class="links">
        <a href="/diag" id="diagLink">View Diagnostics</a>
    </div>
    <div class="links">
        <strong>View Mode:</strong>
        <a href="/?view=auto">Auto (AM Citi / PM Bus)</a> |
        <a href="/?view=morning">Morning (Citi Bike Hero)</a> |
        <a href="/?view=evening">Evening (Bus Hero)</a>
    </div>
    <div class="links">
        <a href="/dashboard.png?view={current_view}" target="_blank">Standard (800x480)</a> |
        <a href="/dashboard.png?kindle=pw5&amp;rotate=90&amp;view={current_view}" target="_blank">Kindle PW5 (Rotated 90°)</a> |
        <a href="/dashboard.png?mock=1&amp;view={current_view}" target="_blank">Mock Preview</a>
    </div>
    <script nonce="{script_nonce}">
        const tokenInput = document.getElementById("tokenInput");
        const savedToken = localStorage.getItem("tracker_token") || "";
        tokenInput.value = savedToken;
        document.getElementById("saveTokenBtn").onclick = () => {{
            localStorage.setItem("tracker_token", tokenInput.value.trim());
            alert("Token saved in browser.");
        }};
        document.getElementById("toggleBtn").onclick = async () => {{
            const tok = tokenInput.value.trim();
            const action = "{'resume' if tracker_stopped else 'stop'}";
            const res = await fetch("/" + action, {{
                method: "POST",
                headers: {{ "X-Tracker-Token": tok }}
            }});
            if (res.ok) {{
                window.location.reload();
            }} else {{
                alert("Action failed: HTTP " + res.status);
            }}
        }};
        document.getElementById("diagLink").onclick = async (e) => {{
            e.preventDefault();
            const tok = tokenInput.value.trim();
            const res = await fetch("/diag", {{
                headers: {{ "X-Tracker-Token": tok }}
            }});
            if (res.ok) {{
                const text = await res.text();
                const w = window.open();
                w.document.open();
                w.document.write("<pre>" + text.replace(/&/g,"&amp;").replace(/</g,"&lt;") + "</pre>");
                w.document.close();
            }} else {{
                alert("Diagnostics access denied: HTTP " + res.status);
            }}
        }};
    </script>
</body>
</html>"""
            payload = html_content.encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Security-Policy", csp)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self._write_body(payload)

        else:
            self._send_empty(404)

    def log_message(self, format, *args):
        print(f"[Server] {self.address_string()} - {args[0]}")


if __name__ == "__main__":
    server_address = ("", PORT)
    httpd = ThreadingHTTPServer(server_address, DashboardHandler)
    local_ip = get_local_ip()
    print("==================================================")
    print(f"  Hoboken Transit Tracker Server v{SERVER_VERSION} on Port {PORT}")
    print(f"  Local View:      http://localhost:{PORT}")
    print(f"  Kindle Endpoint: http://{local_ip}:{PORT}/dashboard.png?kindle=pw5")
    print(f"  Auto-Discovery:  UDP Port {DISCOVERY_PORT} & mDNS (_transittracker._tcp.local)")
    print(f"  Identity status: {_identity_status}")
    print(f"  Control token:   {CONTROL_TOKEN}")
    print("==================================================")

    start_discovery_responder(http_port=PORT, version=SERVER_VERSION)
    zc, mdns_info = start_mdns_advertiser(http_port=PORT, version=SERVER_VERSION)

    threading.Thread(target=warm_up_gtfs, daemon=True).start()

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
