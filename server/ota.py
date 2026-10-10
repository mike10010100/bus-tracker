"""
OTA artifacts: the ``tracker-arm`` binary and its signed release manifest.

The server never holds the release key, so it does not verify the manifest
signature (the client does). It only checks that the manifest is well formed
and describes *exactly* the binary it would serve (sha256 + size), so it never
advertises an update for a different binary (security spec §1/§4).

The binary is read once per (inode, mtime, size) and kept in memory; the hash
is computed over those exact bytes and the download serves those same bytes, so
there is no window between hashing and serving (TOCTOU).
"""

import base64
import binascii
import hashlib
import json
import os
import re
import threading
from typing import Any, Dict, NamedTuple, Optional

from paths import artifact_candidates, find_artifact

BINARY_NAME = "tracker-arm"
MANIFEST_NAME = "tracker-arm.manifest.json"
MANIFEST_FORMAT = "transit-tracker-ota-v1"
# Matches the client's limits (§5): 32 MiB binary, 4 KiB manifest.
MAX_BINARY_SIZE = 32 * 1024 * 1024
MAX_MANIFEST_SIZE = 4096

_VERSION_RE = re.compile(r"\A[0-9]+\.[0-9]+\.[0-9]+\Z")
_SHA256_RE = re.compile(r"\A[0-9a-f]{64}\Z")


class BinaryInfo(NamedTuple):
    exists: bool
    mtime: float
    sha256: str
    size: int


class BinaryBlob(NamedTuple):
    """The exact bytes that are hashed and served."""
    path: str
    data: bytes
    mtime: float
    sha256: str
    size: int


class ManifestInfo(NamedTuple):
    raw: bytes      # served verbatim, as stored on disk
    version: str
    sha256: str
    size: int


class ManifestError(ValueError):
    """Raised when a manifest is malformed."""


# Search order for the binary (server/ first, then the repo root). Tests point
# this at a temporary directory.
BINARY_SEARCH_PATHS = artifact_candidates(BINARY_NAME)
BINARY_PATH = find_artifact(BINARY_NAME) or BINARY_SEARCH_PATHS[-1]

_cache_lock = threading.Lock()
_blob_cache: Dict[str, Any] = {"key": None, "blob": None}
_manifest_cache: Dict[str, Any] = {"key": None, "info": None}


def sha256_file(path: str) -> str:
    """Returns the lowercase hex SHA-256 digest of a file."""
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest()


def find_binary_path(binary_path: Optional[str] = None) -> Optional[str]:
    """Resolves the binary to serve: an explicit path, else the search order."""
    if binary_path:
        return binary_path if os.path.isfile(binary_path) else None
    for path in BINARY_SEARCH_PATHS:
        if os.path.isfile(path):
            return path
    return None


def _stat_key(st: os.stat_result) -> tuple:
    return (st.st_dev, st.st_ino, st.st_mtime_ns, st.st_size)


def clear_caches() -> None:
    """Drops the cached binary and manifest (used by tests)."""
    with _cache_lock:
        _blob_cache.update({"key": None, "blob": None})
        _manifest_cache.update({"key": None, "info": None})


def load_binary(binary_path: Optional[str] = None) -> Optional[BinaryBlob]:
    """
    Returns the binary's bytes and digest, cached by (inode, mtime, size).
    The digest is computed under the lock over the very bytes that will be
    served. Returns None when the binary is missing, oversized, or changed
    while being read.
    """
    path = find_binary_path(binary_path)
    if path is None:
        return None
    with _cache_lock:
        try:
            st = os.stat(path)
        except OSError:
            return None
        if _blob_cache["key"] == (path,) + _stat_key(st):
            return _blob_cache["blob"]
        try:
            with open(path, "rb") as f:
                fst = os.fstat(f.fileno())
                if fst.st_size > MAX_BINARY_SIZE:
                    print(f"[OTA] {path} is larger than {MAX_BINARY_SIZE} bytes; not serving it")
                    return None
                data = f.read(fst.st_size + 1)
        except OSError:
            return None
        if len(data) != fst.st_size:
            # The file is being rewritten in place; try again on the next request.
            print(f"[OTA] {path} changed while being read; not serving it this time")
            return None
        blob = BinaryBlob(path, data, fst.st_mtime, hashlib.sha256(data).hexdigest(), len(data))
        _blob_cache.update({"key": (path,) + _stat_key(fst), "blob": blob})
        return blob


def get_binary_info(binary_path: Optional[str] = None) -> BinaryInfo:
    """
    Returns (exists, mtime, sha256, size) for the OTA binary, caching the
    (expensive) SHA-256 digest by mtime and size so repeated dashboard
    requests don't re-hash the 6MB binary every cycle.
    """
    blob = load_binary(binary_path)
    if blob is None:
        return BinaryInfo(False, 0.0, "", 0)
    return BinaryInfo(True, blob.mtime, blob.sha256, blob.size)


def parse_manifest(raw: bytes) -> ManifestInfo:
    """Parses and structurally validates a release manifest (§1)."""
    if len(raw) > MAX_MANIFEST_SIZE:
        raise ManifestError(f"manifest is larger than {MAX_MANIFEST_SIZE} bytes")
    try:
        doc = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, ValueError) as e:
        raise ManifestError(f"manifest is not valid JSON ({e})") from e
    if not isinstance(doc, dict):
        raise ManifestError("manifest is not a JSON object")
    if doc.get("format") != MANIFEST_FORMAT:
        raise ManifestError(f"manifest format is not {MANIFEST_FORMAT!r}")
    version = doc.get("version")
    if not isinstance(version, str) or not _VERSION_RE.match(version):
        raise ManifestError("manifest version is not MAJOR.MINOR.PATCH")
    digest = doc.get("sha256")
    if not isinstance(digest, str) or not _SHA256_RE.match(digest):
        raise ManifestError("manifest sha256 is not 64 lowercase hex characters")
    size = doc.get("size")
    if not isinstance(size, int) or isinstance(size, bool) or not (0 < size <= MAX_BINARY_SIZE):
        raise ManifestError("manifest size is not a positive integer within limits")
    sig = doc.get("signature")
    try:
        if not isinstance(sig, str) or len(base64.b64decode(sig.encode("ascii"), validate=True)) != 64:
            raise ManifestError("manifest signature is not base64 of 64 bytes")
    except (binascii.Error, UnicodeEncodeError) as e:
        raise ManifestError(f"manifest signature is not valid base64 ({e})") from e
    return ManifestInfo(raw, version, digest, size)


def manifest_path_for(binary_path: str) -> str:
    """The manifest always sits next to the binary it describes."""
    return os.path.join(os.path.dirname(binary_path), MANIFEST_NAME)


def get_valid_manifest(binary_path: Optional[str] = None) -> Optional[ManifestInfo]:
    """
    Returns the manifest only if it is well formed AND its sha256/size match
    the binary that /tracker-arm currently serves. Otherwise None. The result
    is cached by the manifest's and binary's (inode, mtime, size).
    """
    blob = load_binary(binary_path)
    if blob is None:
        return None
    mpath = manifest_path_for(blob.path)
    try:
        st = os.stat(mpath)
    except OSError:
        return None
    key = (mpath,) + _stat_key(st) + (blob.sha256, blob.size)
    with _cache_lock:
        if _manifest_cache["key"] == key:
            return _manifest_cache["info"]
    info: Optional[ManifestInfo] = None
    try:
        with open(mpath, "rb") as f:
            raw = f.read(MAX_MANIFEST_SIZE + 1)
        parsed = parse_manifest(raw)
        if parsed.sha256 != blob.sha256 or parsed.size != blob.size:
            raise ManifestError(
                f"manifest describes sha256={parsed.sha256[:12]}… size={parsed.size}, "
                f"but {blob.path} is sha256={blob.sha256[:12]}… size={blob.size}"
            )
        info = parsed
    except (OSError, ManifestError) as e:
        print(f"[OTA] Ignoring {mpath}: {e}. OTA is not advertised.")
    with _cache_lock:
        _manifest_cache.update({"key": key, "info": info})
    return info
