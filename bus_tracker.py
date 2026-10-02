import os
import time
from typing import List, Dict, Any, Optional
import requests

try:
    from dotenv import load_dotenv
    load_dotenv()
except ImportError:
    pass

class NJTransitBusTracker:
    """
    Client for NJ Transit Bus DepartureVision (BUSDV2) API.
    Handles automated 24-hour token minting and renewal.
    """
    AUTH_URL = "https://pcsdata.njtransit.com/api/BUSDV2/authenticateUser"
    BUS_DV_URL = "https://pcsdata.njtransit.com/api/BUSDV2/getBusDV"

    def __init__(self, username: Optional[str] = None, password: Optional[str] = None):
        self.username = username or os.environ.get("NJT_USERNAME")
        self.password = password or os.environ.get("NJT_PASSWORD")
        self.token: Optional[str] = None
        self.token_expiry: float = 0
        self.session = requests.Session()

    def get_token(self) -> str:
        """
        Retrieves a valid token. If expired or not present, automatically
        authenticates with NJ Transit to obtain a new 24-hour token.
        """
        # Refresh 1 hour before the 24h expiry to be safe
        if not self.token or time.time() > self.token_expiry:
            if not self.username or not self.password:
                raise ValueError(
                    "Missing NJ Transit credentials. Please provide username and password, "
                    "or set NJT_USERNAME and NJT_PASSWORD in environment variables."
                )

            resp = self.session.post(
                self.AUTH_URL,
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

    def get_arrivals(self, stop_id: str, route: str = "126") -> List[Dict[str, Any]]:
        """
        Fetches upcoming bus arrivals for a specific stop number and route.
        """
        token = self.get_token()
        resp = self.session.post(
            self.BUS_DV_URL,
            data={
                "token": token,
                "stop": str(stop_id),
                "route": route,
                "direction": "",
                "IP": "",
            },
            timeout=10,
        )
        resp.raise_for_status()
        data = resp.json()
        trips = data.get("DVTrip") or []
        return trips

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
                arrivals.append({
                    "route": t.get("public_route"),
                    "destination": (t.get("header") or "").strip(),
                    "eta": t.get("departuretime"),
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

    username = os.environ.get("NJT_USERNAME", "YOUR_USERNAME")
    password = os.environ.get("NJT_PASSWORD", "YOUR_PASSWORD")

    if username == "YOUR_USERNAME":
        print("Note: Set NJT_USERNAME and NJT_PASSWORD in your environment or .env file.")
        print("Example usage when credentials are provided:")
        print("  python bus_tracker.py")
    else:
        tracker = NJTransitBusTracker(username, password)
        results = tracker.get_summary(STOPS_TO_TRACK, route="126")
        for name, arrivals in results.items():
            print(f"\n=== {name} ===")
            if not arrivals:
                print("  No buses reported in the next hour.")
            for a in arrivals:
                print(f"  [{a['route']}] {a['destination']} -> {a['eta']} (Load: {a['occupancy'] or 'N/A'})")
