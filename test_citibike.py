"""
Unit tests for Citi Bike live dock tracker module and dashboard rendering integration.
"""

import os
import unittest
from unittest.mock import patch, MagicMock
from PIL import Image

from citibike import CitiBikeTracker, DEFAULT_STATIONS
from render_dashboard import render_dashboard, get_mock_data


class TestCitiBikeTracker(unittest.TestCase):
    def test_default_stations_configuration(self):
        self.assertEqual(len(DEFAULT_STATIONS), 3)
        station_names = [s["name"] for s in DEFAULT_STATIONS]
        self.assertIn("Clinton & 9th", station_names)
        self.assertIn("Washington & 11th", station_names)
        self.assertIn("Washington & 8th", station_names)

        # Ensure walk times and distances are reasonable
        for s in DEFAULT_STATIONS:
            self.assertGreater(s["walk_min"], 0)
            self.assertLessEqual(s["walk_min"], 10)
            self.assertGreater(s["distance_m"], 0)

    def test_get_mock_data(self):
        tracker = CitiBikeTracker()
        mock_data = tracker.get_mock_data()
        self.assertEqual(len(mock_data), 3)
        for s in mock_data:
            self.assertIn("id", s)
            self.assertIn("name", s)
            self.assertIn("ebikes", s)
            self.assertIn("classic", s)
            self.assertIn("docks", s)
            self.assertIn("walk_min", s)
            self.assertGreaterEqual(s["ebikes"], 0)
            self.assertGreaterEqual(s["classic"], 0)
            self.assertGreaterEqual(s["docks"], 0)

    def test_caching_behavior(self):
        tracker = CitiBikeTracker(cache_ttl=60)
        tracker._last_fetch_time = 1000.0
        tracker._cached_data = [{"id": "cached_1", "name": "Cached Station"}]

        with patch("time.time", return_value=1010.0):
            res = tracker.get_station_status(force_refresh=False)
            self.assertEqual(res, tracker._cached_data)

    def test_network_failure_falls_back_to_mock(self):
        tracker = CitiBikeTracker()
        with patch("urllib.request.urlopen", side_effect=Exception("Connection refused")):
            res = tracker.get_station_status(force_refresh=True)
            self.assertEqual(len(res), 3)
            self.assertEqual(res[0]["name"], "Clinton & 9th")

    def test_successful_gbfs_parsing(self):
        fake_payload = {
            "data": {
                "stations": [
                    {
                        "station_id": "fadf00cf-d84a-49e8-9607-c67154915412",
                        "num_bikes_available": 15,
                        "num_ebikes_available": 5,
                        "num_docks_available": 10,
                        "is_renting": 1,
                        "is_returning": 1,
                    },
                    {
                        "station_id": "f417d8da-0f15-49b0-9e3c-3c3e55c2691d",
                        "num_bikes_available": 8,
                        "num_ebikes_available": 2,
                        "num_docks_available": 16,
                        "is_renting": 1,
                        "is_returning": 1,
                    },
                    {
                        "station_id": "519824e4-69ba-4270-a395-17c204f328f8",
                        "num_bikes_available": 12,
                        "num_ebikes_available": 8,
                        "num_docks_available": 9,
                        "is_renting": 1,
                        "is_returning": 1,
                    },
                ]
            }
        }

        mock_resp = MagicMock()
        mock_resp.read.return_value = json_str = (
            __import__("json").dumps(fake_payload).encode("utf-8")
        )
        mock_resp.__enter__.return_value = mock_resp

        tracker = CitiBikeTracker()
        with patch("urllib.request.urlopen", return_value=mock_resp):
            res = tracker.get_station_status(force_refresh=True)
            self.assertEqual(len(res), 3)

            # Check Clinton & 9th
            c9 = next(s for s in res if s["name"] == "Clinton & 9th")
            self.assertEqual(c9["total_bikes"], 15)
            self.assertEqual(c9["ebikes"], 5)
            self.assertEqual(c9["classic"], 10)
            self.assertEqual(c9["docks"], 10)

    def test_render_dashboard_with_citibike(self):
        output_file = "/tmp/test_citibike_render.png"
        stops_data = get_mock_data()
        tracker = CitiBikeTracker()
        cb_data = tracker.get_mock_data()

        render_dashboard(
            stops_data,
            citibike_data=cb_data,
            output_path=output_file,
            is_mock=True,
            batt_level=85,
            is_charging=False,
        )

        self.assertTrue(os.path.exists(output_file))
        img = Image.open(output_file)
        self.assertEqual(img.size, (800, 480))
        img.close()
        os.remove(output_file)

    def test_resolve_view_modes(self):
        from render_dashboard import resolve_view

        # Morning hours (5 AM to 11:59 AM)
        self.assertEqual(resolve_view("auto", hour=5), "morning")
        self.assertEqual(resolve_view("auto", hour=8), "morning")
        self.assertEqual(resolve_view("auto", hour=11), "morning")

        # Afternoon / Evening / Night hours
        self.assertEqual(resolve_view("auto", hour=12), "evening")
        self.assertEqual(resolve_view("auto", hour=17), "evening")
        self.assertEqual(resolve_view("auto", hour=23), "evening")
        self.assertEqual(resolve_view("auto", hour=2), "evening")

        # Explicit overrides
        self.assertEqual(resolve_view("morning", hour=20), "morning")
        self.assertEqual(resolve_view("evening", hour=9), "evening")
        self.assertEqual(resolve_view("citi", hour=18), "morning")
        self.assertEqual(resolve_view("bus", hour=7), "evening")

    def test_render_morning_and_evening_views(self):
        stops_data = get_mock_data()
        tracker = CitiBikeTracker()
        cb_data = tracker.get_mock_data()

        for view_mode in ["morning", "evening"]:
            out_file = f"/tmp/test_view_{view_mode}.png"
            render_dashboard(
                stops_data,
                citibike_data=cb_data,
                output_path=out_file,
                view=view_mode,
                is_mock=True,
                batt_level=90,
                is_charging=True,
            )
            self.assertTrue(os.path.exists(out_file))
            img = Image.open(out_file)
            self.assertEqual(img.size, (800, 480))
            img.close()
            os.remove(out_file)

    def test_render_tall_mode_and_kindle_formatting(self):
        from server import format_for_kindle

        stops_data = get_mock_data()
        tracker = CitiBikeTracker()
        cb_data = tracker.get_mock_data()

        for view_mode in ["morning", "evening"]:
            out_file = f"/tmp/test_tall_view_{view_mode}.png"
            render_dashboard(
                stops_data,
                citibike_data=cb_data,
                output_path=out_file,
                view=view_mode,
                is_mock=True,
                batt_level=90,
                is_charging=False,
                width=800,
                height=600,
            )
            self.assertTrue(os.path.exists(out_file))
            img = Image.open(out_file)
            self.assertEqual(img.size, (800, 600))

            # Format for Kindle PW5 (rotate=90)
            kindle_img = format_for_kindle(img, orientation="landscape", rotation=90)
            self.assertEqual(kindle_img.size, (1236, 1648))
            self.assertEqual(kindle_img.mode, "L")

            img.close()
            os.remove(out_file)


if __name__ == "__main__":
    unittest.main()

