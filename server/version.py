"""Single source of truth for the application version.

The version is read from the VERSION file at the repository root. The Go
client receives the same value at build time via -ldflags (see Makefile and
Dockerfile), so VERSION is the authoritative identity for the whole project.
"""

import os

_local_version = os.path.join(os.path.dirname(__file__), "VERSION")
_parent_version = os.path.join(os.path.dirname(__file__), "..", "VERSION")
VERSION_FILE = _local_version if os.path.exists(_local_version) else _parent_version


def get_version() -> str:
    """Returns the authoritative application version string."""
    version_file = VERSION_FILE
    if not os.path.exists(version_file) and os.path.exists(_parent_version):
        version_file = _parent_version
    try:
        with open(version_file, encoding="utf-8") as f:
            return f.read().strip()
    except OSError:
        return "0.0.0"


VERSION = get_version()
