"""
Citi Bike Live Station Inventory Tracker
Fetches real-time bike & dock availability for the closest stations to 919 Park Ave, Hoboken, NJ,
with dynamic prioritization for stations that currently have e-bikes available.
"""

import json
import threading
import time
import urllib.request
from typing import Any, NamedTuple, Optional, TypedDict

from version import VERSION


class StationConfig(TypedDict, total=False):
    id: str
    name: str
    full_name: str
    walk_min: int
    distance_m: int


class StationStatus(TypedDict, total=False):
    id: str
    name: str
    full_name: str
    walk_min: int
    distance_m: int
    ebikes: int
    classic: int
    total_bikes: int
    docks: int
    is_offline: bool
    is_returning: bool
    # True when the station is missing from the feed: its counts are unknown,
    # so it is rendered as offline/unknown rather than "0 bikes, 0 docks".
    is_unknown: bool


# Snapshot statuses. Unlike the old behaviour, a failed feed is never papered
# over with mock numbers: callers get ERROR (no data) or STALE (cached data no
# older than MAX_STALE_SECS, rendered with an "as of HH:MM" marker).
CB_STATUS_OK = "ok"
CB_STATUS_STALE = "stale"
CB_STATUS_ERROR = "error"
MAX_STALE_SECS = 15 * 60


class CitiBikeSnapshot(NamedTuple):
    status: str
    stations: list[StationStatus]
    as_of: Optional[float]  # unix time of the feed fetch, None when no data


class CitiBikeUnavailable(RuntimeError):
    """Raised when no live or acceptably fresh cached data is available."""


# Closest Citi Bike stations to 919 Park Ave, Hoboken, NJ (40.7484552, -74.0302997)
# Pedestrian routing distances and walk times based on actual street routing via crosswalks
DEFAULT_STATIONS: list[StationConfig] = [
    {
        "id": "fadf00cf-d84a-49e8-9607-c67154915412",
        "name": "Clinton & 9th",
        "full_name": "Columbus Park - Clinton St & 9 St",
        "walk_min": 3,
        "distance_m": 246,
    },
    {
        "id": "f417d8da-0f15-49b0-9e3c-3c3e55c2691d",
        "name": "Washington & 11th",
        "full_name": "11 St & Washington St",
        "walk_min": 6,
        "distance_m": 442,
    },
    {
        "id": "655b0878-c22b-4f8c-8675-21bf30d1eebf",
        "name": "Willow & 12th",
        "full_name": "Willow Ave & 12 St",
        "walk_min": 6,
        "distance_m": 460,
    },
    {
        "id": "519824e4-69ba-4270-a395-17c204f328f8",
        "name": "Washington & 8th",
        "full_name": "8 St & Washington St",
        "walk_min": 6,
        "distance_m": 462,
    },
    {
        "id": "21c4a77d-b1ae-4262-b143-087fd14a07b8",
        "name": "Clinton & 7th",
        "full_name": "Clinton St & 7 St",
        "walk_min": 7,
        "distance_m": 556,
    },
    {
        "id": "9d344652-976b-4c2d-bede-2ef19b0fbf13",
        "name": "Grand & 6th",
        "full_name": "6 St & Grand St",
        "walk_min": 10,
        "distance_m": 754,
    },
]

GBFS_STATUS_URL = "https://gbfs.citibikenyc.com/gbfs/en/station_status.json"


def sort_stations_by_ebike_priority(
    stations: list[Any],
) -> list[Any]:
    """
    Sorts Citi Bike stations to prioritize e-bike availability for commuters:
    1. Active stations with e-bikes (ebikes > 0) come first,
       ordered primarily by walk_min (closest walk first), then by ebike count descending.
    2. Active stations with 0 e-bikes come next, ordered by walk_min (closest walk first).
    3. Offline stations are pushed to the end.
    """

    def priority_key(s: Any):
        is_offline = 1 if s.get("is_offline") else 0
        has_ebikes = 0 if (s.get("ebikes", 0) > 0 and not is_offline) else 1
        walk_min = s.get("walk_min", 99)
        ebikes = s.get("ebikes", 0)
        return (is_offline, has_ebikes, walk_min, -ebikes)

    return sorted(stations, key=priority_key)


class CitiBikeTracker:
    def __init__(
        self, stations: Optional[list[StationConfig]] = None, cache_ttl: int = 30
    ):
        self.stations = stations or DEFAULT_STATIONS
        self.cache_ttl = cache_ttl
        self._last_fetch_time = 0.0
        self._cached_data: list[StationStatus] = []
        self._lock = threading.Lock()

    def _fetch_live(self) -> list[StationStatus]:
        """Fetches the GBFS feed and maps it onto the configured stations."""
        req = urllib.request.Request(
            GBFS_STATUS_URL,
            headers={
                "User-Agent": f"TransitTracker/{VERSION} (Kindle Transit Display)"
            },
        )
        with urllib.request.urlopen(req, timeout=5) as resp:
            payload = json.loads(resp.read().decode("utf-8"))

        stations_raw = payload.get("data", {}).get("stations", [])
        status_map = {s["station_id"]: s for s in stations_raw if "station_id" in s}

        results = []
        for target in self.stations:
            sid = target["id"]
            raw_info = status_map.get(sid)
            is_unknown = raw_info is None
            raw_info = raw_info or {}

            ebikes = raw_info.get("num_ebikes_available", 0)
            total_bikes = raw_info.get("num_bikes_available", 0)
            classic = max(0, total_bikes - ebikes)
            docks = raw_info.get("num_docks_available", 0)
            is_renting = (not is_unknown) and raw_info.get("is_renting", 1) == 1
            is_returning = (not is_unknown) and raw_info.get("is_returning", 1) == 1
            is_offline = not is_renting

            results.append(
                {
                    "id": sid,
                    "name": target["name"],
                    "full_name": target.get("full_name", target["name"]),
                    "walk_min": target["walk_min"],
                    "distance_m": target.get("distance_m", 0),
                    "ebikes": ebikes,
                    "classic": classic,
                    "total_bikes": total_bikes,
                    "docks": docks,
                    "is_offline": is_offline,
                    "is_returning": is_returning,
                    "is_unknown": is_unknown,
                }
            )
        return sort_stations_by_ebike_priority(results)

    def get_snapshot(self, force_refresh: bool = False) -> CitiBikeSnapshot:
        """
        Returns the station status with an explicit freshness status. Never
        raises and never returns mock data:
        - OK: fetched within cache_ttl (or just now);
        - STALE: the feed failed, cached data no older than MAX_STALE_SECS;
        - ERROR: the feed failed and there is no acceptable cached data.
        """
        with self._lock:
            now = time.time()
            if (
                not force_refresh
                and self._cached_data
                and (now - self._last_fetch_time < self.cache_ttl)
            ):
                return CitiBikeSnapshot(
                    CB_STATUS_OK, self._cached_data, self._last_fetch_time
                )
            try:
                data = self._fetch_live()
            except Exception as e:
                age = now - self._last_fetch_time
                if self._cached_data and age <= MAX_STALE_SECS:
                    print(
                        f"[CitiBike] feed unavailable ({e}); serving data from {int(age)}s ago"
                    )
                    return CitiBikeSnapshot(
                        CB_STATUS_STALE, self._cached_data, self._last_fetch_time
                    )
                print(f"[CitiBike] feed unavailable ({e}); no usable cached data")
                return CitiBikeSnapshot(CB_STATUS_ERROR, [], None)
            self._cached_data = data
            self._last_fetch_time = now
            return CitiBikeSnapshot(CB_STATUS_OK, data, now)

    def get_station_status(self, force_refresh: bool = False) -> list[StationStatus]:
        """
        Returns real-time status for the configured target stations,
        dynamically prioritized by e-bike availability and proximity.
        Uses in-memory cache if within cache_ttl; on a feed failure returns
        cached data up to MAX_STALE_SECS old, else raises CitiBikeUnavailable.
        """
        snap = self.get_snapshot(force_refresh=force_refresh)
        if snap.status == CB_STATUS_ERROR:
            raise CitiBikeUnavailable("Citi Bike data unavailable")
        return snap.stations

    def get_mock_data(self) -> list[StationStatus]:
        """Mock preview data (?mock=1 only) with realistic commute availability across all tracked stations."""
        mock = [
            {
                "id": "fadf00cf-d84a-49e8-9607-c67154915412",
                "name": "Clinton & 9th",
                "full_name": "Columbus Park - Clinton St & 9 St",
                "walk_min": 3,
                "distance_m": 246,
                "ebikes": 5,
                "classic": 12,
                "total_bikes": 17,
                "docks": 9,
                "is_offline": False,
                "is_returning": True,
            },
            {
                "id": "f417d8da-0f15-49b0-9e3c-3c3e55c2691d",
                "name": "Washington & 11th",
                "full_name": "11 St & Washington St",
                "walk_min": 6,
                "distance_m": 442,
                "ebikes": 2,
                "classic": 16,
                "total_bikes": 18,
                "docks": 6,
                "is_offline": False,
                "is_returning": True,
            },
            {
                "id": "655b0878-c22b-4f8c-8675-21bf30d1eebf",
                "name": "Willow & 12th",
                "full_name": "Willow Ave & 12 St",
                "walk_min": 6,
                "distance_m": 460,
                "ebikes": 4,
                "classic": 3,
                "total_bikes": 7,
                "docks": 8,
                "is_offline": False,
                "is_returning": True,
            },
            {
                "id": "519824e4-69ba-4270-a395-17c204f328f8",
                "name": "Washington & 8th",
                "full_name": "8 St & Washington St",
                "walk_min": 6,
                "distance_m": 462,
                "ebikes": 0,
                "classic": 14,
                "total_bikes": 14,
                "docks": 7,
                "is_offline": False,
                "is_returning": True,
            },
            {
                "id": "21c4a77d-b1ae-4262-b143-087fd14a07b8",
                "name": "Clinton & 7th",
                "full_name": "Clinton St & 7 St",
                "walk_min": 7,
                "distance_m": 556,
                "ebikes": 3,
                "classic": 5,
                "total_bikes": 8,
                "docks": 10,
                "is_offline": False,
                "is_returning": True,
            },
            {
                "id": "9d344652-976b-4c2d-bede-2ef19b0fbf13",
                "name": "Grand & 6th",
                "full_name": "6 St & Grand St",
                "walk_min": 10,
                "distance_m": 754,
                "ebikes": 1,
                "classic": 4,
                "total_bikes": 5,
                "docks": 13,
                "is_offline": False,
                "is_returning": True,
            },
        ]
        return sort_stations_by_ebike_priority(mock)
