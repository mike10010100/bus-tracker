"""Helpers that keep secrets and attacker-controlled bytes out of the logs."""

import re
from typing import Any

# `token=<value>` in URLs, form bodies and requests' exception messages.
_TOKEN_RE = re.compile(r"(?i)(token=)[^&\s'\"<>]+")
# C0 controls, DEL and C1 controls (incl. the ESC that starts ANSI sequences).
_CONTROL_RE = re.compile(r"[\x00-\x1f\x7f-\x9f]")


def redact(value: Any) -> str:
    """Returns str(value) with every `token=...` value replaced by REDACTED."""
    return _TOKEN_RE.sub(r"\1REDACTED", str(value))


def strip_controls(text: str) -> str:
    """Removes control characters (CR, LF, ESC, ...) from a single line."""
    return _CONTROL_RE.sub("", text)


def safe_log_lines(text: str, max_lines: int = 200) -> list[str]:
    """
    Splits untrusted text into lines and strips control characters from each,
    so a device-supplied body cannot forge log lines or inject ANSI escapes.
    Empty lines are dropped.
    """
    lines = []
    for raw in text.splitlines():
        line = strip_controls(raw).strip()
        if line:
            lines.append(line)
        if len(lines) >= max_lines:
            lines.append("... (truncated)")
            break
    return lines
