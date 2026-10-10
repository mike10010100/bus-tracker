"""
Unit tests for server.py control auth, version wiring, error-state rendering,
and the data/render caching split.
"""

import unittest
from unittest.mock import patch, MagicMock

import server
from server import (
    check_control_auth,
    get_fresh_data,
    is_private_address,
    sha256_file,
)
from version import VERSION


class TestVersionWiring(unittest.TestCase):
    def test_server_version_matches_version_file(self):
        self.assertEqual(server.SERVER_VERSION, VERSION)
        self.assertRegex(VERSION, r"^\d+\.\d+\.\d+$")


class TestPrivateAddress(unittest.TestCase):
    def test_private_and_loopback(self):
        self.assertTrue(is_private_address("127.0.0.1"))
        self.assertTrue(is_private_address("192.168.1.5"))
        self.assertTrue(is_private_address("10.0.0.9"))
        self.assertTrue(is_private_address("172.16.0.1"))
        self.assertTrue(is_private_address("169.254.1.1"))

    def test_public_and_invalid(self):
        self.assertFalse(is_private_address("8.8.8.8"))
        self.assertFalse(is_private_address("not-an-ip"))

    def test_get_local_ip_falls_back_to_localhost_on_error(self):
        from unittest.mock import patch
        with patch("socket.socket", side_effect=OSError("no net")):
            self.assertEqual(server.get_local_ip(), "localhost")


class TestControlAuth(unittest.TestCase):
    def _handler(self, addr, headers=None):
        h = MagicMock()
        h.client_address = (addr, 12345)
        h.headers = headers or {}
        h.path = "/stop"
        return h

    def test_private_origin_allowed_without_token(self):
        with patch.object(server, "CONTROL_TOKEN", ""):
            self.assertTrue(check_control_auth(self._handler("192.168.1.10")))

    def test_public_origin_denied_without_token(self):
        with patch.object(server, "CONTROL_TOKEN", ""):
            self.assertFalse(check_control_auth(self._handler("8.8.8.8")))

    def test_token_required_when_configured(self):
        with patch.object(server, "CONTROL_TOKEN", "secret"):
            self.assertFalse(check_control_auth(self._handler("192.168.1.10")))
            self.assertTrue(check_control_auth(self._handler("192.168.1.10", {"X-Tracker-Token": "secret"})))
            self.assertFalse(check_control_auth(self._handler("192.168.1.10", {"X-Tracker-Token": "wrong"})))


class TestErrorStateRendering(unittest.TestCase):
    def test_empty_state_message_error_vs_empty(self):
        from render_dashboard import empty_state_message, STATUS_ERROR, STATUS_EMPTY, STATUS_OK

        err_msg, err_color = empty_state_message(STATUS_ERROR)
        empty_msg, empty_color = empty_state_message(STATUS_EMPTY)
        self.assertIn("unavailable", err_msg.lower())
        self.assertNotEqual(err_msg, empty_msg)
        self.assertNotEqual(err_color, empty_color)
        self.assertEqual(empty_state_message(STATUS_OK), empty_state_message(STATUS_EMPTY))

    def test_render_with_error_status(self):
        from render_dashboard import render_dashboard, get_mock_data, STATUS_ERROR
        from citibike import CitiBikeTracker
        import os

        out = "/tmp/test_error_view.png"
        render_dashboard(
            {"20512": [], "20494": []},
            citibike_data=CitiBikeTracker().get_mock_data(),
            output_path=out,
            view="evening",
            stop_status={"20512": STATUS_ERROR, "20494": STATUS_ERROR},
        )
        self.assertTrue(os.path.exists(out))
        os.remove(out)


class TestDataCache(unittest.TestCase):
    def setUp(self):
        # Reset module-level caches between tests.
        with server._data_lock:
            server._data_cache.update({"time": 0.0, "stops": None, "status": {}, "cb": None})
        with server._render_lock:
            server._render_cache.clear()

    def test_get_fresh_data_uses_mock_without_network(self):
        stops, status, cb = get_fresh_data(use_mock=True)
        self.assertIn("20512", stops)
        self.assertTrue(all(v == "ok" for v in status.values()))
        self.assertTrue(len(cb) > 0)

    def test_get_fresh_data_returns_cached_within_ttl(self):
        first = get_fresh_data(use_mock=True)
        # Second call within TTL and not mock must hit the cache branch (line: fresh).
        second = get_fresh_data(use_mock=False)
        self.assertIs(second[0], first[0])
        self.assertIs(second[2], first[2])

    def test_get_fresh_data_live_paths_use_tracker_and_citibike(self):
        from unittest.mock import MagicMock, patch

        fake_tracker = MagicMock()
        fake_tracker.get_arrivals_with_status.return_value = ("ok", [{
            "departurestatus": "in 4 mins", "departuretime": "8:34 AM",
            "public_route": "126", "header": "126 NYC", "passload": "EMPTY", "vehicle_id": "7",
        }])
        fake_gtfs = MagicMock()
        fake_gtfs.get_upcoming.return_value = []  # force the public-API fallback
        with patch.object(server, "tracker", fake_tracker), patch.object(server, "gtfs_tracker", fake_gtfs):
            with patch.object(server, "cb_tracker") as cb:
                cb.get_station_status.return_value = [{"name": "X", "ebikes": 1, "classic": 2, "docks": 3, "walk_min": 3, "is_offline": False}]
                stops, status, data = get_fresh_data(use_mock=False)
        self.assertIn("20512", stops)
        self.assertEqual(status["20512"], "ok")
        self.assertEqual(data[0]["name"], "X")

    def test_get_fresh_data_citibike_exception_falls_back_to_mock(self):
        from unittest.mock import MagicMock, patch

        fake_tracker = MagicMock()
        fake_tracker.get_arrivals_with_status.return_value = ("empty", [])
        fake_gtfs = MagicMock()
        fake_gtfs.get_upcoming.return_value = []
        with patch.object(server, "tracker", fake_tracker), patch.object(server, "gtfs_tracker", fake_gtfs):
            with patch.object(server, "cb_tracker") as cb:
                cb.get_station_status.side_effect = Exception("GBFS down")
                cb.get_mock_data.return_value = [{"name": "MOCK"}]
                _stops, _status, data = get_fresh_data(use_mock=False)
        self.assertEqual(data[0]["name"], "MOCK")

    def test_get_fresh_data_uses_gtfs_when_available(self):
        from unittest.mock import MagicMock, patch

        fake_gtfs = MagicMock()
        fake_gtfs.get_upcoming.return_value = [{
            "route": "126", "destination": "126 NEW YORK", "eta": "in 5 mins (8:35 AM)",
            "occupancy": None, "vehicle_id": None, "live": True,
        }]
        with patch.object(server, "gtfs_tracker", fake_gtfs), patch.object(server, "cb_tracker") as cb:
            cb.get_station_status.return_value = []
            stops, status, _data = get_fresh_data(use_mock=False)
        self.assertEqual(status["20512"], "ok")
        self.assertEqual(stops["20512"][0]["eta"], "in 5 mins (8:35 AM)")

    def test_data_cache_ttl_follows_schedule_and_interactive(self):
        from unittest.mock import patch

        self.assertEqual(server.data_cache_ttl(interactive=True), server.INTERACTIVE_TTL)
        with patch.object(server, "get_target_poll_interval", return_value=600):
            self.assertEqual(server.data_cache_ttl(interactive=False), 600)

    def test_get_fresh_dashboard_image_is_cached_by_key(self):
        img1 = server.get_fresh_dashboard_image(use_mock=True, view="morning", width=800, height=480)
        img2 = server.get_fresh_dashboard_image(use_mock=True, view="morning", width=800, height=480)
        self.assertEqual(img1.size, img2.size)
        # Changing a key dimension should produce a (re-rendered) distinct entry.
        img3 = server.get_fresh_dashboard_image(use_mock=True, view="evening", width=800, height=600)
        self.assertEqual(img3.size, (800, 600))


class TestVersionFallback(unittest.TestCase):
    def test_get_version_falls_back_on_oserror(self):
        from unittest.mock import patch
        import version

        with patch("builtins.open", side_effect=OSError("missing")):
            self.assertEqual(version.get_version(), "0.0.0")


if __name__ == "__main__":
    unittest.main()
