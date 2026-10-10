import os
import sys
import hashlib
import threading
from typing import NamedTuple, Optional


class BinaryInfo(NamedTuple):
    exists: bool
    mtime: float
    sha256: str
    size: int


_local_binary = os.path.join(os.path.dirname(__file__), "tracker-arm")
_parent_binary = os.path.join(os.path.dirname(__file__), "..", "tracker-arm")
BINARY_PATH = _local_binary if os.path.exists(_local_binary) else _parent_binary
_binary_info_cache = {"mtime": None, "sha256": "", "size": 0}
_binary_info_lock = threading.Lock()


def sha256_file(path: str) -> str:
    """Returns the lowercase hex SHA-256 digest of a file."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def get_binary_info(binary_path: Optional[str] = None) -> BinaryInfo:
    """
    Returns (exists, mtime, sha256, size) for the OTA binary, caching the
    (expensive) SHA-256 digest and keying the cache on mtime so repeated
    dashboard requests don't re-hash the 6MB binary every cycle.
    """
    if binary_path is None:
        server_mod = sys.modules.get("server")
        path = getattr(server_mod, "BINARY_PATH", BINARY_PATH) if server_mod else BINARY_PATH
    else:
        path = binary_path

    if not os.path.exists(path):
        if os.path.exists(_local_binary):
            path = _local_binary
        elif os.path.exists(_parent_binary):
            path = _parent_binary

    try:
        stat = os.stat(path)
    except OSError:
        return BinaryInfo(False, 0.0, "", 0)

    with _binary_info_lock:
        if _binary_info_cache["mtime"] == stat.st_mtime:
            return BinaryInfo(True, stat.st_mtime, _binary_info_cache["sha256"], _binary_info_cache["size"])
        digest = sha256_file(path)
        _binary_info_cache.update({"mtime": stat.st_mtime, "sha256": digest, "size": stat.st_size})
    return BinaryInfo(True, stat.st_mtime, _binary_info_cache["sha256"], _binary_info_cache["size"])
