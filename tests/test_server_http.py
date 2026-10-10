"""
Integration tests that exercise the real HTTP handlers over a loopback socket.
The server is bound to an ephemeral port; the dashboard route is served from
mock data so no external network calls are made.
"""

import copy
import datetime
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
from unittest.mock import patch, MagicMock

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
        super().setUp()
        with server._diag_lock:
            server._last_diagnostics["text"] = ""
            server._last_diagnostics["time"] = 0.0
            server._diag_requested = ""

    def tearDown(self):
        with server._diag_lock:
            server._last_diagnostics["text"] = ""
            server._diag_requested = ""
        super().tearDown()

    def test_get_diag_404_before_upload(self):
        status, _headers, _body = _http_get(self.port, "/diag")
        self.assertEqual(status, 404)

    def test_parse_diag_battery(self):
        cases = [
            ("battery_level=83 charging=true", 83, True),
            ("battery_level=42 charging=false", 42, False),
            ("no battery here", None, None),
            ("battery_level=-1 charging=false", None, None),
        ]
        for text, exp_level, exp_charging in cases:
            with self.subTest(text=text):
                res = server.parse_diag_battery(text)
                self.assertEqual(res, (exp_level, exp_charging))
                self.assertEqual(res.level, exp_level)
                self.assertEqual(res.charging, exp_charging)

    def test_post_diag_extracts_and_exposes_battery(self):
        body = b"--- Kindle Diagnostics ---\nbattery_level=87 charging=true\nfw=5.16.21\n"
        status, _headers, _body = _http(method="POST", port=self.port, path="/diag", body=body)
        self.assertEqual(status, 200)
        self.assertEqual(server._last_diagnostics["battery"], 87)
        self.assertEqual(server._last_diagnostics["charging"], True)

    def test_post_then_get_diag(self):
        body = b"sample diagnostics payload"
        _http(method="POST", port=self.port, path="/diag", body=body)
        status, headers, text = _http_get(self.port, "/diag")
        self.assertEqual(status, 200)
        self.assertEqual(headers.get("Content-Type"), "text/plain; charset=utf-8")
        self.assertIn(b"sample diagnostics payload", text)

    def test_diag_request_flagged_and_consumed_by_dashboard(self):
        status, _headers, _body = _http_get(self.port, "/diag?request=1")
        self.assertEqual(status, 404)
        self.assertEqual(server._diag_requested, "1")

        status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertEqual(status, 200)
        self.assertEqual(headers.get("X-Tracker-Diag"), "1")
        self.assertEqual(server._diag_requested, "")

        status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertNotIn("X-Tracker-Diag", headers)

    def test_web_request_does_not_consume_diag_flag(self):
        _http_get(self.port, "/diag?request=1")
        self.assertEqual(server._diag_requested, "1")

        status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1")
        self.assertEqual(status, 200)
        self.assertNotIn("X-Tracker-Diag", headers)
        self.assertEqual(server._diag_requested, "1")

    def test_diag_full_request_forwarded(self):
        _http_get(self.port, "/diag?request=full")
        self.assertEqual(server._diag_requested, "full")
        _status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertEqual(headers.get("X-Tracker-Diag"), "full")
        self.assertEqual(server._diag_requested, "")

    def test_diag_flag_also_sent_on_304(self):
        _status, headers, _body = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        etag = headers.get("ETag")
        self.assertTrue(etag)

        _http_get(self.port, "/diag?request=1")
        self.assertEqual(server._diag_requested, "1")

        status, headers, _body = _http_get(
            self.port, "/dashboard.png?mock=1&kindle=pw5", headers={"If-None-Match": etag}
        )
        self.assertEqual(status, 304)
        self.assertEqual(headers.get("X-Tracker-Diag"), "1")
        self.assertEqual(server._diag_requested, "")


class TestRunModeEndpoint(ServerHTTPTestBase):
    def setUp(self):
        super().setUp()
        with server._diag_lock:
            server._mode_requested = ""

    def tearDown(self):
        with server._diag_lock:
            server._mode_requested = ""
        super().tearDown()

    def test_mode_get_reports_valid_and_pending(self):
        status, headers, body = _http_get(self.port, "/mode")
        self.assertEqual(status, 200)
        self.assertEqual(headers.get("Content-Type"), "application/json")
        data = json.loads(body)
        self.assertEqual(data["pending"], "")
        self.assertIn("resident", data["valid"])
        self.assertIn("oneshot", data["valid"])
        self.assertIn("sleep", data["valid"])
        self.assertIn("sleep-suspend", data["valid"])

    def test_mode_set_and_forwarded_on_kindle_poll(self):
        _http_get(self.port, "/mode?set=sleep")
        self.assertEqual(server._mode_requested, "sleep")

        _status, headers_web, _ = _http_get(self.port, "/dashboard.png?mock=1")
        self.assertNotIn("X-Tracker-Mode", headers_web)

        _status, headers_k, _ = _http_get(
            self.port,
            "/dashboard.png?mock=1&kindle=pw5",
            headers={"X-Tracker-Mode": "oneshot"},
        )
        self.assertEqual(headers_k.get("X-Tracker-Mode"), "sleep")

    def test_mode_is_sticky_across_client_restarts(self):
        _http_get(self.port, "/mode?set=sleep")

        _s, h1, _ = _http_get(
            self.port,
            "/dashboard.png?mock=1&kindle=pw5",
            headers={"X-Tracker-Mode": "oneshot"},
        )
        self.assertEqual(h1.get("X-Tracker-Mode"), "sleep")

        _s, h2, _ = _http_get(
            self.port,
            "/dashboard.png?mock=1&kindle=pw5",
            headers={"X-Tracker-Mode": "sleep"},
        )
        self.assertNotIn("X-Tracker-Mode", h2)

        _s, h3, _ = _http_get(
            self.port,
            "/dashboard.png?mock=1&kindle=pw5",
            headers={"X-Tracker-Mode": "oneshot"},
        )
        self.assertEqual(h3.get("X-Tracker-Mode"), "sleep")
        self.assertEqual(server._mode_requested, "sleep")

    def test_mode_invalid_value_ignored(self):
        status, _headers, body = _http_get(self.port, "/mode?set=bogus")
        self.assertEqual(status, 200)
        data = json.loads(body)
        self.assertEqual(data["pending"], "")

    def test_mode_accepts_sleep_suspend(self):
        _http_get(self.port, "/mode?set=sleep-suspend")
        self.assertEqual(server._mode_requested, "sleep-suspend")

    def test_action_queued_and_forwarded_to_kindle(self):
        with server._diag_lock:
            server._device_action = ""

        status, _headers, body = _http_get(self.port, "/action?do=disable-ads")
        self.assertEqual(status, 200)
        self.assertEqual(server._device_action, "disable-ads")

        _s, hw, _ = _http_get(self.port, "/dashboard.png?mock=1")
        self.assertNotIn("X-Tracker-Action", hw)

        _s, hk, _ = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertEqual(hk.get("X-Tracker-Action"), "disable-ads")
        self.assertEqual(server._device_action, "")

        _s, hk2, _ = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertNotIn("X-Tracker-Action", hk2)

    def test_mode_forwarded_on_304(self):
        _s, h, _ = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        etag = h.get("ETag")

        _http_get(self.port, "/mode?set=oneshot")

        s304, h304, _ = _http_get(
            self.port,
            "/dashboard.png?mock=1&kindle=pw5",
            headers={"If-None-Match": etag, "X-Tracker-Mode": "sleep"},
        )
        self.assertEqual(s304, 304)
        self.assertEqual(h304.get("X-Tracker-Mode"), "oneshot")


class TestKeepAlive(ServerHTTPTestBase):
    def test_multiple_requests_on_one_connection(self):
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(5)
        s.connect(("127.0.0.1", self.port))
        try:
            for _ in range(3):
                req = b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: keep-alive\r\n\r\n"
                s.sendall(req)

                buf = b""
                while b"\r\n\r\n" not in buf:
                    chunk = s.recv(1024)
                    self.assertTrue(chunk, "connection closed prematurely by server")
                    buf += chunk
                header_part, rest = buf.split(b"\r\n\r\n", 1)

                content_length = 0
                for line in header_part.decode().split("\r\n"):
                    if line.lower().startswith("content-length:"):
                        content_length = int(line.split(":", 1)[1].strip())

                while len(rest) < content_length:
                    rest += s.recv(1024)

                payload = json.loads(rest[:content_length].decode())
                self.assertEqual(payload["status"], "ok")
        finally:
            s.close()

    def test_head_has_no_body_and_keeps_connection_usable(self):
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.settimeout(5)
        s.connect(("127.0.0.1", self.port))
        try:
            req = b"HEAD /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: keep-alive\r\n\r\n"
            s.sendall(req)
            buf = b""
            while b"\r\n\r\n" not in buf:
                chunk = s.recv(1024)
                self.assertTrue(chunk)
                buf += chunk
            header_part, rest = buf.split(b"\r\n\r\n", 1)
            self.assertEqual(rest, b"")
            self.assertIn(b"200 OK", header_part)

            req2 = b"GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"
            s.sendall(req2)
            buf2 = b""
            while True:
                chunk = s.recv(1024)
                if not chunk:
                    break
                buf2 += chunk
            self.assertIn(b"200 OK", buf2)
            self.assertIn(b'"status": "ok"', buf2)
        finally:
            s.close()


class TestLogEndpoint(ServerHTTPTestBase):
    def test_post_log_returns_200(self):
        status, _headers, _body = _http(method="POST", port=self.port, path="/log", body=b"hello from kindle\n")
        self.assertEqual(status, 200)

    def test_post_unknown_path_404(self):
        status, _headers, _body = _http(method="POST", port=self.port, path="/nonexistent", body=b"xyz")
        self.assertEqual(status, 404)

    def test_get_log_with_query(self):
        status, _headers, _body = _http_get(self.port, "/log?msg=test-message")
        self.assertEqual(status, 200)


class TestControlEndpoints(ServerHTTPTestBase):
    def setUp(self):
        super().setUp()
        server.tracker_stopped = False
        server.CONTROL_TOKEN = ""

    def tearDown(self):
        server.tracker_stopped = False
        server.CONTROL_TOKEN = ""
        super().tearDown()

    def test_stop_then_resume_from_loopback(self):
        status, _headers, body = _http_get(self.port, "/stop")
        self.assertEqual(status, 200)
        self.assertTrue(server.tracker_stopped)
        self.assertIn(b"Stopping", body)

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


class TestForbiddenResponse(unittest.TestCase):
    def test_send_forbidden_renders_403(self):
        class FakeHandler:
            command = "GET"
            sent_headers = {}

            def send_response(self, code):
                self.code = code

            def send_header(self, k, v):
                self.sent_headers[k] = v

            def end_headers(self):
                pass

            class Out:
                def __init__(self):
                    self.data = b""

                def write(self, b):
                    self.data += b

            def __init__(self):
                self.wfile = self.Out()

        h = FakeHandler()
        DashboardHandler._send_forbidden(h)
        self.assertEqual(h.code, 403)
        self.assertEqual(h.sent_headers["Content-Type"], "text/html")
        self.assertIn(b"403 Forbidden", h.wfile.data)


class TestServerCoverageAdditions(ServerHTTPTestBase):
    def test_warm_up_gtfs(self):
        mock_gtfs = MagicMock()
        orig = server.gtfs_tracker
        try:
            server.gtfs_tracker = mock_gtfs
            server.warm_up_gtfs()
            mock_gtfs.ensure_index.assert_called_once()

            mock_gtfs.ensure_index.side_effect = Exception("warmup failed")
            server.warm_up_gtfs()
        finally:
            server.gtfs_tracker = orig

    def test_get_fresh_dashboard_image_cached(self):
        img1 = server.get_fresh_dashboard_image(use_mock=True, width=800, height=480)
        img2 = server.get_fresh_dashboard_image(use_mock=True, width=800, height=480)
        self.assertIsNotNone(img1)
        self.assertIsNotNone(img2)

    def test_parse_hour_env_invalid(self):
        with patch.dict("os.environ", {"TEST_INVALID_HOUR": "not-a-float"}):
            self.assertEqual(server._parse_hour_env("TEST_INVALID_HOUR", 8.5), 8.5)

    def test_is_overnight_hours_ordered(self):
        orig_start = server.OVERNIGHT_START
        orig_end = server.OVERNIGHT_END
        try:
            server.OVERNIGHT_START = 1.0
            server.OVERNIGHT_END = 5.0
            dt_in = datetime.datetime(2026, 10, 10, 3, 0)
            dt_out = datetime.datetime(2026, 10, 10, 6, 0)
            self.assertTrue(server.is_overnight_hours(dt_in))
            self.assertFalse(server.is_overnight_hours(dt_out))
        finally:
            server.OVERNIGHT_START = orig_start
            server.OVERNIGHT_END = orig_end

    def test_get_presentation_force_fast_poll(self):
        orig = server.FORCE_FAST_POLL
        try:
            server.FORCE_FAST_POLL = True
            self.assertEqual(server.get_presentation(), "interactive")
        finally:
            server.FORCE_FAST_POLL = orig

    def test_action_endpoint_without_do(self):
        status, _headers, body = _http_get(self.port, "/action")
        self.assertEqual(status, 200)
        data = json.loads(body)
        self.assertIn("pending", data)

    def test_tracker_arm_head_request(self):
        cand1 = os.path.join(os.path.dirname(server.__file__), "tracker-arm")
        cand2 = os.path.join(os.path.dirname(server.__file__), "..", "tracker-arm")
        binary = cand1 if os.path.exists(cand1) else (cand2 if os.path.exists(cand2) else cand1)
        created = False
        if not os.path.exists(binary):
            with open(binary, "wb") as f:
                f.write(b"FAKEARM" * 100)
            created = True
        try:
            status, headers, body = _http("HEAD", self.port, "/tracker-arm")
            self.assertEqual(status, 200)
            self.assertEqual(len(body), 0)
            self.assertIn("X-Tracker-Version", headers)
        finally:
            if created and os.path.exists(binary):
                os.remove(binary)

    def test_dashboard_304_with_action_header(self):
        status, headers, _ = _http_get(self.port, "/dashboard.png?mock=1&kindle=pw5")
        self.assertEqual(status, 200)
        etag = headers.get("ETag")
        self.assertIsNotNone(etag)

        with server._diag_lock:
            server._device_action = "disable-ads"
        try:
            status_304, headers_304, _ = _http_get(
                self.port, "/dashboard.png?mock=1&kindle=pw5", headers={"If-None-Match": etag}
            )
            self.assertEqual(status_304, 304)
            self.assertEqual(headers_304.get("X-Tracker-Action"), "disable-ads")
        finally:
            with server._diag_lock:
                server._device_action = ""

    def test_send_forbidden_on_head(self):
        with patch("server.check_control_auth", return_value=False):
            status, _headers, body = _http("HEAD", self.port, "/stop")
            self.assertEqual(status, 403)
            self.assertEqual(len(body), 0)

    def test_post_log_and_diag_read_errors(self):
        handler = DashboardHandler.__new__(DashboardHandler)
        handler.command = "POST"
        handler.path = "/log"
        handler.headers = {"Content-Length": "10"}
        mock_rfile = MagicMock()
        mock_rfile.read.side_effect = IOError("socket closed")
        handler.rfile = mock_rfile
        handler._send_empty = MagicMock()

        handler.do_POST()
        handler._send_empty.assert_called_with(200)

        handler.path = "/diag"
        handler._send_empty.reset_mock()
        handler.do_POST()
        handler._send_empty.assert_called_with(200)

    def test_get_fresh_data_live_gtfs_exception_fallback(self):
        mock_gtfs = MagicMock()
        mock_gtfs.get_upcoming.side_effect = Exception("GTFS failure")
        mock_njt = MagicMock()
        mock_njt.get_arrivals_with_status.return_value = ("ok", [])
        mock_cb = MagicMock()
        mock_cb.get_station_status.return_value = [
            {"name": "Mock Station", "ebikes": 2, "classic": 1, "docks": 5, "walk_min": 2, "is_offline": False}
        ]
        mock_cb.get_mock_data.return_value = []
        orig_gtfs = server.gtfs_tracker
        orig_njt = server.tracker
        orig_cb = server.cb_tracker

        with server._data_lock:
            orig_cache = copy.deepcopy(server._data_cache)

        orig_connect = socket.socket.connect

        def hermetic_connect(sock, address):
            if sock.type == socket.SOCK_STREAM and address[0] not in ("127.0.0.1", "localhost", "::1"):
                raise AssertionError(f"Hermetic isolation violation: outbound WAN connection attempted to {address}")
            return orig_connect(sock, address)

        try:
            server.gtfs_tracker = mock_gtfs
            server.tracker = mock_njt
            server.cb_tracker = mock_cb
            with server._data_lock:
                server._data_cache["time"] = 0
            with patch.object(socket.socket, "connect", hermetic_connect):
                stops, status, cb_data = server.get_fresh_data(use_mock=False)
            mock_njt.get_arrivals_with_status.assert_called()
            mock_cb.get_station_status.assert_called_once()
            self.assertEqual(cb_data, mock_cb.get_station_status.return_value)
        finally:
            server.gtfs_tracker = orig_gtfs
            server.tracker = orig_njt
            server.cb_tracker = orig_cb
            with server._data_lock:
                server._data_cache.clear()
                server._data_cache.update(orig_cache)


if __name__ == "__main__":
    unittest.main()
