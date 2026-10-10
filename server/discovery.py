import socket
import sys
import threading
import ipaddress
from typing import Tuple, Any, Optional

try:
    from zeroconf import Zeroconf, ServiceInfo
    ZEROCONF_AVAILABLE = True
except ImportError:
    Zeroconf = None
    ServiceInfo = None
    ZEROCONF_AVAILABLE = False

from version import VERSION

DISCOVERY_PORT = 8001


def is_private_address(addr: str) -> bool:
    """Returns True for loopback, link-local and RFC1918 private addresses."""
    try:
        ip = ipaddress.ip_address(addr)
    except ValueError:
        return False
    return ip.is_loopback or ip.is_link_local or ip.is_private


def get_local_ip() -> str:
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
            s.connect(("8.8.8.8", 80))
            return str(s.getsockname()[0])
    except Exception:
        return "localhost"


def start_discovery_responder(http_port: int = 8000, version: str = VERSION) -> threading.Thread:
    """
    Listens on UDP 8001 for TRANSIT_TRACKER_DISCOVER broadcasts
    and replies with the server URL and version.
    """
    server_mod = sys.modules.get("server")
    sock_mod = getattr(server_mod, "socket", socket) if server_mod else socket
    disc_port = getattr(server_mod, "DISCOVERY_PORT", DISCOVERY_PORT) if server_mod else DISCOVERY_PORT

    def responder_loop() -> None:
        try:
            sock = sock_mod.socket(sock_mod.AF_INET, sock_mod.SOCK_DGRAM)
            sock.setsockopt(sock_mod.SOL_SOCKET, sock_mod.SO_REUSEADDR, 1)
            try:
                sock.setsockopt(sock_mod.SOL_SOCKET, sock_mod.SO_REUSEPORT, 1)
            except (AttributeError, OSError):
                pass
            sock.bind(("", disc_port))
            print(f"[Discovery] UDP broadcast responder active on port {disc_port}")
        except Exception as e:
            print(f"[Discovery] Could not bind UDP {disc_port}: {e}")
            return

        with sock:
            while True:
                try:
                    data, addr = sock.recvfrom(1024)
                    msg = data.decode("utf-8", errors="ignore").strip()
                    # Accept the legacy BUS_TRACKER_DISCOVER probe so devices running
                    # an older binary can still locate the server and OTA-upgrade.
                    if "TRANSIT_TRACKER_DISCOVER" in msg or "BUS_TRACKER_DISCOVER" in msg:
                        resp_ip = get_local_ip()
                        reply = f"TRANSIT_TRACKER_OFFER http://{resp_ip}:{http_port} {version}\n".encode("utf-8")
                        sock.sendto(reply, addr)
                        print(f"[Discovery] Answered probe from {addr[0]}:{addr[1]} -> http://{resp_ip}:{http_port}")
                except Exception:
                    pass

    t = threading.Thread(target=responder_loop, daemon=True, name="DiscoveryResponder")
    t.start()
    return t


def start_mdns_advertiser(http_port: int = 8000, version: str = VERSION) -> Tuple[Optional[Any], Optional[Any]]:
    """
    Registers _transittracker._tcp.local. service with Zeroconf / mDNS.
    """
    server_mod = sys.modules.get("server")
    zc_avail = getattr(server_mod, "ZEROCONF_AVAILABLE", ZEROCONF_AVAILABLE) if server_mod else ZEROCONF_AVAILABLE
    if not zc_avail:
        print("[mDNS] Zeroconf library not installed; skipping mDNS advertisement.")
        return None, None

    zc_cls = getattr(server_mod, "Zeroconf", Zeroconf) if server_mod and hasattr(server_mod, "Zeroconf") else Zeroconf
    sinfo_cls = getattr(server_mod, "ServiceInfo", ServiceInfo) if server_mod and hasattr(server_mod, "ServiceInfo") else ServiceInfo

    try:
        local_ip = get_local_ip()
        ip_bytes = socket.inet_aton(local_ip)
        service_type = "_transittracker._tcp.local."
        service_name = f"TransitTracker._transittracker._tcp.local."
        desc = {"version": version, "endpoint": "/dashboard.png"}

        info = sinfo_cls(
            service_type,
            service_name,
            addresses=[ip_bytes],
            port=http_port,
            properties=desc,
            server="transittracker.local.",
        )
        zc = zc_cls()
        zc.register_service(info)
        print(f"[mDNS] Registered service {service_name} at {local_ip}:{http_port}")
        return zc, info
    except Exception as e:
        print(f"[mDNS] Failed to register Zeroconf service: {e}")
        return None, None
