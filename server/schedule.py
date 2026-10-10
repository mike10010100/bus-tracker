import os
from datetime import datetime
from typing import NamedTuple, Optional


class CommuteLighting(NamedTuple):
    brightness: int
    warmth: int


def _parse_hour_env(name: str, default: float) -> float:
    """Parses a decimal-hour env var (e.g. '9.5' or '10'), falling back to
    default on any error."""
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        return float(raw)
    except ValueError:
        return default


# Peak commute windows are configurable so the schedule can be tuned without a
# code change (e.g. extending the morning window while testing on the device).
PEAK_AM_START = _parse_hour_env("PEAK_AM_START", 7.5)
PEAK_AM_END = _parse_hour_env("PEAK_AM_END", 9.5)
PEAK_PM_START = _parse_hour_env("PEAK_PM_START", 16.5)
PEAK_PM_END = _parse_hour_env("PEAK_PM_END", 19.0)
# Weekends are off-peak (no commute: light off, slow polling) unless
# PEAK_WEEKENDS=1.
PEAK_WEEKENDS = os.environ.get("PEAK_WEEKENDS", "").strip().lower() in ("1", "true", "yes", "on")


def is_peak_commute_hours(dt: Optional[datetime] = None) -> bool:
    """
    Returns True during peak commute windows in Hoboken, NJ. Defaults:
    - Morning commute: 7:30 AM - 9:30 AM
    - Evening commute: 4:30 PM - 7:00 PM (16:30 - 19:00)
    Override with PEAK_AM_START/PEAK_AM_END/PEAK_PM_START/PEAK_PM_END.
    Saturdays and Sundays are never peak unless PEAK_WEEKENDS=1.
    """
    if dt is None:
        dt = datetime.now()
    if dt.weekday() >= 5 and not PEAK_WEEKENDS:
        return False
    hour = dt.hour + dt.minute / 60.0
    return (PEAK_AM_START <= hour < PEAK_AM_END) or (PEAK_PM_START <= hour < PEAK_PM_END)


def get_commute_lighting(dt: Optional[datetime] = None) -> CommuteLighting:
    """
    Returns (brightness, warmth) for the Hoboken, NJ local time.
    A cozy ambient glow (8, 12) is used during the peak morning and evening
    commute windows; the frontlight is off (0, 0) off-peak and overnight to
    save battery. This is a fixed schedule, not sunrise/sunset calculation.
    """
    if is_peak_commute_hours(dt=dt):
        return CommuteLighting(8, 12)
    return CommuteLighting(0, 0)


# When set (any non-empty value), the server always advertises the fast poll
# interval regardless of the time of day. For testing only.
FORCE_FAST_POLL = os.environ.get("FORCE_FAST_POLL", "").strip().lower() in ("1", "true", "yes", "on")

# Overnight "deep eco" window: a long poll interval while nobody is commuting.
# The window may wrap past midnight (start > end).
OVERNIGHT_START = _parse_hour_env("OVERNIGHT_START", 22.0)
OVERNIGHT_END = _parse_hour_env("OVERNIGHT_END", 6.0)
# Poll intervals advertised to clients are clamped to [30 s, 2 h]: a 0 or
# negative value would make clients spin, a huge one would freeze the board.
MIN_POLL_INTERVAL = 30
MAX_POLL_INTERVAL = 7200


def _parse_interval_env(name: str, default: int) -> int:
    """Parses a poll-interval env var (seconds), clamped to the allowed range."""
    value = int(_parse_hour_env(name, float(default)))
    return max(MIN_POLL_INTERVAL, min(MAX_POLL_INTERVAL, value))


OVERNIGHT_INTERVAL = _parse_interval_env("OVERNIGHT_INTERVAL", 3600)
OFFPEAK_INTERVAL = _parse_interval_env("OFFPEAK_INTERVAL", 600)


def is_overnight_hours(dt: Optional[datetime] = None) -> bool:
    """
    Returns True inside the overnight deep-eco window (default 22:00-06:00).
    Handles a window that wraps past midnight.
    """
    start = OVERNIGHT_START
    end = OVERNIGHT_END
    if dt is None:
        dt = datetime.now()
    hour = dt.hour + dt.minute / 60.0
    if start <= end:
        return start <= hour < end
    return hour >= start or hour < end


def is_force_fast_poll() -> bool:
    """Checks whether FORCE_FAST_POLL is enabled."""
    return bool(FORCE_FAST_POLL)


def get_presentation(dt: Optional[datetime] = None) -> str:
    """
    Returns the client-facing presentation state:
    - "interactive" during peak commute: the client stays awake, so the panel is
      a live, tappable dashboard with buttons.
    - "idle" off-peak daytime: the client deep-suspends between polls and a tap
      cannot wake the SoC, so the panel shows an inert "press power" strip.
    - "dormant" overnight (22:00-06:00): deep-suspend with an inert "sleeping"
      face; a power press still starts an interaction session.

    A client may override to "interactive" while it is awake in a power-button
    interaction session (see the ?present= param).

    FORCE_FAST_POLL=1 (testing) forces "interactive" so experiments are unaffected.
    """
    if is_force_fast_poll():
        return "interactive"
    if is_peak_commute_hours(dt=dt):
        return "interactive"
    if is_overnight_hours(dt=dt):
        return "dormant"
    return "idle"


def get_dormant_note() -> str:
    """
    Human-readable label for the dormant overnight strip, announcing when the
    dashboard wakes (the overnight window end).
    """
    # Round to the nearest minute first so e.g. 6.999 reads 7:00, not 6:00.
    total_minutes = int(round(OVERNIGHT_END * 60)) % (24 * 60)
    hour, minute = divmod(total_minutes, 60)
    suffix = "AM" if hour < 12 else "PM"
    hour12 = hour % 12 or 12
    return f"SLEEPING — back at {hour12}:{minute:02d} {suffix} · press power to interact"


def get_status_note(presentation: str) -> str:
    """
    Bottom-strip label for a non-interactive presentation (idle/dormant).
    """
    if presentation == "dormant":
        return get_dormant_note()
    if presentation == "idle":
        return "PRESS POWER BUTTON TO INTERACT"
    return ""


def get_target_poll_interval(dt: Optional[datetime] = None) -> int:
    """
    Returns target Kindle poll interval in seconds:
    - 60s during peak commute rush (the client aligns this to the top of each
      minute, so the on-screen clock rolls exactly when the new data lands)
    - 3600s (1 hour) overnight deep-eco mode
    - 600s (10 min) off-peak Eco Mode

    FORCE_FAST_POLL=1 forces the fast interval at all times (testing aid).
    """
    if is_force_fast_poll():
        return 60
    if is_peak_commute_hours(dt=dt):
        return 60
    if is_overnight_hours(dt=dt):
        return OVERNIGHT_INTERVAL
    return OFFPEAK_INTERVAL
