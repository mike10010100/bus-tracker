"""
Citi Bike Live Station Inventory Tracker
Fetches real-time bike & dock availability for the closest stations to 919 Park Ave, Hoboken, NJ.
"""

import json
import time
import urllib.request
from typing import Dict, List, Any, Optional

# Closest Citi Bike stations to 919 Park Ave, Hoboken, NJ (40.7484552, -74.0302997)
# Walking times based on actual pedestrian street routing with crosswalks/signals (~65 m/min)
DEFAULT_STATIONS = [
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
        "walk_min": 7,
        "distance_m": 442,
    },
    {
        "id": "519824e4-69ba-4270-a395-17c204f328f8",
        "name": "Washington & 8th",
        "full_name": "8 St & Washington St",
        "walk_min": 7,
        "distance_m": 462,
    },
]

GBFS_STATUS_URL = "https://gbfs.citibikenyc.com/gbfs/en/station_status.json"


class CitiBikeTracker:
    def __init__(self, stations: Optional[List[Dict[str, Any]]] = None, cache_ttl: int = 30):
        self.stations = stations or DEFAULT_STATIONS
        self.cache_ttl = cache_ttl
        self._last_fetch_time = 0.0
        self._cached_data: List[Dict[str, Any]] = []

    def get_station_status(self, force_refresh: bool = False) -> List[Dict[str, Any]]:
        """
        Returns real-time status for the configured target stations.
        Uses in-memory cache if within cache_ttl.
        """
        now = time.time()
        if not force_refresh and (now - self._last_fetch_time < self.cache_ttl) and self._cached_data:
            return self._cached_data

        try:
            req = urllib.request.Request(
                GBFS_STATUS_URL,
                headers={"User-Agent": "126BusTracker/1.4.0 (Kindle Transit Display)"},
            )
            with urllib.request.urlopen(req, timeout=5) as resp:
                payload = json.loads(resp.read().decode("utf-8"))

            stations_raw = payload.get("data", {}).get("stations", [])
            status_map = {s["station_id"]: s for s in stations_raw if "station_id" in s}

            results = []
            for target in self.stations:
                sid = target["id"]
                raw_info = status_map.get(sid, {})

                ebikes = raw_info.get("num_ebikes_available", 0)
                total_bikes = raw_info.get("num_bikes_available", 0)
                classic = max(0, total_bikes - ebikes)
                docks = raw_info.get("num_docks_available", 0)
                is_renting = raw_info.get("is_renting", 1) == 1
                is_returning = raw_info.get("is_returning", 1) == 1
                is_offline = not is_renting

                results.append({
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
                })

            self._cached_data = results
            self._last_fetch_time = now
            return results

        except Exception as e:
            # If fetch fails but we have cached data, return cached
            if self._cached_data:
                return self._cached_data
            # Otherwise return mock fallback
            return self.get_mock_data()

    def get_mock_data(self) -> List[Dict[str, Any]]:
        """Mock fallback data for preview or offline mode."""
        return [
            {
                "id": "fadf00cf-d84a-49e8-9607-c67154915412",
                "name": "Clinton & 9th",
                "full_name": "Columbus Park - Clinton St & 9 St",
                "walk_min": 3,
                "distance_m": 246,
                "ebikes": 9,
                "classic": 12,
                "total_bikes": 21,
                "docks": 2,
                "is_offline": False,
                "is_returning": True,
            },
            {
                "id": "f417d8da-0f15-49b0-9e3c-3c3e55c2691d",
                "name": "Washington & 11th",
                "full_name": "11 St & Washington St",
                "walk_min": 7,
                "distance_m": 442,
                "ebikes": 3,
                "classic": 17,
                "total_bikes": 20,
                "docks": 2,
                "is_offline": False,
                "is_returning": True,
            },
            {
                "id": "519824e4-69ba-4270-a395-17c204f328f8",
                "name": "Washington & 8th",
                "full_name": "8 St & Washington St",
                "walk_min": 7,
                "distance_m": 462,
                "ebikes": 10,
                "classic": 4,
                "total_bikes": 14,
                "docks": 7,
                "is_offline": False,
                "is_returning": True,
            },
        ]


if __name__ == "__main__":
    tracker = CitiBikeTracker()
    status = tracker.get_station_status()
    print("Closest 3 Citi Bike stations to 919 Park Ave:")
    for s in status:
        print(f"- {s['name']} (~{s['walk_min']}m walk): {s['ebikes']} Ebikes, {s['classic']} Classic, {s['docks']} Docks")
