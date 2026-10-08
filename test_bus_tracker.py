"""
Unit tests for the NJ Transit arrival client (BUSDV2 + GraphQL fallback),
canonical Arrival normalization, and the status-aware fetch API.
"""

import unittest
from unittest.mock import patch, MagicMock

from bus_tracker import NJTransitBusTracker, normalize_arrival


class FakeResponse:
    def __init__(self, payload=None, status=200, raise_exc=None):
        self._payload = payload or {}
        self.status_code = status
        self._raise_exc = raise_exc

    def raise_for_status(self):
        if self._raise_exc:
            raise self._raise_exc
        if self.status_code >= 400:
            raise RuntimeError(f"HTTP {self.status_code}")

    def json(self):
        return self._payload


class TestNormalizeArrival(unittest.TestCase):
    def test_status_and_time(self):
        rec = normalize_arrival({
            "departurestatus": "  in 5 mins ",
            "departuretime": " 8:35 AM ",
            "public_route": "126",
            "header": " 126 NEW YORK ",
            "passload": "SEATS_AVAILABLE",
            "vehicle_id": "25248",
        })
        self.assertEqual(rec["eta"], "in 5 mins (8:35 AM)")
        self.assertEqual(rec["route"], "126")
        self.assertEqual(rec["destination"], "126 NEW YORK")
        self.assertEqual(rec["occupancy"], "SEATS_AVAILABLE")
        self.assertEqual(rec["vehicle_id"], "25248")

    def test_status_only(self):
        self.assertEqual(normalize_arrival({"departurestatus": "APPROACHING"})["eta"], "APPROACHING")

    def test_time_only(self):
        self.assertEqual(normalize_arrival({"departuretime": "8:35 AM"})["eta"], "8:35 AM")

    def test_neither_defaults_to_scheduled(self):
        rec = normalize_arrival({})
        self.assertEqual(rec["eta"], "Scheduled")
        self.assertEqual(rec["destination"], "")
        self.assertIsNone(rec["route"])


class TestInitAndURLs(unittest.TestCase):
    def test_email_username_is_stripped_to_handle(self):
        t = NJTransitBusTracker(username="jane.doe@example.com", password="pw")
        self.assertEqual(t.username, "jane.doe")
        self.assertEqual(t.password, "pw")

    def test_plain_username_preserved(self):
        t = NJTransitBusTracker(username="jdoe", password="pw")
        self.assertEqual(t.username, "jdoe")

    def test_env_fallback(self):
        with patch.dict("os.environ", {"NJT_USERNAME": "envuser", "NJT_PASSWORD": "envpw"}):
            t = NJTransitBusTracker()
            self.assertEqual(t.username, "envuser")
            self.assertEqual(t.password, "envpw")

    def test_base_url_trailing_slash_trimmed(self):
        t = NJTransitBusTracker(base_url="http://example.com/")
        self.assertEqual(t.base_url, "http://example.com")

    def test_url_properties(self):
        t = NJTransitBusTracker(base_url="http://example.com")
        self.assertEqual(t.auth_url, "http://example.com/api/BUSDV2/authenticateUser")
        self.assertEqual(t.bus_dv_url, "http://example.com/api/BUSDV2/getBusDV")


class TestGetToken(unittest.TestCase):
    def test_missing_credentials_raises_value_error(self):
        t = NJTransitBusTracker(username="", password="")
        with self.assertRaises(ValueError):
            t.get_token()

    def test_successful_auth_mints_token(self):
        t = NJTransitBusTracker(username="u", password="p")
        t.session.post = MagicMock(return_value=FakeResponse({"Authenticated": "true", "UserToken": "tok123"}))
        self.assertEqual(t.get_token(), "tok123")
        self.assertGreater(t.token_expiry, 0)

    def test_failed_auth_raises_runtime_error(self):
        t = NJTransitBusTracker(username="u", password="p")
        t.session.post = MagicMock(return_value=FakeResponse({"Authenticated": "false"}))
        with self.assertRaises(RuntimeError):
            t.get_token()

    def test_cached_token_returned_without_new_auth(self):
        t = NJTransitBusTracker(username="u", password="p")
        t.token = "cached"
        t.token_expiry = 9_999_999_999
        t.session.post = MagicMock(side_effect=AssertionError("should not re-auth"))
        self.assertEqual(t.get_token(), "cached")


class TestGraphQL(unittest.TestCase):
    def test_parses_and_filters_by_route(self):
        t = NJTransitBusTracker(base_url="http://example.com")
        payload = {"data": {"getBusArrivalsByStopID": [
            {"publicRoute": "126", "header": "126 NEW YORK", "vehicleId": "111", "passload": "EMPTY",
             "departuretime": "8:35 AM", "departurestatus": "in 5 mins"},
            {"publicRoute": "22", "header": "22 OTHER", "vehicleId": "222"},
        ]}}
        t.session.post = MagicMock(return_value=FakeResponse(payload))
        trips = t.get_arrivals_graphql(stop_id="20512", route="126")
        self.assertEqual(len(trips), 1)
        self.assertEqual(trips[0]["vehicle_id"], "111")
        self.assertEqual(trips[0]["public_route"], "126")

    def test_empty_data_returns_empty_list(self):
        t = NJTransitBusTracker(base_url="http://example.com")
        t.session.post = MagicMock(return_value=FakeResponse({"data": {}}))
        self.assertEqual(t.get_arrivals_graphql(stop_id="20512"), [])


class TestGetArrivalsWithStatus(unittest.TestCase):
    def test_busdv2_success_is_ok(self):
        t = NJTransitBusTracker(username="u", password="p")
        t.token = "tok"
        t.token_expiry = 9_999_999_999
        t.session.post = MagicMock(return_value=FakeResponse({"DVTrip": [{"public_route": "126"}]}))
        status, trips = t.get_arrivals_with_status("20512")
        self.assertEqual(status, NJTransitBusTracker.STATUS_OK)
        self.assertEqual(len(trips), 1)

    def test_busdv2_empty_is_empty(self):
        t = NJTransitBusTracker(username="u", password="p")
        t.token = "tok"
        t.token_expiry = 9_999_999_999
        t.session.post = MagicMock(return_value=FakeResponse({"DVTrip": []}))
        status, trips = t.get_arrivals_with_status("20512")
        self.assertEqual(status, NJTransitBusTracker.STATUS_EMPTY)
        self.assertEqual(trips, [])

    def test_busdv2_failure_falls_back_to_graphql(self):
        t = NJTransitBusTracker(username="u", password="p")
        t.token = "tok"
        t.token_expiry = 9_999_999_999
        t.session.post = MagicMock(side_effect=Exception("BUSDV2 down"))
        t.get_arrivals_graphql = MagicMock(return_value=[{"public_route": "126"}])
        status, trips = t.get_arrivals_with_status("20512")
        self.assertEqual(status, NJTransitBusTracker.STATUS_OK)
        self.assertEqual(len(trips), 1)

    def test_both_fail_is_error(self):
        t = NJTransitBusTracker(username="u", password="p")
        t.token = "tok"
        t.token_expiry = 9_999_999_999
        t.session.post = MagicMock(side_effect=Exception("down"))
        t.get_arrivals_graphql = MagicMock(side_effect=Exception("also down"))
        status, trips = t.get_arrivals_with_status("20512")
        self.assertEqual(status, NJTransitBusTracker.STATUS_ERROR)
        self.assertEqual(trips, [])

    def test_get_arrivals_wrapper_returns_trips_only(self):
        t = NJTransitBusTracker(username="u", password="p")
        t.token = "tok"
        t.token_expiry = 9_999_999_999
        t.session.post = MagicMock(return_value=FakeResponse({"DVTrip": [{"public_route": "126"}]}))
        self.assertEqual(len(t.get_arrivals("20512")), 1)


class TestGetSummary(unittest.TestCase):
    def test_summary_normalizes_each_stop(self):
        t = NJTransitBusTracker(username="u", password="p")
        t.get_arrivals = MagicMock(return_value=[{
            "departurestatus": "in 3 mins", "departuretime": "8:33 AM",
            "public_route": "126", "header": "126 NYC", "passload": "EMPTY", "vehicle_id": "9",
        }])
        summary = t.get_summary({"Stop A": "20512", "Stop B": "20494"})
        self.assertEqual(set(summary.keys()), {"Stop A", "Stop B"})
        self.assertEqual(summary["Stop A"][0]["eta"], "in 3 mins (8:33 AM)")


if __name__ == "__main__":
    unittest.main()


class TestEnvFileFallback(unittest.TestCase):
    def test_parses_keys_ignoring_comments_and_blanks(self):
        import os as _os
        import tempfile
        from bus_tracker import load_env_file

        with tempfile.NamedTemporaryFile("w", suffix=".env", delete=False) as f:
            f.write("# a comment\n\nNJT_TEST_KEY=somevalue\nQUOTED='quotedvalue'\nSPACED = padded value \n")
            path = f.name
        try:
            _os.environ.pop("NJT_TEST_KEY", None)
            _os.environ.pop("QUOTED", None)
            _os.environ.pop("SPACED", None)
            load_env_file(path)
            self.assertEqual(_os.environ.get("NJT_TEST_KEY"), "somevalue")
            self.assertEqual(_os.environ.get("QUOTED"), "quotedvalue")
            self.assertEqual(_os.environ.get("SPACED"), "padded value")
        finally:
            _os.remove(path)
            for k in ("NJT_TEST_KEY", "QUOTED", "SPACED"):
                _os.environ.pop(k, None)

    def test_existing_env_is_not_overwritten(self):
        import os as _os
        import tempfile
        from bus_tracker import load_env_file

        with tempfile.NamedTemporaryFile("w", suffix=".env", delete=False) as f:
            f.write("PRESET=fromfile\n")
            path = f.name
        try:
            _os.environ["PRESET"] = "fromenv"
            load_env_file(path)
            self.assertEqual(_os.environ["PRESET"], "fromenv")
        finally:
            _os.remove(path)
            _os.environ.pop("PRESET", None)

    def test_missing_file_is_noop(self):
        from bus_tracker import load_env_file
        load_env_file("/nonexistent/path/.env")
