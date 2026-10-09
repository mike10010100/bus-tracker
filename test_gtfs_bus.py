"""Unit tests for the NJ Transit GTFS-BUS client (static index + realtime merge)."""

import datetime
import io
import unittest
import zipfile
from unittest.mock import MagicMock, patch

from gtfs_bus import GTFSBusTracker, hms_to_secs, format_eta, format_clock


def make_zip():
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr(
            "calendar.txt",
            "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n"
            "WEEKDAY,1,1,1,1,1,0,0,20200101,20301231\n"
            "WEEKEND,0,0,0,0,0,1,1,20200101,20301231\n",
        )
        z.writestr("calendar_dates.txt", "service_id,date,exception_type\n")
        z.writestr(
            "routes.txt",
            "route_id,agency_id,route_short_name,route_long_name,route_type\n"
            "126,NJB,126,Hoboken - New York,3\n",
        )
        z.writestr(
            "trips.txt",
            "trip_id,route_id,service_id,trip_headsign,direction_id,block_id,shape_id\n"
            "A,126,WEEKDAY,126 NEW YORK,0,,\n"
            "B,126,WEEKDAY,126 NEW YORK,0,,\n"   # duplicate departure (other service variant)
            "C,126,WEEKEND,126 NEW YORK,0,,\n"
            "D,999,WEEKDAY,OTHER ROUTE,0,,\n",   # different route, must be ignored
        )
        z.writestr(
            "stop_times.txt",
            "trip_id,arrival_time,departure_time,stop_id,stop_sequence,pickup_type,drop_off_type\n"
            "A,08:00:00,08:00:00,20512,1,,\n"
            "B,08:00:00,08:00:00,20512,1,,\n"
            "C,09:00:00,09:00:00,20512,1,,\n"
            "D,09:00:00,09:00:00,20512,1,,\n",
        )
    return buf.getvalue()


def build_tracker():
    t = GTFSBusTracker(route="126", stops=["20512"], cache_dir="/tmp/nonexistent")
    t._index = t.build_index(make_zip())
    return t


def eta_minutes(eta_str):
    # "in 7 mins (5:54 PM)" -> 7
    return int(eta_str.split("in ", 1)[1].split(" mins")[0])


class TestParsingHelpers(unittest.TestCase):
    def test_hms_to_secs(self):
        self.assertEqual(hms_to_secs("08:00:00"), 8 * 3600)
        self.assertEqual(hms_to_secs("25:30:00"), 25 * 3600 + 30 * 60)
        self.assertEqual(hms_to_secs("bad"), -1)

    def test_format_eta_rounds(self):
        now = 1_000_000.0
        s = format_eta(now + 7 * 60 + 20, now)
        self.assertTrue(s.startswith("in 7 mins ("))
        self.assertEqual(eta_minutes(s), 7)

    def test_format_clock_not_empty(self):
        self.assertIn(":", format_clock(1_000_000.0))


class TestBuildIndex(unittest.TestCase):
    def test_index_contents(self):
        t = build_tracker()
        self.assertEqual(set(t._index["trips"].keys()), {"A", "B", "C"})  # route 126 only
        self.assertEqual(len(t._index["deps"]["20512"]), 3)  # A, B, C (not D)
        self.assertGreater(len(t._index["services"]), 0)
        self.assertEqual(t._index["valid_until"], "20301231")

    def test_active_services_weekday_vs_weekend(self):
        t = build_tracker()
        monday = datetime.date(2026, 10, 12)
        saturday = datetime.date(2026, 10, 10)
        self.assertIn("WEEKDAY", t.active_services(t._index, monday))
        self.assertNotIn("WEEKEND", t.active_services(t._index, monday))
        self.assertIn("WEEKEND", t.active_services(t._index, saturday))

    def test_active_services_exception_removes(self):
        t = build_tracker()
        t._index["exceptions"] = {"20261012": {"WEEKDAY": 2}}
        monday = datetime.date(2026, 10, 12)
        self.assertNotIn("WEEKDAY", t.active_services(t._index, monday))


class TestGetUpcoming(unittest.TestCase):
    def test_static_only_with_dedup_and_window(self):
        t = build_tracker()
        # Monday 07:30 -> A/B (08:00) dedup to one; C (WEEKEND) excluded; D route ignored.
        now = datetime.datetime(2026, 10, 12, 7, 30)
        out = t.get_upcoming("20512", limit=5, allow_realtime=False, now=now)
        self.assertEqual(len(out), 1)
        self.assertFalse(out[0]["live"])
        self.assertEqual(out[0]["destination"], "126 NEW YORK")
        self.assertLessEqual(eta_minutes(out[0]["eta"]), 31)

    def test_window_excludes_far_future(self):
        t = build_tracker()
        now = datetime.datetime(2026, 10, 12, 6, 0)  # 08:00 is 2h away -> in window
        self.assertEqual(len(t.get_upcoming("20512", allow_realtime=False, now=now)), 1)
        now2 = datetime.datetime(2026, 10, 12, 4, 0)  # 08:00 is 4h away -> out
        self.assertEqual(len(t.get_upcoming("20512", allow_realtime=False, now=now2)), 0)

    def test_realtime_overrides_schedule_and_marks_live(self):
        t = build_tracker()
        now = datetime.datetime(2026, 10, 12, 7, 30)
        base = datetime.datetime(2026, 10, 12, 8, 0).timestamp()
        # A predicts 10 minutes late; B has no realtime, but A should win the dedup.
        t.fetch_realtime = MagicMock(return_value={
            "A": {"20512": {"time": base + 600, "delay": 600, "vehicle_id": "v9"}},
        })
        out = t.get_upcoming("20512", limit=5, allow_realtime=True, now=now)
        self.assertEqual(len(out), 1)
        self.assertTrue(out[0]["live"])
        self.assertEqual(out[0]["vehicle_id"], "v9")
        # 08:10 vs now 07:30 -> ~40 mins
        self.assertIn(eta_minutes(out[0]["eta"]), (39, 40, 41))

    def test_realtime_unavailable_falls_back_to_schedule(self):
        t = build_tracker()
        t.fetch_realtime = MagicMock(return_value={})
        now = datetime.datetime(2026, 10, 12, 7, 30)
        out = t.get_upcoming("20512", limit=5, now=now)
        self.assertEqual(len(out), 1)
        self.assertFalse(out[0]["live"])

    def test_limit_respected(self):
        t = build_tracker()
        now = datetime.datetime(2026, 10, 12, 7, 30)
        out = t.get_upcoming("20512", limit=1, allow_realtime=False, now=now)
        self.assertEqual(len(out), 1)

    def test_no_index_returns_empty(self):
        t = GTFSBusTracker(route="126", stops=["20512"], cache_dir="/tmp/nonexistent")
        t.ensure_index = MagicMock(return_value=None)
        self.assertEqual(t.get_upcoming("20512"), [])


class TestAuth(unittest.TestCase):
    def test_missing_credentials_raises(self):
        with patch.dict("os.environ", {
            "NJT_USERNAME": "", "NJT_PASSWORD": "",
            "NJT_API_USERNAME": "", "NJT_API_PASSWORD": "",
        }):
            t = GTFSBusTracker(username="", password="", route="126", stops=["20512"])
            with self.assertRaises(ValueError):
                t.get_token()

    def test_auth_mints_token(self):
        t = GTFSBusTracker(username="u", password="p", route="126", stops=["20512"])
        t.session.post = MagicMock(return_value=MagicMock(
            raise_for_status=MagicMock(),
            json=MagicMock(return_value={"Authenticated": "True", "UserToken": "tok"}),
        ))
        self.assertEqual(t.get_token(), "tok")
        self.assertGreater(t.token_expiry, 0)

    def test_failed_auth_raises(self):
        t = GTFSBusTracker(username="u", password="p", route="126", stops=["20512"])
        t.session.post = MagicMock(return_value=MagicMock(
            raise_for_status=MagicMock(),
            json=MagicMock(return_value={"Authenticated": "False"}),
        ))
        with self.assertRaises(RuntimeError):
            t.get_token()


if __name__ == "__main__":
    unittest.main()
