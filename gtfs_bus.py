"""
NJ Transit GTFS-BUS client.

Unlike the BUSDATA/DepartureVision product (which can be blocked by a backend
provisioning issue), the GTFS-G2 endpoints work for any account subscribed to
GTFS-BUS. We use two of them:

  * getGTFS        -> the static schedule (a zip of standard GTFS CSVs). This is
                      the source of the FULL list of upcoming departures, not
                      just the one active vehicle the public website returns.
  * getTripUpdates -> GTFS-Realtime (protobuf) trip predictions, merged onto the
                      static schedule by (trip_id, stop_id) for live ETAs.

The static feed only covers a short rolling window (a handful of days), so it is
re-downloaded periodically; the derived, small index is cached to disk so a
restart does not re-parse the whole state's stop_times.txt.
"""

import csv
import datetime
import io
import json
import os
import time
import zipfile
from typing import Any, Dict, List, Optional

import requests

try:
    from google.transit import gtfs_realtime_pb2

    _REALTIME_AVAILABLE = True
except Exception:  # pragma: no cover - exercised only when the dep is missing
    _REALTIME_AVAILABLE = False

_DAY_KEYS = ["monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"]

# How often the static schedule is re-downloaded. The upstream window is only a
# few days, so refresh daily.
STATIC_TTL = 20 * 3600
# Keep only departures within this window of "now" (a little slack behind, an
# hour or two ahead) -- enough for a commute board.
LOOKBACK_SECS = 120
LOOKAHEAD_SECS = 3 * 3600


def hms_to_secs(value: str) -> int:
    """Parses a GTFS HH:MM:SS time (may exceed 24:00:00) into seconds."""
    try:
        h, m, s = (int(x) for x in value.strip().split(":"))
        return h * 3600 + m * 60 + s
    except Exception:
        return -1


def format_clock(epoch: float) -> str:
    """Formats a unix epoch as a local 'H:MM AM/PM' clock string."""
    dt = datetime.datetime.fromtimestamp(epoch)
    return dt.strftime("%I:%M %p").lstrip("0")


def format_eta(epoch: float, now: float) -> str:
    """Human ETA like 'in 5 mins (5:35 PM)' matching the renderer's parser."""
    mins = int(round((epoch - now) / 60.0))
    if mins < 0:
        mins = 0
    return f"in {mins} mins ({format_clock(epoch)})"


class GTFSBusTracker:
    """Fetches and merges NJ Transit GTFS-BUS static schedule + realtime."""

    BASE_URL = os.environ.get("NJT_GTFS_BASE_URL", "https://pcsdata.njtransit.com").rstrip("/")
    AUTH_PATH = "/api/GTFSG2/authenticateUser"
    STATIC_PATH = "/api/GTFSG2/getGTFS"
    TRIPS_PATH = "/api/GTFSG2/getTripUpdates"

    def __init__(
        self,
        route: str = "126",
        stops: Optional[List[str]] = None,
        username: Optional[str] = None,
        password: Optional[str] = None,
        base_url: Optional[str] = None,
        cache_dir: str = "/app/cache",
        session: Optional[requests.Session] = None,
    ):
        raw_user = username or os.environ.get("NJT_USERNAME") or os.environ.get("NJT_API_USERNAME") or ""
        self.username = raw_user.split("@")[0] if "@" in raw_user else raw_user
        self.password = password or os.environ.get("NJT_PASSWORD") or os.environ.get("NJT_API_PASSWORD") or ""
        self.route = route
        self.stops = list(stops or [])
        self.base_url = (base_url or self.BASE_URL).rstrip("/")
        self.cache_dir = cache_dir
        self.session = session or requests.Session()

        self.token: Optional[str] = None
        self.token_expiry: float = 0
        self._index: Optional[Dict[str, Any]] = None
        self._realtime: Optional[Dict[str, Dict[str, Any]]] = None
        self._realtime_at: float = 0

    # -- URLs --------------------------------------------------------------
    @property
    def auth_url(self) -> str:
        return self.base_url + self.AUTH_PATH

    @property
    def static_url(self) -> str:
        return self.base_url + self.STATIC_PATH

    @property
    def trips_url(self) -> str:
        return self.base_url + self.TRIPS_PATH

    @property
    def _index_path(self) -> str:
        return os.path.join(self.cache_dir, "gtfs_index.json")

    # -- Auth --------------------------------------------------------------
    def get_token(self) -> str:
        if self.token and time.time() < self.token_expiry:
            return self.token
        if not self.username or not self.password:
            raise ValueError("Missing NJ Transit credentials (NJT_USERNAME/NJT_PASSWORD).")
        resp = self.session.post(
            self.auth_url,
            data={"username": self.username, "password": self.password},
            timeout=10,
        )
        resp.raise_for_status()
        data = resp.json()
        if str(data.get("Authenticated")).lower() == "true" and data.get("UserToken"):
            self.token = data["UserToken"]
            self.token_expiry = time.time() + 82800  # 23h
            return self.token
        raise RuntimeError(f"NJ Transit GTFS auth failed: {data}")

    # -- Static schedule ---------------------------------------------------
    def _index_is_fresh(self, index: Dict[str, Any]) -> bool:
        if not index or index.get("route") != self.route:
            return False
        if time.time() - index.get("built_at", 0) > STATIC_TTL:
            return False
        valid_until = index.get("valid_until", "")
        return bool(valid_until) and datetime.date.today().strftime("%Y%m%d") <= valid_until

    def _load_cached_index(self) -> Optional[Dict[str, Any]]:
        try:
            with open(self._index_path, "r") as f:
                index = json.load(f)
            if index.get("stops") == self.stops and self._index_is_fresh(index):
                return index
        except Exception:
            pass
        return None

    def _save_index(self, index: Dict[str, Any]) -> None:
        try:
            os.makedirs(self.cache_dir, exist_ok=True)
            tmp = self._index_path + ".tmp"
            with open(tmp, "w") as f:
                json.dump(index, f)
            os.replace(tmp, self._index_path)
        except Exception:
            pass

    def ensure_index(self) -> Optional[Dict[str, Any]]:
        if self._index is not None:
            return self._index
        cached = self._load_cached_index()
        if cached is not None:
            self._index = cached
            return self._index
        try:
            self.get_token()
            resp = self.session.get(self.static_url, params={"token": self.token}, timeout=180)
            resp.raise_for_status()
            self._index = self.build_index(resp.content)
            self._save_index(self._index)
            return self._index
        except Exception as e:
            print(f"[GTFS] static schedule unavailable ({e})")
            return None

    def build_index(self, zip_bytes: bytes) -> Dict[str, Any]:
        """Parses the static GTFS zip into a small index for this route/stops."""
        z = zipfile.ZipFile(io.BytesIO(zip_bytes))

        services: Dict[str, Any] = {}
        with z.open("calendar.txt") as f:
            for r in csv.DictReader(io.TextIOWrapper(f, "utf-8")):
                services[r["service_id"]] = {
                    "days": [r[k] for k in _DAY_KEYS],
                    "start": r["start_date"],
                    "end": r["end_date"],
                }

        exceptions: Dict[str, Dict[str, int]] = {}
        try:
            with z.open("calendar_dates.txt") as f:
                for r in csv.DictReader(io.TextIOWrapper(f, "utf-8")):
                    exceptions.setdefault(r["date"], {})[r["service_id"]] = int(r["exception_type"])
        except KeyError:
            pass

        route_ids = set()
        with z.open("routes.txt") as f:
            for r in csv.DictReader(io.TextIOWrapper(f, "utf-8")):
                if r.get("route_short_name") == self.route:
                    route_ids.add(r["route_id"])

        trips: Dict[str, Any] = {}
        with z.open("trips.txt") as f:
            for r in csv.DictReader(io.TextIOWrapper(f, "utf-8")):
                if r["route_id"] in route_ids:
                    trips[r["trip_id"]] = {
                        "headsign": (r.get("trip_headsign") or "").strip(),
                        "service_id": r.get("service_id", ""),
                        "direction": r.get("direction_id", ""),
                    }

        deps: Dict[str, List[Dict[str, Any]]] = {sid: [] for sid in self.stops}
        with z.open("stop_times.txt") as f:
            for r in csv.DictReader(io.TextIOWrapper(f, "utf-8")):
                tid = r["trip_id"]
                sid = r["stop_id"]
                if tid in trips and sid in deps:
                    secs = hms_to_secs(r.get("departure_time") or r.get("arrival_time") or "")
                    if secs >= 0:
                        deps[sid].append({"trip_id": tid, "secs": secs})
        for sid in deps:
            deps[sid].sort(key=lambda d: d["secs"])

        valid_until = max((s["end"] for s in services.values()), default="")
        return {
            "route": self.route,
            "stops": self.stops,
            "built_at": time.time(),
            "valid_until": valid_until,
            "trips": trips,
            "deps": deps,
            "services": services,
            "exceptions": exceptions,
        }

    def active_services(self, index: Dict[str, Any], date: datetime.date) -> set:
        ymd = date.strftime("%Y%m%d")
        dow = date.weekday()  # 0 = Monday
        active = set()
        for sid, s in index["services"].items():
            if s["start"] <= ymd <= s["end"] and s["days"][dow] == "1":
                active.add(sid)
        for sid, typ in index["exceptions"].get(ymd, {}).items():
            if typ == 1:
                active.add(sid)
            elif typ == 2:
                active.discard(sid)
        return active

    # -- Realtime ----------------------------------------------------------
    def fetch_realtime(self) -> Dict[str, Dict[str, Any]]:
        """Fetches GTFS-RT trip updates, keyed trip_id -> stop_id -> {time,delay,vehicle_id}."""
        index = self._index or {}
        known = index.get("trips", {})
        out: Dict[str, Dict[str, Any]] = {}
        if not _REALTIME_AVAILABLE:
            print("[GTFS] gtfs-realtime-bindings unavailable; skipping realtime.")
            return out
        try:
            self.get_token()
            resp = self.session.get(self.trips_url, params={"token": self.token}, timeout=60)
            resp.raise_for_status()
            feed = gtfs_realtime_pb2.FeedMessage()
            feed.ParseFromString(resp.content)
        except Exception as e:
            print(f"[GTFS] realtime unavailable ({e})")
            return out

        for entity in feed.entity:
            if not entity.HasField("trip_update"):
                continue
            tu = entity.trip_update
            tid = tu.trip.trip_id
            if known and tid not in known:
                continue
            vid = tu.vehicle.id if tu.HasField("vehicle") and tu.vehicle.id else None
            for stu in tu.stop_time_update:
                sid = stu.stop_id
                if self.stops and sid not in self.stops:
                    continue
                when = None
                delay = None
                if stu.HasField("departure") and stu.departure.HasField("time"):
                    when = stu.departure.time
                    if stu.departure.HasField("delay"):
                        delay = stu.departure.delay
                elif stu.HasField("arrival") and stu.arrival.HasField("time"):
                    when = stu.arrival.time
                    if stu.arrival.HasField("delay"):
                        delay = stu.arrival.delay
                out.setdefault(tid, {})[sid] = {"time": when, "delay": delay, "vehicle_id": vid}
        return out

    # -- Query -------------------------------------------------------------
    def get_upcoming(self, stop_id: str, limit: int = 3, allow_realtime: bool = True,
                     now: Optional[datetime.datetime] = None) -> List[Dict[str, Any]]:
        """
        Returns the next `limit` upcoming departures for a stop, merging the
        static schedule with realtime predictions. Each item match the canonical
        Arrival shape the renderer consumes.
        """
        index = self.ensure_index()
        if not index:
            return []
        now = now or datetime.datetime.now()
        now_epoch = now.timestamp()
        now_secs = now.hour * 3600 + now.minute * 60 + now.second
        midnight = datetime.datetime.combine(now.date(), datetime.time(0, 0))
        active = self.active_services(index, now.date())

        realtime: Dict[str, Dict[str, Any]] = {}
        if allow_realtime:
            if self._realtime is not None and time.time() - self._realtime_at < 15:
                realtime = self._realtime
            else:
                realtime = self.fetch_realtime()
                self._realtime = realtime
                self._realtime_at = time.time()

        # Dedup by the *scheduled* (minute, headsign, direction): the static feed
        # lists the same physical departure under multiple trip_ids (service-day
        # variants), and realtime can shift one copy's time, so keying on the
        # scheduled time collapses them. Prefer the copy that has realtime data.
        by_key: Dict[tuple, Dict[str, Any]] = {}
        for d in index["deps"].get(stop_id, []):
            trip = index["trips"].get(d["trip_id"])
            if not trip or trip["service_id"] not in active:
                continue
            secs = d["secs"]
            if secs < now_secs - LOOKBACK_SECS or secs > now_secs + LOOKAHEAD_SECS:
                continue
            sched_epoch = (midnight + datetime.timedelta(seconds=secs)).timestamp()
            pred = (realtime.get(d["trip_id"]) or {}).get(stop_id) if realtime else None
            if pred and pred.get("time"):
                epoch, live = float(pred["time"]), True
            else:
                epoch, live = sched_epoch, False
            key = (secs // 60, trip["headsign"], trip["direction"])
            row = {
                "trip_id": d["trip_id"],
                "epoch": epoch,
                "live": live,
                "direction": trip["direction"],
                "headsign": trip["headsign"],
                "vehicle_id": (pred or {}).get("vehicle_id"),
            }
            if key not in by_key or (live and not by_key[key]["live"]):
                by_key[key] = row

        rows = sorted(by_key.values(), key=lambda r: r["epoch"])
        result: List[Dict[str, Any]] = []
        for r in rows:
            result.append({
                "route": self.route,
                "destination": r["headsign"] or f"{self.route} bus",
                "eta": format_eta(r["epoch"], now_epoch),
                "occupancy": None,
                "vehicle_id": r["vehicle_id"],
                "live": r["live"],
            })
            if len(result) >= limit:
                break
        return result
