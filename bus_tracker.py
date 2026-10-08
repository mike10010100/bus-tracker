import os
import time
from typing import List, Dict, Any, Optional
import requests

try:
    from dotenv import load_dotenv
    load_dotenv()
except ImportError:
    # Native fallback if python-dotenv is not installed
    env_file = os.path.join(os.path.dirname(__file__), ".env")
    if os.path.exists(env_file):
        with open(env_file, "r") as f:
            for line in f:
                line = line.strip()
                if line and not line.startswith("#") and "=" in line:
                    k, v = line.split("=", 1)
                    k = k.strip()
                    v = v.strip().strip("'\"")
                    if k not in os.environ:
                        os.environ[k] = v

class NJTransitBusTracker:
    """
    Client for NJ Transit Bus DepartureVision (BUSDV2) API.
    Handles automated 24-hour token minting and renewal.
    """
    DEFAULT_BASE_URL = os.environ.get("NJT_BASE_URL", "https://testpcsdata.njtransit.com")

    def __init__(self, username: Optional[str] = None, password: Optional[str] = None, base_url: Optional[str] = None):
        raw_user = username or os.environ.get("NJT_USERNAME") or os.environ.get("NJT_API_USERNAME") or ""
        # NJ Transit API requires username handle, not email
        self.username = raw_user.split("@")[0] if "@" in raw_user else raw_user
        self.password = password or os.environ.get("NJT_PASSWORD") or os.environ.get("NJT_API_PASSWORD")
        self.base_url = (base_url or self.DEFAULT_BASE_URL).rstrip("/")
        self.token: Optional[str] = None
        self.token_expiry: float = 0
        self.session = requests.Session()

    @property
    def auth_url(self) -> str:
        return f"{self.base_url}/api/BUSDV2/authenticateUser"

    @property
    def bus_dv_url(self) -> str:
        return f"{self.base_url}/api/BUSDV2/getBusDV"

    def get_token(self) -> str:
        """
        Retrieves a valid token. If expired or not present, automatically
        authenticates with NJ Transit to obtain a new 24-hour token.
        """
        if not self.token or time.time() > self.token_expiry:
            if not self.username or not self.password:
                raise ValueError(
                    "Missing NJ Transit credentials. Please provide username and password, "
                    "or set NJT_USERNAME and NJT_PASSWORD in environment variables."
                )

            resp = self.session.post(
                self.auth_url,
                data={"username": self.username, "password": self.password},
                timeout=10,
            )
            resp.raise_for_status()
            data = resp.json()

            if str(data.get("Authenticated")).lower() == "true" and data.get("UserToken"):
                self.token = data["UserToken"]
                # 23 hours in seconds = 82800
                self.token_expiry = time.time() + 82800
            else:
                raise RuntimeError(f"NJ Transit authentication failed: {data}")

        return self.token

    GRAPHQL_URL = "https://www.njtransit.com/api/graphql/graphql"
    GRAPHQL_QUERY = """
    query BusArrivalsByStopID($stopID: ID!) {
      getBusArrivalsByStopID(stopID: $stopID) {
        publicRoute
        header
        lanegate
        departuretime
        departurestatus
        schedDepTime
        vehicleId
        passload
      }
    }
    """

    def get_arrivals_graphql(self, stop_id: str, route: str = "126") -> List[Dict[str, Any]]:
        """
        Fetches live arrivals via NJ Transit's public web GraphQL API.
        No token or authentication required; highly reliable fallback.
        """
        resp = self.session.post(
            self.GRAPHQL_URL,
            json={
                "operationName": "BusArrivalsByStopID",
                "variables": {"stopID": str(stop_id)},
                "query": self.GRAPHQL_QUERY,
            },
            headers={"User-Agent": "Mozilla/5.0"},
            timeout=10,
        )
        resp.raise_for_status()
        data = resp.json().get("data", {}).get("getBusArrivalsByStopID") or []
        results = []
        for item in data:
            pub_route = item.get("publicRoute") or ""
            if route and pub_route.strip() != route.strip():
                continue
            results.append({
                "public_route": pub_route,
                "header": item.get("header"),
                "lanegate": item.get("lanegate"),
                "departuretime": item.get("departuretime"),
                "departurestatus": item.get("departurestatus"),
                "sched_dep_time": item.get("schedDepTime"),
                "vehicle_id": item.get("vehicleId"),
                "passload": item.get("passload"),
            })
        return results

    def get_arrivals(self, stop_id: str, route: str = "126") -> List[Dict[str, Any]]:
        """
        Fetches upcoming bus arrivals for a specific stop number and route.
        Tries Developer BUSDV2 API first, automatically falling back to
        the public GraphQL API if the developer server is in maintenance.
        """
        try:
            token = self.get_token()
            resp = self.session.post(
                self.bus_dv_url,
                data={
                    "token": token,
                    "stop": str(stop_id),
                    "route": route,
                    "direction": "",
                    "IP": "",
                },
                timeout=8,
            )
            resp.raise_for_status()
            data = resp.json()
            trips = data.get("DVTrip") or []
            return trips
        except Exception as e:
            # Automatic fallback to official website GraphQL API
            try:
                return self.get_arrivals_graphql(stop_id=stop_id, route=route)
            except Exception as e2:
                print(f"[Tracker] Both BUSDV2 ({e}) and GraphQL ({e2}) failed.")
                return []

    def get_summary(self, stops: Dict[str, str], route: str = "126") -> Dict[str, List[Dict[str, str]]]:
        """
        Fetches simplified arrival summaries for multiple stops.
        stops format: {"Stop Name": "5-digit-stop-id"}
        """
        summary = {}
        for stop_name, stop_id in stops.items():
            trips = self.get_arrivals(stop_id=stop_id, route=route)
            arrivals = []
            for t in trips:
                status = (t.get("departurestatus") or "").strip()
                dep_time = (t.get("departuretime") or "").strip()
                eta_str = f"{status} ({dep_time})" if status and dep_time else (status or dep_time or "Scheduled")

                arrivals.append({
                    "route": t.get("public_route"),
                    "destination": (t.get("header") or "").strip(),
                    "eta": eta_str,
                    "occupancy": t.get("passload"),
                    "vehicle_id": t.get("vehicle_id"),
                })
            summary[stop_name] = arrivals
        return summary


if __name__ == "__main__":
    # Test target stops in Hoboken for Route 126 to NYC
    STOPS_TO_TRACK = {
        "Washington St at 9th St (Stop #20512)": "20512",
        "Clinton St at 9th St (Stop #20494)": "20494",
    }

    tracker = NJTransitBusTracker()
    try:
        results = tracker.get_summary(STOPS_TO_TRACK, route="126")
        for name, arrivals in results.items():
            print(f"\n=== {name} ===")
            if not arrivals:
                print("  No buses reported in the next hour.")
            for a in arrivals:
                load = f" [Occupancy: {a['occupancy']}]" if a['occupancy'] and a['occupancy'] != "EMPTY" else ""
                bus_num = f" (Bus #{a['vehicle_id']})" if a['vehicle_id'] else ""
                print(f"  [{a['route']}] {a['destination']} -> {a['eta']}{bus_num}{load}")
    except ValueError as e:
        print(f"Setup error: {e}")
    except Exception as e:
        print(f"Error fetching arrivals: {e}")
