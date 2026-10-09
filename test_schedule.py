"""
Unit tests for commute schedule and lighting logic in server.py
"""

import unittest
from datetime import datetime
from server import is_peak_commute_hours, get_commute_lighting, get_target_poll_interval


class TestCommuteSchedule(unittest.TestCase):
    def test_morning_rush(self):
        # 8:15 AM
        dt = datetime(2026, 10, 8, 8, 15)
        self.assertTrue(is_peak_commute_hours(dt))
        self.assertEqual(get_commute_lighting(dt), (8, 12))
        self.assertEqual(get_target_poll_interval(dt), 60)

    def test_evening_rush(self):
        # 5:30 PM (17:30)
        dt = datetime(2026, 10, 8, 17, 30)
        self.assertTrue(is_peak_commute_hours(dt))
        self.assertEqual(get_commute_lighting(dt), (8, 12))
        self.assertEqual(get_target_poll_interval(dt), 60)

    def test_midday_eco(self):
        # 1:30 PM (13:30)
        dt = datetime(2026, 10, 8, 13, 30)
        self.assertFalse(is_peak_commute_hours(dt))
        self.assertEqual(get_commute_lighting(dt), (0, 0))
        self.assertEqual(get_target_poll_interval(dt), 600)

    def test_overnight_eco(self):
        # 2:00 AM
        dt = datetime(2026, 10, 8, 2, 0)
        self.assertFalse(is_peak_commute_hours(dt))
        self.assertEqual(get_commute_lighting(dt), (0, 0))
        self.assertEqual(get_target_poll_interval(dt), 600)

    def test_peak_window_end_is_exclusive(self):
        # Defaults: morning peak ends at 9:30 (exclusive).
        self.assertTrue(is_peak_commute_hours(datetime(2026, 10, 8, 9, 29)))
        self.assertFalse(is_peak_commute_hours(datetime(2026, 10, 8, 9, 30)))

    def test_force_fast_poll_overrides_schedule(self):
        import server

        original = server.FORCE_FAST_POLL
        try:
            server.FORCE_FAST_POLL = True
            # Even at 3 AM, forced-fast returns 60s.
            self.assertEqual(server.get_target_poll_interval(datetime(2026, 10, 8, 3, 0)), 60)
        finally:
            server.FORCE_FAST_POLL = original


if __name__ == "__main__":
    unittest.main()
