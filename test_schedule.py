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

    def test_overnight_lighting_off(self):
        # Overnight the frontlight stays off regardless of the poll interval.
        dt = datetime(2026, 10, 8, 2, 0)
        self.assertEqual(get_commute_lighting(dt), (0, 0))

    def test_peak_window_end_is_exclusive(self):
        # Defaults: morning peak ends at 9:30 (exclusive).
        self.assertTrue(is_peak_commute_hours(datetime(2026, 10, 8, 9, 29)))
        self.assertFalse(is_peak_commute_hours(datetime(2026, 10, 8, 9, 30)))

    def test_overnight_deep_eco(self):
        # Default overnight window is 22:00-06:00 -> 3600s.
        self.assertEqual(get_target_poll_interval(datetime(2026, 10, 8, 23, 0)), 3600)
        self.assertEqual(get_target_poll_interval(datetime(2026, 10, 8, 2, 0)), 3600)
        self.assertEqual(get_target_poll_interval(datetime(2026, 10, 8, 5, 59)), 3600)
        # Boundaries: 06:00 is no longer overnight (off-peak 600s).
        self.assertEqual(get_target_poll_interval(datetime(2026, 10, 8, 6, 0)), 600)
        self.assertEqual(get_target_poll_interval(datetime(2026, 10, 8, 21, 59)), 600)

    def test_is_overnight_hours_wraps_midnight(self):
        from server import is_overnight_hours
        self.assertTrue(is_overnight_hours(datetime(2026, 10, 8, 23, 30)))
        self.assertTrue(is_overnight_hours(datetime(2026, 10, 8, 0, 30)))
        self.assertFalse(is_overnight_hours(datetime(2026, 10, 8, 12, 0)))

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
