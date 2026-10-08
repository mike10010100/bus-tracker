"""Single source of truth for the application version.

The version is read from the VERSION file at the repository root. The Go
client receives the same value at build time via -ldflags (see Makefile and
Dockerfile), so VERSION is the authoritative identity for the whole project.
"""

import os

VERSION_FILE = os.path.join(os.path.dirname(__file__), "VERSION")


def get_version() -> str:
    """Returns the authoritative application version string."""
    try:
        with open(VERSION_FILE, "r") as f:
            return f.read().strip()
    except OSError:
        return "0.0.0"


VERSION = get_version()
