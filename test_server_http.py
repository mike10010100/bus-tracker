"""
Integration tests that exercise the real HTTP handlers over a loopback socket.
The server is bound to an ephemeral port; the dashboard route is served from
mock data so no external network calls are made.
"""

import io
import json
import os
import socket
import tempfile
import threading
import time
import unittest
import urllib.request
import urllib.error
from http.server import ThreadingHTTPServer

import server
from server import DashboardHandler, format_for_kindle, sha256_file
from PIL import Image


def _http_get(port, path, headers=None):
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, dict(resp.headers), resp.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()


def _http(method, port, path, headers=None, body=None):
    req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", data=body, headers=headers or {}, method=method)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, dict(resp.headers), resp.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()


class ServerHTTPTestBase(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.httpd = ThreadingHTTPServer(("127.0.0.1", 0), DashboardHandler)
        cls.port = cls.httpd.server_address[1]
        cls.thread = threading.Thread(target=cls.httpd.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.httpd.shutdown()
        cls.httpd.server_close()
        cls.thread.join(timeout=5)


class TestHealthAndRoot(ServerHTTPTestBase):
    def test_healthz(self):
        status, _headers, body = _http_get(self.port, "/healthz")
        self.assertEqual(status, 200)
        payload = json.loads(body)
        self.assertEqual(payload["status"], "ok")
        self.assertEqual(payload["version"], server.SERVER_VERSION)
        self.assertIn("stopped", payload)

    def test_root_html(self):
        status, headers, body = _http_get(self.port, "/")
        self.assertEqual(status, 200)
        self.assertIn("text/html", headers.get("Content-Type", ""))
        self.assertIn(b"Dashboard", body)

    def test_root_html_with_token_shows_token_links(self):
        original = server.CONTROL_TOKEN
        server.CONTROL_TOKEN = "s3cret"
        try:
            status, _headers, body = _http_get(self.port, "/")
            self.assertEqual(status, 200)
            self.assertIn(b"token=s3cret", body)
        finally:
            server.CONTROL_TOKEN = original

    def test_unknown_path_404(self):
        status, _headers, _body = _http_get(self.port, "/does-not-exist")
        self.assertEqual(status, 404)

    def test_head_request(self):
        status, _headers, _body = _http(method="HEAD", port=self.port, path="/healthz")
        self.assertEqual(status, 200)


class TestDiagnosticsEndpoints(ServerHTTPTestBase):
    def setUp(self):
        with server._diag_lock:
            server._last_diagnostics["text"] = ""
            server._last_diagnostics["time"] = 0.0
            server._diag_requested = ""

    def tearDown(self):
        with server._diag_lock:
            server._last_diagnostics["text"] = ""
            server._diag_requested = ""

    def test_get_diag_404_before_upload(self):
        status, _headers, _body = _http_get(self.port, "/diag")
        self.assertEqual(status, 404)

    def test_parse_diag_battery(self):
        self.assertEqual(server.parse_diag_battery("battery_level=83 charging=true"), (83, True))
        self.assertEqual(server.parse_diag_battery("battery_level=42 charging=false"), (42, False))
        self.assertEqual(server.parse_diag_battery("no battery here"), (None, None))
        self.assertEqual(server.parse_diag_battery("battery_level=-1 charging=false"), (None, None))

    def test_post_diag_extracts_and_exposes_battery(self):
        report = "=== DIAGNOSTICS ===\nbattery: 77%\nbattery_level=77 charging=true\n=== END DIAGNOSTICS ==="
        _http("POST", self.port, "/diag", body=report.encode("utf-8"))
        self.assertEqual(server._last_diagnostics["battery"], 77)
        self.assertEqual(server._last_diagnostics["charging"], True)

        status, _headers, body = _http_get(self.port, "/")
        self.assertEqual(status, 200)
        self.assertIn(b"77%", body)

    def test_post_then_get_diag(self):
        report = "=== DIAGNOSTICS v1.0.0 ===\nhas_kron: true\n=== END DIAGNOSTICS ==="
        status, _headers, _body = _http("POST", self.port, "/diag", body=report.encode("utf-8"))
        self.assertEqual(status, 200)

        status, headers, body = _http_get(self.port, "/diag")
        self.assertEqual(status, 200)
        self.assertIn("text/plain", headers.get("Content-Type", ""))
        self.assertIn(b"has_kron: true", body)
        self.assertIn(b"diagnostics captured", body)

    def test_diag_request_flagged_and_consumed_by_dashboard(self):
        # Asking for a dump arms a one-shot flag.
        status, _headers, _body = _http_get(self.port, "/diag?request=1")
        self.assertEqual(status, 404)  # nothing stored yet, but the flag is set
        self.assertEqual(server._diag_requested, "1")

        # The next Kindle poll advertises it and clears the flag.
        status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertEqual(status, 200)
        self.assertEqual(headers.get("X-Tracker-Diag"), "1")
        self.assertEqual(server._diag_requested, "")

        # A subsequent poll does not re-request.
        _status, headers2, _body2 = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertIsNone(headers2.get("X-Tracker-Diag"))

    def test_web_request_does_not_consume_diag_flag(self):
        # A desktop/web /dashboard.png (no kindle param) must NOT consume the
        # one-shot diag request; only a Kindle poll should.
        _http_get(self.port, "/diag?request=full")
        self.assertEqual(server._diag_requested, "full")

        status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1")
        self.assertEqual(status, 200)
        self.assertIsNone(headers.get("X-Tracker-Diag"))
        self.assertEqual(server._diag_requested, "full")  # still pending

        # The Kindle poll then consumes and forwards it.
        status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertEqual(headers.get("X-Tracker-Diag"), "full")
        self.assertEqual(server._diag_requested, "")

    def test_diag_full_request_forwarded(self):
        status, _headers, _body = _http_get(self.port, "/diag?request=full")
        self.assertEqual(status, 404)  # nothing stored yet
        self.assertEqual(server._diag_requested, "full")
        _status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertEqual(headers.get("X-Tracker-Diag"), "full")
        self.assertEqual(server._diag_requested, "")

    def test_diag_flag_also_sent_on_304(self):
        # Establish an ETag on a Kindle request.
        _status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        etag = headers.get("ETag")
        _http_get(self.port, "/diag?request=1")
        status, headers2, _body2 = _http_get(
            self.port, "/dashboard.png?mock=1&kindle=pw5", headers={"If-None-Match": etag}
        )
        self.assertEqual(status, 304)
        self.assertEqual(headers2.get("X-Tracker-Diag"), "1")


class TestRunModeEndpoint(ServerHTTPTestBase):
    def setUp(self):
        with server._diag_lock:
            server._mode_requested = ""

    def tearDown(self):
        with server._diag_lock:
            server._mode_requested = ""

    def test_mode_get_reports_valid_and_pending(self):
        status, _headers, body = _http_get(self.port, "/mode")
        self.assertEqual(status, 200)
        payload = json.loads(body)
        self.assertEqual(payload["pending"], "")
        self.assertIn("sleep", payload["valid"])

    def test_mode_set_and_forwarded_on_kindle_poll(self):
        status, _headers, body = _http_get(self.port, "/mode?set=sleep")
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["pending"], "sleep")

        # A web request must not consume it.
        _status, wheaders, _body = _http_get(self.port, "/dashboard.png?mock=1")
        self.assertIsNone(wheaders.get("X-Tracker-Mode"))
        self.assertEqual(server._mode_requested, "sleep")

        # A legacy Kindle poll (no reported mode) forwards it once, then stops.
        status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertEqual(headers.get("X-Tracker-Mode"), "sleep")
        self.assertEqual(server._mode_requested, "")

    def test_mode_persists_until_client_confirms(self):
        # If the client reports a mode different from the desired one, the server
        # keeps requesting until it reports the target.
        _http_get(self.port, "/mode?set=sleep")
        _status, headers, _body = _http_get(
            self.port, "/dashboard.png?mock=1&kindle=pw5", headers={"X-Tracker-Mode": "resident"}
        )
        self.assertEqual(headers.get("X-Tracker-Mode"), "sleep")
        self.assertEqual(server._mode_requested, "sleep")  # still pending

        # Once the client reports the target mode, the request clears.
        _status, headers, _body = _http_get(
            self.port, "/dashboard.png?mock=1&kindle=pw5", headers={"X-Tracker-Mode": "sleep"}
        )
        self.assertEqual(server._mode_requested, "")
        self.assertIsNone(headers.get("X-Tracker-Mode"))

    def test_mode_invalid_value_ignored(self):
        _http_get(self.port, "/mode?set=bogus")
        self.assertEqual(server._mode_requested, "")

    def test_mode_accepts_sleep_suspend(self):
        _http_get(self.port, "/mode?set=sleep-suspend")
        self.assertEqual(server._mode_requested, "sleep-suspend")

    def test_action_queued_and_forwarded_to_kindle(self):
        with server._diag_lock:
            server._device_action = ""
        status, _headers, body = _http_get(self.port, "/action?do=disable-ads")
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["pending"], "disable-ads")
        self.assertEqual(server._device_action, "disable-ads")

        # Web request must not consume it; Kindle poll forwards and clears.
        _status, wheaders, _body = _http_get(self.port, "/dashboard.png?mock=1")
        self.assertIsNone(wheaders.get("X-Tracker-Action"))
        status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertEqual(headers.get("X-Tracker-Action"), "disable-ads")
        self.assertEqual(server._device_action, "")

    def test_mode_forwarded_on_304(self):
        _status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        etag = headers.get("ETag")
        _http_get(self.port, "/mode?set=oneshot")
        status, headers2, _body2 = _http_get(
            self.port, "/dashboard.png?mock=1&kindle=pw5",
            headers={"If-None-Match": etag, "X-Tracker-Mode": "resident"},
        )
        self.assertEqual(status, 304)
        self.assertEqual(headers2.get("X-Tracker-Mode"), "oneshot")


class TestKeepAlive(ServerHTTPTestBase):
    def test_multiple_requests_on_one_connection(self):
        # HTTP/1.1 keep-alive: the client can reuse a connection across polls
        # rather than waking the radio to rebuild TCP each time.
        import http.client
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        try:
            for _ in range(3):
                conn.request("GET", "/healthz")
                resp = conn.getresponse()
                self.assertEqual(resp.status, 200)
                self.assertTrue(resp.read())
        finally:
            conn.close()

    def test_head_has_no_body_and_keeps_connection_usable(self):
        import http.client
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        try:
            conn.request("HEAD", "/dashboard.png?mock=1")
            resp = conn.getresponse()
            self.assertEqual(resp.status, 200)
            self.assertEqual(resp.read(), b"")
            self.assertIsNotNone(resp.getheader("Content-Length"))
            # Connection still usable afterwards.
            conn.request("GET", "/healthz")
            self.assertEqual(conn.getresponse().status, 200)
        finally:
            conn.close()


class TestLogEndpoint(ServerHTTPTestBase):
    def test_post_log_returns_200(self):
        status, _headers, _body = _http(method="POST", port=self.port, path="/log", body=b"hello from kindle")
        self.assertEqual(status, 200)

    def test_post_unknown_path_404(self):
        status, _headers, _body = _http(method="POST", port=self.port, path="/nope", body=b"x")
        self.assertEqual(status, 404)

    def test_get_log_with_query(self):
        status, _headers, _body = _http_get(self.port, "/log?msg=test-message")
        self.assertEqual(status, 200)


class TestDashboardRoute(ServerHTTPTestBase):
    def test_mock_dashboard_standard(self):
        status, headers, body = _http_get(self.port, "/dashboard.png?mock=1")
        self.assertEqual(status, 200)
        self.assertEqual(headers.get("Content-Type"), "image/png")
        self.assertIn("X-Kindle-Poll-Interval", headers)
        img = Image.open(io.BytesIO(body))
        self.assertEqual(img.size, (800, 480))

    def test_mock_dashboard_kindle_rotation(self):
        status, headers, body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5&rotate=90")
        self.assertEqual(status, 200)
        self.assertEqual(headers.get("X-Resolved-View") in ("morning", "evening"), True)
        img = Image.open(io.BytesIO(body))
        self.assertEqual(img.size, (1236, 1648))
        self.assertEqual(img.mode, "L")

    def test_kindle_custom_panel_dimensions_reported_natively(self):
        # Client reports its landscape panel size; server must render natively
        # at that size (no bitmap upscaling) then rotate to portrait.
        status, _headers, body = _http_get(
            self.port, "/dashboard.png?mock=1&kindle=pw5&w=1648&h=1236&rotate=90"
        )
        self.assertEqual(status, 200)
        img = Image.open(io.BytesIO(body))
        # Portrait rotation of a 1648x1236 landscape canvas.
        self.assertEqual(img.size, (1236, 1648))
        self.assertEqual(img.mode, "L")

    def test_kindle_smaller_panel(self):
        # A hypothetical smaller panel (e.g. 1072x1448 portrait -> 1448x1072).
        status, _headers, body = _http_get(
            self.port, "/dashboard.png?mock=1&kindle=pw5&w=1448&h=1072&rotate=90"
        )
        self.assertEqual(status, 200)
        img = Image.open(io.BytesIO(body))
        self.assertEqual(img.size, (1072, 1448))

    def test_kindle_bad_dimensions_fall_back(self):
        # Malformed w/h must not crash; falls back to the PW5 default.
        status, _headers, body = _http_get(
            self.port, "/dashboard.png?mock=1&kindle=pw5&w=abc&h=xyz&rotate=90"
        )
        self.assertEqual(status, 200)
        img = Image.open(io.BytesIO(body))
        self.assertEqual(img.size, (1236, 1648))

    def test_kindle_buffer_sized_dimensions_rejected(self):
        # A misreporting client may send the double-buffered backing size
        # (3296x1248 for a 1648x1236 panel); it must be sanitized to the default.
        status, _headers, body = _http_get(
            self.port, "/dashboard.png?mock=1&kindle=pw5&w=3296&h=1248&rotate=90"
        )
        self.assertEqual(status, 200)
        img = Image.open(io.BytesIO(body))
        self.assertEqual(img.size, (1236, 1648))
        self.assertEqual(server.sanitize_kindle_panel(3296, 1248), server.PW5_LANDSCAPE)

    def test_dashboard_carries_version_and_sha_headers(self):
        # The dashboard response must advertise the server version and the OTA
        # binary digest so the client can make the OTA decision without a
        # separate per-cycle HEAD request.
        binary = os.path.join(os.path.dirname(server.__file__), "tracker-arm")
        existed = os.path.exists(binary)
        if not existed:
            with open(binary, "wb") as f:
                f.write(b"FAKEARM")
        try:
            status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
            self.assertEqual(status, 200)
            self.assertEqual(headers.get("X-Tracker-Version"), server.SERVER_VERSION)
            self.assertEqual(headers.get("X-Tracker-SHA256"), server.sha256_file(binary))
        finally:
            if not existed:
                os.remove(binary)

    def test_dashboard_etag_and_304(self):
        status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1")
        self.assertEqual(status, 200)
        etag = headers.get("ETag")
        self.assertTrue(etag)

        # Conditional request with the same ETag must return 304 with no body.
        status2, headers2, body2 = _http_get(
            self.port, "/dashboard.png?mock=1", headers={"If-None-Match": etag}
        )
        self.assertEqual(status2, 304)
        self.assertEqual(body2, b"")
        self.assertEqual(headers2.get("ETag"), etag)
        self.assertIn("X-Kindle-Poll-Interval", headers2)

    def test_battery_clamped(self):
        # Out-of-range battery must be ignored (rendered without battery), not crash
        status, _headers, body = _http_get(self.port, "/dashboard.png?mock=1&batt=999")
        self.assertEqual(status, 200)
        self.assertTrue(len(body) > 0)

    def test_battery_valid_in_range_accepted(self):
        status, _headers, body = _http_get(self.port, "/dashboard.png?mock=1&batt=77&charging=1")
        self.assertEqual(status, 200)
        self.assertTrue(len(body) > 0)

    def test_view_via_header(self):
        status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1", headers={"X-Tracker-View": "evening"})
        self.assertEqual(status, 200)
        self.assertEqual(headers.get("X-Tracker-View"), "evening")

    def test_bus_png_alias(self):
        status, _headers, _body = _http_get(self.port, "/bus.png?mock=1")
        self.assertEqual(status, 200)


class TestControlEndpoints(ServerHTTPTestBase):
    def tearDown(self):
        server.tracker_stopped = False
        server.CONTROL_TOKEN = ""

    def test_stop_then_resume_from_loopback(self):
        status, _headers, body = _http_get(self.port, "/stop")
        self.assertEqual(status, 200)
        self.assertTrue(server.tracker_stopped)
        self.assertIn(b"Stopping", body)

        # While stopped, the dashboard route signals HTTP 205 to the client
        status, _headers, _body = _http_get(self.port, "/dashboard.png?mock=1")
        self.assertEqual(status, 205)

        status, _headers, _body = _http_get(self.port, "/resume")
        self.assertEqual(status, 200)
        self.assertFalse(server.tracker_stopped)

    def test_start_alias_resumes(self):
        server.tracker_stopped = True
        status, _headers, _body = _http_get(self.port, "/start")
        self.assertEqual(status, 200)
        self.assertFalse(server.tracker_stopped)

    def test_stop_denied_with_token_configured_and_wrong_token(self):
        server.CONTROL_TOKEN = "topsecret"
        status, _headers, body = _http_get(self.port, "/stop")
        self.assertEqual(status, 403)
        self.assertFalse(server.tracker_stopped)
        self.assertIn(b"Forbidden", body)

    def test_stop_allowed_with_correct_token(self):
        server.CONTROL_TOKEN = "topsecret"
        status, _headers, _body = _http_get(self.port, "/stop?token=topsecret")
        self.assertEqual(status, 200)
        self.assertTrue(server.tracker_stopped)

    def test_stop_allowed_with_correct_header_token(self):
        server.CONTROL_TOKEN = "topsecret"
        status, _headers, _body = _http_get(self.port, "/stop", headers={"X-Tracker-Token": "topsecret"})
        self.assertEqual(status, 200)
        self.assertTrue(server.tracker_stopped)

    def test_resume_denied_without_token(self):
        server.tracker_stopped = True
        server.CONTROL_TOKEN = "topsecret"
        status, _headers, _body = _http_get(self.port, "/resume")
        self.assertEqual(status, 403)
        self.assertTrue(server.tracker_stopped)

    def test_token_links_present_when_stopped_and_configured(self):
        server.CONTROL_TOKEN = "topsecret"
        server.tracker_stopped = True
        status, _headers, body = _http_get(self.port, "/")
        self.assertEqual(status, 200)
        self.assertIn(b"resume?token=topsecret", body)


class TestTrackerArmRoute(ServerHTTPTestBase):
    @classmethod
    def setUpClass(cls):
        super().setUpClass()
        cls.binary = os.path.join(os.path.dirname(server.__file__), "tracker-arm")
        cls._pre_existing = os.path.exists(cls.binary)
        if not cls._pre_existing:
            with open(cls.binary, "wb") as f:
                f.write(b"FAKEARM" * 100)

    @classmethod
    def tearDownClass(cls):
        if not cls._pre_existing and os.path.exists(cls.binary):
            os.remove(cls.binary)
        super().tearDownClass()

    def test_get_binary_headers(self):
        status, headers, body = _http_get(self.port, "/tracker-arm")
        self.assertEqual(status, 200)
        self.assertEqual(headers.get("Content-Type"), "application/octet-stream")
        self.assertEqual(headers.get("X-Tracker-Version"), server.SERVER_VERSION)
        self.assertEqual(headers.get("X-Tracker-SHA256"), sha256_file(self.binary))
        self.assertEqual(len(body), os.path.getsize(self.binary))
        with open(self.binary, "rb") as f:
            self.assertEqual(body, f.read())

    def test_missing_binary_returns_404(self):
        os.rename(self.binary, self.binary + ".bak")
        try:
            status, _headers, _body = _http_get(self.port, "/tracker-arm")
            self.assertEqual(status, 404)
        finally:
            os.rename(self.binary + ".bak", self.binary)

    def test_conditional_request_304(self):
        _status, headers, _body = _http_get(self.port, "/tracker-arm")
        last_mod = headers.get("Last-Modified")
        status, _headers, _body = _http_get(self.port, "/tracker-arm", headers={"If-Modified-Since": last_mod})
        self.assertEqual(status, 304)


class TestDiscoveryAndLighting(unittest.TestCase):
    def test_sha256_file_matches_hashlib(self):
        import hashlib
        with tempfile.NamedTemporaryFile(delete=False) as f:
            f.write(b"payload-bytes")
            path = f.name
        try:
            self.assertEqual(sha256_file(path), hashlib.sha256(b"payload-bytes").hexdigest())
        finally:
            os.remove(path)

    def test_get_local_ip_returns_string(self):
        ip = server.get_local_ip()
        self.assertIsInstance(ip, str)
        self.assertTrue(len(ip) > 0)

    def test_peak_commute_boundaries(self):
        from datetime import datetime
        self.assertTrue(server.is_peak_commute_hours(datetime(2026, 1, 1, 7, 30)))
        self.assertFalse(server.is_peak_commute_hours(datetime(2026, 1, 1, 9, 30)))
        self.assertTrue(server.is_peak_commute_hours(datetime(2026, 1, 1, 16, 30)))
        self.assertFalse(server.is_peak_commute_hours(datetime(2026, 1, 1, 19, 0)))

    def test_lighting_and_poll_interval_pair(self):
        from datetime import datetime
        peak = datetime(2026, 1, 1, 8, 0)
        off = datetime(2026, 1, 1, 13, 0)
        self.assertEqual(server.get_commute_lighting(peak), (8, 12))
        self.assertEqual(server.get_target_poll_interval(peak), 60)
        self.assertEqual(server.get_commute_lighting(off), (0, 0))
        self.assertEqual(server.get_target_poll_interval(off), 600)


class TestMdnsAdvertiser(unittest.TestCase):
    def test_returns_none_when_zeroconf_unavailable(self):
        original = server.ZEROCONF_AVAILABLE
        server.ZEROCONF_AVAILABLE = False
        try:
            zc, info = server.start_mdns_advertiser()
            self.assertIsNone(zc)
            self.assertIsNone(info)
        finally:
            server.ZEROCONF_AVAILABLE = original

    def test_registers_service_when_available(self):
        original_flag = server.ZEROCONF_AVAILABLE
        original_zc = getattr(server, "Zeroconf", None)
        original_info = getattr(server, "ServiceInfo", None)
        registered = {}

        class FakeZC:
            def register_service(self, info):
                registered["info"] = info

            def unregister_service(self, info):
                registered["unregistered"] = info

            def close(self):
                registered["closed"] = True

        server.ZEROCONF_AVAILABLE = True
        server.Zeroconf = FakeZC
        server.ServiceInfo = lambda *a, **k: {"args": a, "kwargs": k}
        try:
            zc, info = server.start_mdns_advertiser(http_port=8000, version="1.2.3")
            self.assertIsInstance(zc, FakeZC)
            self.assertIsNotNone(info)
            self.assertIn("info", registered)
            self.assertEqual(registered["info"]["kwargs"]["server"], "transittracker.local.")
        finally:
            server.ZEROCONF_AVAILABLE = original_flag
            if original_zc is None:
                del server.Zeroconf
            else:
                server.Zeroconf = original_zc
            if original_info is None:
                del server.ServiceInfo
            else:
                server.ServiceInfo = original_info


class TestForbiddenResponse(unittest.TestCase):
    def test_send_forbidden_renders_403(self):
        captured = {}

        class FakeHandler(DashboardHandler):
            command = "GET"

            def __init__(self):
                pass

            def send_response(self, code):
                captured["code"] = code

            def send_header(self, k, v):
                pass

            def end_headers(self):
                pass

            @property
            def wfile(self):
                return io.BytesIO()

        FakeHandler()._send_forbidden()
        self.assertEqual(captured["code"], 403)


class TestDiscoveryResponderFailure(unittest.TestCase):
    def test_bind_failure_is_handled(self):
        import socket as _socket
        original = server.socket.socket

        class FakeSock:
            def setsockopt(self, *a, **k):
                pass

            def bind(self, *a, **k):
                raise OSError("permission denied")

        server.socket.socket = lambda *a, **k: FakeSock()
        try:
            t = server.start_discovery_responder(http_port=1, version="1")
            self.assertTrue(t.daemon)
            time.sleep(0.1)
        finally:
            server.socket.socket = original


class TestMdnsFailure(unittest.TestCase):
    def test_registration_exception_returns_none(self):
        original_flag = server.ZEROCONF_AVAILABLE
        original_zc = getattr(server, "Zeroconf", None)
        original_info = getattr(server, "ServiceInfo", None)
        server.ZEROCONF_AVAILABLE = True
        server.ServiceInfo = lambda *a, **k: object()

        def boom():
            raise RuntimeError("cannot bind mdns")

        server.Zeroconf = boom
        try:
            zc, info = server.start_mdns_advertiser()
            self.assertIsNone(zc)
            self.assertIsNone(info)
        finally:
            server.ZEROCONF_AVAILABLE = original_flag
            if original_zc is None:
                del server.Zeroconf
            else:
                server.Zeroconf = original_zc
            if original_info is None:
                del server.ServiceInfo
            else:
                server.ServiceInfo = original_info


class TestUDPDiscoveryResponder(unittest.TestCase):
    def test_responder_answers_probe(self):
        # Bind a responder on an ephemeral discovery port using a patched module constant.
        original_port = server.DISCOVERY_PORT
        # Grab a free UDP port.
        probe = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
        probe.close()

        server.DISCOVERY_PORT = port
        try:
            t = server.start_discovery_responder(http_port=server.PORT, version="9.9.9")
            self.assertTrue(t.daemon)
            time.sleep(0.2)

            client = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            client.settimeout(3)
            try:
                client.sendto(b"TRANSIT_TRACKER_DISCOVER\n", ("127.0.0.1", port))
                data, _addr = client.recvfrom(1024)
                self.assertIn(b"TRANSIT_TRACKER_OFFER", data)
                self.assertIn(b"9.9.9", data)
            finally:
                client.close()
        finally:
            server.DISCOVERY_PORT = original_port


class TestSanitizeKindlePanel(unittest.TestCase):
    def test_valid_panels_pass_through(self):
        self.assertEqual(server.sanitize_kindle_panel(1648, 1236), (1648, 1236))
        self.assertEqual(server.sanitize_kindle_panel(1448, 1072), (1448, 1072))
        self.assertEqual(server.sanitize_kindle_panel(800, 600), (800, 600))

    def test_buffer_sized_falls_back(self):
        self.assertEqual(server.sanitize_kindle_panel(3296, 1248), server.PW5_LANDSCAPE)

    def test_out_of_range_falls_back(self):
        self.assertEqual(server.sanitize_kindle_panel(9999, 9999), server.PW5_LANDSCAPE)
        self.assertEqual(server.sanitize_kindle_panel(100, 100), server.PW5_LANDSCAPE)
        self.assertEqual(server.sanitize_kindle_panel(1648, 9999), server.PW5_LANDSCAPE)

    def test_non_numeric_falls_back(self):
        self.assertEqual(server.sanitize_kindle_panel("abc", "xyz"), server.PW5_LANDSCAPE)
        self.assertEqual(server.sanitize_kindle_panel(None, None), server.PW5_LANDSCAPE)

    def test_bad_aspect_ratio_falls_back(self):
        # Very wide or very tall values are rejected.
        self.assertEqual(server.sanitize_kindle_panel(2000, 600), server.PW5_LANDSCAPE)


class TestFormatForKindle(unittest.TestCase):
    def test_landscape_rotation_and_grayscale(self):
        base = Image.new("RGB", (800, 480), "white")
        out = format_for_kindle(base, orientation="landscape", rotation=90)
        self.assertEqual(out.size, (1236, 1648))
        self.assertEqual(out.mode, "L")

    def test_non_landscape_is_grayscale(self):
        base = Image.new("RGB", (800, 480), "white")
        out = format_for_kindle(base, orientation="portrait")
        self.assertEqual(out.mode, "L")

    def test_native_size_is_not_resampled(self):
        # A base already at the landscape panel size must be rotated as-is,
        # not resized (preserving native pixel detail).
        base = Image.new("RGB", (1648, 1236), "white")
        out = format_for_kindle(base, orientation="landscape", rotation=90)
        self.assertEqual(out.size, (1236, 1648))

    def test_custom_target_size(self):
        # 1072x1448 portrait panel -> 1448x1072 landscape.
        base = Image.new("RGB", (1448, 1072), "white")
        out = format_for_kindle(base, orientation="landscape", rotation=90, target=(1448, 1072))
        self.assertEqual(out.size, (1072, 1448))

    def test_native_render_scale(self):
        self.assertAlmostEqual(server.native_render_scale(1648, 1236, 800), 2.06, places=2)
        self.assertAlmostEqual(server.native_render_scale(1448, 1072, 800), 1.81, places=2)


if __name__ == "__main__":
    unittest.main()
