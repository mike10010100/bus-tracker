import socket
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


def _usable_ip(addr: Any) -> Optional[str]:
    """Returns addr as a string if it is a concrete IPv4/IPv6 address."""
    try:
        ip = ipaddress.ip_address(str(addr))
    except ValueError:
        return None
    if ip.is_unspecified or ip.is_multicast:
        return None
    return str(ip)


def get_local_ip() -> Optional[str]:
    """
    Best-effort address of the default-route interface (for the startup banner
    and mDNS). Returns None, never "localhost", when there is no route.
    """
    try:
        with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as s:
            s.connect(("8.8.8.8", 80))
            return _usable_ip(s.getsockname()[0])
    except Exception:
        return None


def local_ip_for_peer(peer_ip: str, peer_port: int = DISCOVERY_PORT) -> Optional[str]:
    """
    Returns the local address the kernel would use to reach peer_ip, i.e. the
    interface the prober is on. Correct on multi-homed hosts and with no
    default route. A UDP connect() sends no packets.
    """
    try:
        family = socket.AF_INET6 if ":" in peer_ip else socket.AF_INET
        with socket.socket(family, socket.SOCK_DGRAM) as s:
            s.connect((peer_ip, peer_port or 9))
            return _usable_ip(s.getsockname()[0])
    except Exception:
        return None


def format_http_url(host: str, port: int) -> str:
    """Builds http://host:port, bracketing IPv6 literals."""
    if ":" in host:
        host = f"[{host}]"
    return f"http://{host}:{port}"


def start_discovery_responder(
    http_port: int = 8000,
    version: str = VERSION,
    port: Optional[int] = None,
) -> threading.Thread:
    """
    Listens on UDP 8001 for TRANSIT_TRACKER_DISCOVER broadcasts and replies
    with the server URL and version. The advertised host is the local address
    facing the prober; if it cannot be determined, no offer is sent.
    """
    disc_port = DISCOVERY_PORT if port is None else port

    def responder_loop() -> None:
        try:
            sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEPORT, 1)
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
                        resp_ip = local_ip_for_peer(addr[0], addr[1])
                        if not resp_ip:
                            print(f"[Discovery] No route back to {addr[0]}; not answering")
                            continue
                        url = format_http_url(resp_ip, http_port)
                        reply = f"TRANSIT_TRACKER_OFFER {url} {version}\n".encode("utf-8")
                        sock.sendto(reply, addr)
                        print(f"[Discovery] Answered probe from {addr[0]}:{addr[1]} -> {url}")
                except Exception:
                    pass

    t = threading.Thread(target=responder_loop, daemon=True, name="DiscoveryResponder")
    t.start()
    return t


def start_mdns_advertiser(http_port: int = 8000, version: str = VERSION) -> Tuple[Optional[Any], Optional[Any]]:
    """
    Registers _transittracker._tcp.local. service with Zeroconf / mDNS.
    """
    if not ZEROCONF_AVAILABLE:
        print("[mDNS] Zeroconf library not installed; skipping mDNS advertisement.")
        return None, None

    try:
        local_ip = get_local_ip()
        if not local_ip or ":" in local_ip:
            print("[mDNS] No usable IPv4 address; skipping mDNS advertisement.")
            return None, None
        ip_bytes = socket.inet_aton(local_ip)
        service_type = "_transittracker._tcp.local."
        service_name = f"TransitTracker._transittracker._tcp.local."
        desc = {"version": version, "endpoint": "/dashboard.png"}

        info = ServiceInfo(
            service_type,
            service_name,
            addresses=[ip_bytes],
            port=http_port,
            properties=desc,
            server="transittracker.local.",
        )
        zc = Zeroconf()
        zc.register_service(info)
        print(f"[mDNS] Registered service {service_name} at {local_ip}:{http_port}")
        return zc, info
    except Exception as e:
        print(f"[mDNS] Failed to register Zeroconf service: {e}")
        return None, None
