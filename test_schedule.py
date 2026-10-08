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


if __name__ == "__main__":
    unittest.main()
