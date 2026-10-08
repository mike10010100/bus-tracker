import os
import re
import sys
import time
from datetime import datetime
from typing import Dict, List, Any, Optional
from PIL import Image, ImageDraw, ImageFont

from bus_tracker import NJTransitBusTracker, normalize_arrival
from citibike import CitiBikeTracker

# Design-space canvas dimensions. All layout math in this module is expressed
# in this logical 800px-wide coordinate space; ScaledDraw translates it to the
# actual (possibly much larger) native canvas so we never resample a bitmap.
WIDTH = 800
HEIGHT = 480


class ScaledDraw:
    """
    A transparent ImageDraw proxy that renders a logical-coordinate layout onto
    a larger native canvas.

    Every view function draws in the 800px logical design space. Wrapping the
    real ImageDraw in this proxy lets us render at the panel's true resolution
    (e.g. 1648x1236 for a Kindle PW5) with crisp native text instead of
    LANCZOS-upscaling an 800px bitmap.

    Coordinates, outline widths and corner radii are multiplied by `scale`.
    Fonts are reconstructed at `size * scale` (cached) so glyphs are rendered
    natively. `textbbox` divides its result back into logical units so callers
    (and the ellipsize helper) keep operating in one consistent space.
    """

    def __init__(self, draw: ImageDraw.ImageDraw, scale: float = 1.0):
        self._d = draw
        self._s = float(scale)
        self._font_cache = {}

    # -- helpers -----------------------------------------------------------
    def _pt(self, p):
        return (p[0] * self._s, p[1] * self._s)

    def _box(self, xy):
        """Accepts [(x0,y0),(x1,y1)] or flat [x0,y0,x1,y1] and scales it."""
        if len(xy) == 4 and not hasattr(xy[0], "__len__"):
            return [xy[0] * self._s, xy[1] * self._s, xy[2] * self._s, xy[3] * self._s]
        return [self._pt(xy[0]), self._pt(xy[1])]

    def _outline(self, w):
        if w is None:
            return None
        return max(1, int(round(w * self._s)))

    def _font(self, font):
        if font is None or self._s == 1.0:
            return font
        path = getattr(font, "_transit_path", None)
        size = getattr(font, "_transit_size", None)
        if path is None or size is None:
            return font
        key = (path, size)
        cached = self._font_cache.get(key)
        if cached is not None:
            return cached
        try:
            scaled = ImageFont.truetype(path, max(1, int(round(size * self._s))))
            try:
                scaled._transit_path = path
                scaled._transit_size = max(1, int(round(size * self._s)))
            except Exception:
                pass
        except Exception:
            scaled = font
        self._font_cache[key] = scaled
        return scaled

    # -- drawing primitives ------------------------------------------------
    def text(self, xy, text, fill=None, font=None, **kwargs):
        return self._d.text(self._pt(xy), text, fill=fill, font=self._font(font), **kwargs)

    def textbbox(self, xy, text, font=None, **kwargs):
        bbox = self._d.textbbox(self._pt(xy), text, font=self._font(font), **kwargs)
        return tuple(v / self._s for v in bbox)

    def textlength(self, text, font=None, **kwargs):
        return self._d.textlength(text, font=self._font(font), **kwargs) / self._s

    def line(self, xy, fill=None, width=1, **kwargs):
        return self._d.line([self._pt(p) for p in xy], fill=fill, width=self._outline(width), **kwargs)

    def rectangle(self, xy, fill=None, outline=None, width=1, **kwargs):
        return self._d.rectangle(self._box(xy), fill=fill, outline=outline, width=self._outline(width), **kwargs)

    def rounded_rectangle(self, xy, radius=0, fill=None, outline=None, width=1, **kwargs):
        return self._d.rounded_rectangle(
            self._box(xy),
            radius=max(0, int(round(radius * self._s))),
            fill=fill,
            outline=outline,
            width=self._outline(width),
            **kwargs,
        )

    def polygon(self, xy, fill=None, outline=None, width=1, **kwargs):
        return self._d.polygon([self._pt(p) for p in xy], fill=fill, outline=outline, width=self._outline(width), **kwargs)

STOPS = [
    {
        "id": "20512",
        "name": "Washington & 9th",
        "subtitle": "Stop #20512 • via Lincoln Tunnel",
        "walk_min": 5,
    },
    {
        "id": "20494",
        "name": "Clinton & 9th",
        "subtitle": "Stop #20494 • via Clinton Ave",
        "walk_min": 3,
    },
]


def get_font(size: int, bold: bool = False):
    """
    Attempts to load a clean sans-serif system font (Arial/Helvetica),
    falling back gracefully to Pillow's default font.
    """
    font_paths = [
        # Linux / Docker paths (fonts-dejavu-core)
        "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf" if bold else "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
        "/usr/share/fonts/truetype/liberation/LiberationSans-Bold.ttf" if bold else "/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
        "/usr/share/fonts/truetype/freefont/FreeSansBold.ttf" if bold else "/usr/share/fonts/truetype/freefont/FreeSans.ttf",
        # macOS paths
        "/System/Library/Fonts/Supplemental/Arial Bold.ttf" if bold else "/System/Library/Fonts/Supplemental/Arial.ttf",
        "/System/Library/Fonts/Helvetica.ttc",
        "/System/Library/Fonts/Supplemental/Trebuchet MS.ttf",
        "/System/Library/Fonts/SFNS.ttf",
        "/Library/Fonts/Arial.ttf",
    ]
    for p in font_paths:
        if os.path.exists(p):
            try:
                font = ImageFont.truetype(p, size)
                # Tag so ScaledDraw can reconstruct this font at native size.
                font._transit_path = p
                font._transit_size = size
                return font
            except Exception:
                continue
    try:
        font = ImageFont.load_default()
        try:
            font._transit_path = getattr(font, "path", None)
            font._transit_size = size
        except Exception:
            pass
        return font
    except Exception:
        return None


# Arrival fetch statuses mirrored from bus_tracker.NJTransitBusTracker
STATUS_OK = "ok"
STATUS_EMPTY = "empty"
STATUS_ERROR = "error"

_NO_BUS_MESSAGE = "No buses tracked in next hour"
_ERROR_MESSAGE = "Live data unavailable — retrying"


def empty_state_message(status: Optional[str]) -> tuple:
    """
    Maps an upstream fetch status to the message (and colour) shown in an
    empty arrival card, so an upstream outage is never presented as
    'no buses'.
    """
    if status == STATUS_ERROR:
        return _ERROR_MESSAGE, "#aa0000"
    return _NO_BUS_MESSAGE, "#666666"


def parse_minutes(eta_text: str) -> Optional[int]:
    """
    Extracts the arrival minute countdown from ETA strings like:
    'in 9 mins (11:36 PM)', 'in 3 mins', 'APPROACHING', 'All Aboard', etc.
    """
    if not eta_text:
        return None
    text = eta_text.lower()
    if "approach" in text or "due" in text or "now" in text or "all aboard" in text or "board" in text:
        return 0
    match = re.search(r"(\d+)\s*min", text)
    if match:
        return int(match.group(1))
    return None


def draw_header_badge(
    draw: ImageDraw.ImageDraw,
    x0: int,
    y0: int,
    text: str,
    font,
    pad_x: int = 14,
    pad_y: int = 5,
    radius: int = 6,
) -> int:
    """
    Draws an inverted black badge with white text, perfectly centered with generous padding.
    Returns the right edge (x1) of the badge bounding box.
    """
    bbox = draw.textbbox((0, 0), text, font=font)
    tw = bbox[2] - bbox[0]
    th = bbox[3] - bbox[1]
    box_w = tw + 2 * pad_x
    box_h = 32
    box_x1 = x0 + box_w
    box_y1 = y0 + box_h
    draw.rounded_rectangle([x0, y0, box_x1, box_y1], radius=radius, fill="black")
    text_x = x0 + (box_w - tw) // 2 - bbox[0]
    text_y = y0 + (box_h - th) // 2 - bbox[1]
    draw.text((text_x, text_y), text, fill="white", font=font)
    return box_x1


def draw_rounded_card(draw: ImageDraw.ImageDraw, xy, radius=12, fill="white", outline="black", width=2):
    draw.rounded_rectangle(xy, radius=radius, fill=fill, outline=outline, width=width)


def ellipsize_to_width(draw: ImageDraw.ImageDraw, text: str, font, max_width: int) -> str:
    """
    Truncates text with a trailing ellipsis so its rendered width never exceeds
    max_width. Returns the original string when it already fits (or when the
    budget is too small to place even an ellipsis).
    """
    if max_width <= 0 or not text:
        return text
    if draw.textbbox((0, 0), text, font=font)[2] <= max_width:
        return text
    ellipsis = "…"
    if draw.textbbox((0, 0), ellipsis, font=font)[2] > max_width:
        return ""
    lo, hi = 0, len(text)
    while lo < hi:
        mid = (lo + hi + 1) // 2
        if draw.textbbox((0, 0), text[:mid] + ellipsis, font=font)[2] <= max_width:
            lo = mid
        else:
            hi = mid - 1
    return text[:lo].rstrip() + ellipsis


def mock_badge_width(draw: ImageDraw.ImageDraw, font) -> int:
    """Returns the width of a 'MOCK DATA' badge, to reserve header space."""
    bbox = draw.textbbox((0, 0), "MOCK DATA", font=font)
    return (bbox[2] - bbox[0]) + 16


def draw_mock_badge(draw: ImageDraw.ImageDraw, x: int, y: int, font=None) -> int:
    """
    Draws a prominent 'MOCK DATA' badge so fabricated preview data is never
    mistaken for live telemetry. Returns the right edge of the badge.
    """
    if font is None:
        font = get_font(11, bold=True)
    text = "MOCK DATA"
    bbox = draw.textbbox((0, 0), text, font=font)
    tw = bbox[2] - bbox[0]
    box_h = 22
    x1 = x + tw + 16
    draw.rounded_rectangle([x, y, x1, y + box_h], radius=4, fill="black")
    draw.text((x + 8 - bbox[0], y + (box_h - (bbox[3] - bbox[1])) // 2 - bbox[1]), text, fill="white", font=font)
    return x1


def draw_battery_indicator(
    draw: ImageDraw.ImageDraw,
    x: int,
    y: int,
    level: Optional[int],
    is_charging: bool = False,
    font=None,
) -> int:
    """
    Draws a clean, high-contrast e-ink battery icon with percentage text and optional charging bolt.
    xy specifies top-left of the overall indicator.
    Returns the total width drawn.
    """
    if level is None or level < 0:
        return 0

    curr_x = x

    # Draw crisp vector lightning bolt if charging
    if is_charging:
        bolt_pts = [
            (curr_x + 5, y),
            (curr_x + 1, y + 7),
            (curr_x + 4, y + 7),
            (curr_x + 2, y + 13),
            (curr_x + 8, y + 5),
            (curr_x + 5, y + 5),
        ]
        draw.polygon(bolt_pts, fill="black")
        curr_x += 12

    label = f"{level}%"
    if font:
        bbox = draw.textbbox((0, 0), label, font=font)
        label_w = bbox[2] - bbox[0]
        draw.text((curr_x, y), label, fill="black", font=font)
        curr_x += label_w + 6
    else:
        curr_x += 6

    bw, bh = 28, 14
    term_w, term_h = 3, 6
    icon_x = curr_x
    icon_y = y + 1

    # Outer battery shell
    draw.rounded_rectangle([icon_x, icon_y, icon_x + bw, icon_y + bh], radius=3, outline="black", width=2)
    # Terminal cap on right edge
    term_y = icon_y + (bh - term_h) // 2
    draw.rounded_rectangle([icon_x + bw, term_y, icon_x + bw + term_w, term_y + term_h], radius=1, fill="black")

    # Inner charge bar
    inner_pad = 3
    max_fill_w = bw - (inner_pad * 2) - 1
    fill_w = max(2, int(max_fill_w * (min(level, 100) / 100.0)))
    draw.rectangle([icon_x + inner_pad, icon_y + inner_pad, icon_x + inner_pad + fill_w, icon_y + bh - inner_pad], fill="black")

    return (icon_x + bw + term_w) - x


def resolve_view(view: str = "auto", hour: Optional[int] = None) -> str:
    """
    Resolves view mode ('morning' or 'evening').
    - 'morning': Citi Bike Hero view (AM commute, 5:00 AM - 12:00 PM).
    - 'evening': Bus Hero view (PM commute / evening / night, 12:00 PM - 5:00 AM).
    - 'auto': Automatically switches based on current local hour.
    """
    v = (view or "auto").lower().strip()
    if v in ("morning", "citi", "citibike", "am"):
        return "morning"
    if v in ("evening", "bus", "pm", "afternoon", "night"):
        return "evening"

    if hour is None:
        hour = datetime.now().hour
    if 5 <= hour < 12:
        return "morning"
    return "evening"


def draw_bottom_button_bar(
    draw: ImageDraw.ImageDraw,
    width: int,
    height: int,
    active_view: str = "morning",
    font=None,
):
    """
    Renders 5 tactile touch buttons across the bottom edge of the dashboard:
    [ BUSES ] [ CITI BIKE ] [ ☼ LIGHT ] [ ↻ REFRESH ] [ ✕ EXIT ]
    The active view button is highlighted with inverted fill (black fill, white text).
    """
    is_tall = height >= 580
    btn_y0 = height - (44 if is_tall else 38)
    btn_y1 = height - (10 if is_tall else 8)
    btn_h = btn_y1 - btn_y0

    # Subtle separator line above the buttons
    draw.line([(20, btn_y0 - 8), (width - 20, btn_y0 - 8)], fill="#bbbbbb", width=1)

    start_x = 20
    total_w = width - 40
    gap = 10
    col_w = (total_w - 4 * gap) // 5

    if font is None:
        font = get_font(12, bold=True)

    is_bus = (active_view == "evening")
    is_bike = (active_view == "morning")

    buttons = [
        ("● BUSES" if is_bus else "BUSES", is_bus),
        ("● CITI BIKE" if is_bike else "CITI BIKE", is_bike),
        ("☼ LIGHT", False),
        ("↻ REFRESH", False),
        ("✕ EXIT", False),
    ]

    for i, (label, active) in enumerate(buttons):
        x0 = start_x + i * (col_w + gap)
        x1 = x0 + col_w
        bg = "black" if active else "#f4f4f4"
        fg = "white" if active else "black"
        draw.rounded_rectangle([x0, btn_y0, x1, btn_y1], radius=6, fill=bg, outline="black", width=2)
        bbox = draw.textbbox((0, 0), label, font=font)
        tw = bbox[2] - bbox[0]
        th = bbox[3] - bbox[1]
        draw.text((x0 + (col_w - tw) // 2, btn_y0 + (btn_h - th) // 2 - 1), label, fill=fg, font=font)


def render_morning_view(
    draw: ImageDraw.ImageDraw,
    stops_data: Dict[str, List[Dict[str, Any]]],
    citibike_data: List[Dict[str, Any]],
    now: datetime,
    batt_level: Optional[int],
    is_charging: bool,
    is_mock: bool,
    stop_status: Optional[Dict[str, str]] = None,
    width: int = WIDTH,
    height: int = HEIGHT,
):
    """
    Morning Commute View:
    Citi Bike dock status is the primary hero display (3 large cards for Clinton & 9th,
    Washington & 11th, and Washington & 8th).
    NJ Transit 126 bus arrivals are displayed in a compact bottom commute bar.
    """
    is_tall = height >= 580

    font_title = get_font(21, bold=True)
    font_header_sub = get_font(12, bold=False)
    font_time = get_font(18, bold=True)
    font_card_title = get_font(16 if is_tall else 15, bold=True)
    font_walk = get_font(11, bold=True)
    font_hero_num = get_font(58 if is_tall else 52, bold=True)
    font_hero_label = get_font(13, bold=True)
    font_sub_stat = get_font(14 if is_tall else 13, bold=False)
    font_badge = get_font(11, bold=True)
    font_bus_bar_title = get_font(11, bold=True)
    font_bus_stop = get_font(14 if is_tall else 13, bold=True)
    font_bus_eta = get_font(14 if is_tall else 13, bold=True)
    font_bus_meta = get_font(13 if is_tall else 12, bold=False)

    now_time_str = now.strftime("%-I:%M %p")
    now_date_str = now.strftime("%A, %b %-d")

    # 1. Header
    time_bbox = draw.textbbox((0, 0), now_time_str, font=font_time)
    time_w = time_bbox[2] - time_bbox[0]
    time_x = width - 20 - time_w
    draw.text((time_x, 15), now_time_str, fill="black", font=font_time)

    batt_x = time_x
    if batt_level is not None:
        font_batt = get_font(13, bold=True)
        label = f"{batt_level}%"
        bbox = draw.textbbox((0, 0), label, font=font_batt)
        label_w = bbox[2] - bbox[0]
        bolt_w = 12 if is_charging else 0
        total_batt_w = bolt_w + label_w + 6 + 28 + 3
        batt_x = time_x - total_batt_w - 18
        draw_battery_indicator(draw, batt_x, 16, batt_level, is_charging=is_charging, font=font_batt)

    b_x1 = draw_header_badge(draw, 20, 14, "CITI BIKE", font_title, pad_x=12)
    morning_title = "HOBOKEN MORNING DOCKS"
    # Ensure title never collides with battery indicator
    max_title_w = (batt_x - 16) - (b_x1 + 12)
    tb = draw.textbbox((0, 0), morning_title, font=font_title)
    if (tb[2] - tb[0]) > max_title_w:
        morning_title = "MORNING DOCKS"
    draw.text((b_x1 + 12, 15), morning_title, fill="black", font=font_title)
    draw.text((b_x1 + 12, 39), "919 PARK AVE • E-BIKE PRIORITY & 126 BUS", fill="#555555", font=font_header_sub)

    date_bbox = draw.textbbox((0, 0), now_date_str, font=font_header_sub)
    date_w = date_bbox[2] - date_bbox[0]
    draw.text((width - 20 - date_w, 39), now_date_str, fill="#555555", font=font_header_sub)

    if is_mock:
        badge_font = get_font(11, bold=True)
        draw_mock_badge(draw, width - 20 - date_w - mock_badge_width(draw, badge_font) - 10, 36, badge_font)

    draw.line([(20, 62), (width - 20, 62)], fill="black", width=2)

    # 2. Citi Bike Hero Cards (3 Columns)
    cb_y0 = 70
    cb_y1 = 416 if is_tall else 346
    col_w = (width - 40 - 22) // 3
    gap = 11

    for i, c in enumerate(citibike_data[:3]):
        cx0 = 20 + i * (col_w + gap)
        cx1 = cx0 + col_w

        draw.rounded_rectangle([cx0, cb_y0, cx1, cb_y1], radius=10, fill="white", outline="black", width=2)

        pill_h = 44
        draw.rounded_rectangle([cx0, cb_y0, cx1, cb_y0 + pill_h], radius=10, fill="#f2f2f2", outline="black", width=2)
        draw.rectangle([cx0 + 1, cb_y0 + pill_h - 12, cx1 - 1, cb_y0 + pill_h], fill="#f2f2f2")
        draw.line([(cx0, cb_y0 + pill_h), (cx1, cb_y0 + pill_h)], fill="black", width=2)

        walk_text = f"{c['walk_min']} MIN"
        wb = draw.textbbox((0, 0), walk_text, font=font_walk)
        ww = wb[2] - wb[0]
        badge_x0 = cx1 - ww - 16
        draw.rounded_rectangle([badge_x0, cb_y0 + 11, cx1 - 8, cb_y0 + 33], radius=4, fill="black")
        draw.text((cx1 - ww - 12, cb_y0 + 15), walk_text, fill="white", font=font_walk)

        card_title_font = font_card_title
        max_title_w = badge_x0 - (cx0 + 10) - 6
        for size in [16 if is_tall else 15, 14, 13, 12]:
            candidate_font = get_font(size, bold=True)
            tb = draw.textbbox((0, 0), c["name"].upper(), font=candidate_font)
            if (tb[2] - tb[0]) <= max_title_w:
                card_title_font = candidate_font
                break
        else:
            card_title_font = get_font(11, bold=True)

        title_line = ellipsize_to_width(draw, c["name"].upper(), card_title_font, max_title_w)
        draw.text((cx0 + 10, cb_y0 + 6), title_line, fill="black", font=card_title_font)

        if c.get("is_offline"):
            draw.text((cx0 + 14, cb_y0 + (90 if is_tall else 80)), "STATION OFFLINE", fill="#666666", font=font_card_title)
            draw.text((cx0 + 14, cb_y0 + (125 if is_tall else 110)), "Temporarily not renting", fill="#888888", font=font_sub_stat)
        else:
            stat_y = cb_y0 + (64 if is_tall else 54)
            draw.text((cx0 + 12, stat_y), str(c["ebikes"]), fill="black", font=font_hero_num)
            num_box = draw.textbbox((cx0 + 12, stat_y), str(c["ebikes"]), font=font_hero_num)
            draw.text((num_box[2] + 8, stat_y + 16), "E-BIKES", fill="black", font=font_hero_label)
            draw.text((num_box[2] + 8, stat_y + 34), "AVAILABLE", fill="#555555", font=font_walk)

            div_y = stat_y + (86 if is_tall else 70)
            draw.line([(cx0 + 10, div_y), (cx1 - 10, div_y)], fill="#e0e0e0", width=1)

            draw.text((cx0 + 12, div_y + (16 if is_tall else 12)), f"{c['classic']} Classic Bikes", fill="#333333", font=font_sub_stat)
            draw.text((cx0 + 12, div_y + (40 if is_tall else 32)), f"{c['docks']} Open Docks", fill="#333333", font=font_sub_stat)

            badge_box_y0 = cb_y1 - (46 if is_tall else 42)
            badge_box_y1 = cb_y1 - (12 if is_tall else 12)
            draw.rounded_rectangle([cx0 + 10, badge_box_y0, cx1 - 10, badge_box_y1], radius=6, fill="#f8f8f8", outline="black", width=1)
            if c["ebikes"] >= 4:
                status_label = "E-BIKES READY"
            elif c["ebikes"] > 0:
                status_label = "LOW E-BIKES"
            elif c["docks"] == 0:
                status_label = "STATION FULL"
            else:
                status_label = "NO E-BIKES"
            draw.text((cx0 + 18, badge_box_y0 + (9 if is_tall else 8)), status_label, fill="black", font=font_badge)

    # 3. Compact 126 Bus Section (Bottom Bar)
    bus_y0 = 422 if is_tall else 348
    bus_y1 = 542 if is_tall else 424
    draw.rounded_rectangle([20, bus_y0, width - 20, bus_y1], radius=10, fill="white", outline="black", width=2)

    header_h = 26 if is_tall else 24
    draw.rounded_rectangle([20, bus_y0, width - 20, bus_y0 + header_h], radius=10, fill="#f4f4f4", outline="black", width=2)
    draw.rectangle([21, bus_y0 + 14, width - 21, bus_y0 + header_h], fill="#f4f4f4")
    draw.line([(20, bus_y0 + header_h), (width - 20, bus_y0 + header_h)], fill="black", width=1)
    draw.text((34, bus_y0 + (6 if is_tall else 5)), "NJ TRANSIT 126 BUS • UPCOMING PORT AUTHORITY ARRIVALS", fill="#444444", font=font_bus_bar_title)

    # Row offsets below the section header. The compact (800x480) box has only
    # 52px of content height, so its rows must sit tighter than the tall one;
    # using the tall offsets here pushed the "Following" line past the border.
    content_top = bus_y0 + header_h
    if is_tall:
        row_name, row_next, row_follow, row_third, row_empty = 10, 32, 50, 68, 34
    else:
        row_name, row_next, row_follow, row_empty = 2, 17, 34, 20

    bus_col_w = (width - 40) // 2
    for i, stop_cfg in enumerate(STOPS):
        sid = stop_cfg["id"]
        bx0 = 20 + i * bus_col_w
        bx1 = bx0 + bus_col_w
        if i > 0:
            draw.line([(bx0, bus_y0 + header_h + 6), (bx0, bus_y1 - 6)], fill="#dddddd", width=1)

        stop_name_y = content_top + row_name
        draw.text((bx0 + 14, stop_name_y), stop_cfg["name"].upper(), fill="black", font=font_bus_stop)

        walk_badge = f"{stop_cfg['walk_min']}m walk"
        wb = draw.textbbox((0, 0), walk_badge, font=font_walk)
        ww = wb[2] - wb[0]
        draw.rounded_rectangle([bx1 - ww - 18, stop_name_y - 2, bx1 - 10, stop_name_y + 14], radius=4, fill="#eeeeee", outline="black", width=1)
        draw.text((bx1 - ww - 14, stop_name_y), walk_badge, fill="black", font=font_walk)

        # Column interior; text must not cross into the neighbouring column.
        col_max_w = (bx1 - 12) - (bx0 + 14)

        arrivals = stops_data.get(sid, [])
        if arrivals:
            first_bus = arrivals[0]
            eta = first_bus.get("eta", "")
            b_num = f" (Bus #{first_bus['vehicle_id']})" if first_bus.get("vehicle_id") else ""
            next_line = ellipsize_to_width(draw, f"Next: {eta}{b_num}", font_bus_eta, col_max_w)
            draw.text((bx0 + 14, content_top + row_next), next_line, fill="black", font=font_bus_eta)
            if len(arrivals) > 1:
                next_eta = arrivals[1].get("eta", "")
                follow_line = ellipsize_to_width(draw, f"Following: {next_eta}", font_bus_meta, col_max_w)
                draw.text((bx0 + 14, content_top + row_follow), follow_line, fill="#555555", font=font_bus_meta)
            if is_tall and len(arrivals) > 2:
                third_eta = arrivals[2].get("eta", "")
                third_line = ellipsize_to_width(draw, f"Upcoming: {third_eta}", font_bus_meta, col_max_w)
                draw.text((bx0 + 14, content_top + row_third), third_line, fill="#777777", font=font_bus_meta)
        else:
            status = (stop_status or {}).get(sid)
            msg, color = empty_state_message(status)
            msg_font = font_bus_meta if status == STATUS_ERROR else font_bus_eta
            msg = ellipsize_to_width(draw, msg, msg_font, col_max_w)
            draw.text((bx0 + 14, content_top + row_empty), msg, fill=color, font=msg_font)

    # 4. Touch Button Bar (Interactive Actions)
    draw_bottom_button_bar(draw, width, height, active_view="morning")


def render_evening_view(
    draw: ImageDraw.ImageDraw,
    stops_data: Dict[str, List[Dict[str, Any]]],
    citibike_data: List[Dict[str, Any]],
    now: datetime,
    batt_level: Optional[int],
    is_charging: bool,
    is_mock: bool,
    stop_status: Optional[Dict[str, str]] = None,
    width: int = WIDTH,
    height: int = HEIGHT,
):
    """
    Afternoon / Evening View:
    NJ Transit 126 bus arrivals are the primary hero display with large minute countdowns
    and decision badges. Citi Bike dock inventory is summarized across the bottom.
    """
    has_citibike = bool(citibike_data)
    is_tall = height >= 580

    font_title = get_font(21, bold=True)
    font_header_sub = get_font(12, bold=False)
    font_time = get_font(18, bold=True)
    font_stop_name = get_font(20, bold=True)
    font_walk = get_font(12, bold=True)
    font_countdown_num = get_font(62 if has_citibike else 74, bold=True)
    font_countdown_unit = get_font(22 if has_citibike else 24, bold=True)
    font_badge = get_font(12 if has_citibike else 13, bold=True)
    font_detail = get_font(13 if has_citibike else 14, bold=False)
    font_detail_bold = get_font(13 if has_citibike else 14, bold=True)
    font_footer = get_font(11 if has_citibike else 12, bold=False)

    font_cb_tag = get_font(11, bold=True)
    font_cb_name = get_font(14, bold=True)
    font_cb_stat = get_font(13, bold=True)
    font_cb_sub = get_font(11, bold=False)
    font_cb_walk = get_font(10, bold=True)

    now_time_str = now.strftime("%-I:%M %p")
    now_date_str = now.strftime("%A, %b %-d")

    # 1. Header
    b_x1 = draw_header_badge(draw, 20, 14, "126", font_title, pad_x=16)
    draw.text((b_x1 + 12, 15), "HOBOKEN → NYC PORT AUTHORITY", fill="black", font=font_title)
    sub_title = "NJ TRANSIT 126 & CITI BIKE LIVE TRACKER" if has_citibike else "NJ TRANSIT REAL-TIME TRACKER"
    draw.text((b_x1 + 12, 39), sub_title, fill="#555555", font=font_header_sub)

    time_bbox = draw.textbbox((0, 0), now_time_str, font=font_time)
    time_w = time_bbox[2] - time_bbox[0]
    time_x = width - 20 - time_w
    draw.text((time_x, 15), now_time_str, fill="black", font=font_time)

    if batt_level is not None:
        font_batt = get_font(13, bold=True)
        label = f"{batt_level}%"
        bbox = draw.textbbox((0, 0), label, font=font_batt)
        label_w = bbox[2] - bbox[0]
        bolt_w = 12 if is_charging else 0
        total_batt_w = bolt_w + label_w + 6 + 28 + 3
        batt_x = time_x - total_batt_w - 18
        draw_battery_indicator(draw, batt_x, 16, batt_level, is_charging=is_charging, font=font_batt)

    date_bbox = draw.textbbox((0, 0), now_date_str, font=font_header_sub)
    date_w = date_bbox[2] - date_bbox[0]
    draw.text((width - 20 - date_w, 39), now_date_str, fill="#555555", font=font_header_sub)

    if is_mock:
        badge_font = get_font(11, bold=True)
        draw_mock_badge(draw, width - 20 - date_w - mock_badge_width(draw, badge_font) - 10, 36, badge_font)

    draw.line([(20, 62), (width - 20, 62)], fill="black", width=2)

    # 2. Dual Bus Cards (Side-by-Side)
    col_w = (width - 40 - 20) // 2
    col_h = (346 if is_tall else 276) if has_citibike else (475 if is_tall else 345)
    card_y = 70 if has_citibike else 80
    xs = [20, 20 + col_w + 20]

    for i, stop_cfg in enumerate(STOPS):
        stop_id = stop_cfg["id"]
        x0 = xs[i]
        x1 = x0 + col_w
        y0 = card_y
        y1 = y0 + col_h

        draw.rounded_rectangle([x0, y0, x1, y1], radius=10, fill="white", outline="black", width=2)

        pill_h = 48 if is_tall else (44 if has_citibike else 52)
        draw.rounded_rectangle([x0, y0, x1, y0 + pill_h], radius=10, fill="#f2f2f2", outline="black", width=2)
        draw.rectangle([x0 + 1, y0 + pill_h - 12, x1 - 1, y0 + pill_h], fill="#f2f2f2")
        draw.line([(x0, y0 + pill_h), (x1, y0 + pill_h)], fill="black", width=2)

        draw.text((x0 + 12, y0 + (6 if is_tall else (5 if has_citibike else 8))), stop_cfg["name"].upper(), fill="black", font=font_stop_name)
        draw.text((x0 + 12, y0 + (28 if is_tall else (26 if has_citibike else 32))), stop_cfg["subtitle"], fill="#555555", font=font_header_sub)

        walk_text = f"{stop_cfg['walk_min']} MIN WALK"
        wb = draw.textbbox((0, 0), walk_text, font=font_walk)
        ww = wb[2] - wb[0]
        walk_btn_h = 24 if is_tall else (22 if has_citibike else 24)
        walk_btn_y = y0 + (12 if is_tall else (11 if has_citibike else 14))
        draw.rounded_rectangle([x1 - ww - 20, walk_btn_y, x1 - 10, walk_btn_y + walk_btn_h], radius=5, fill="black")
        draw.text((x1 - ww - 15, walk_btn_y + (5 if is_tall else (4 if has_citibike else 5))), walk_text, fill="white", font=font_walk)

        arrivals = stops_data.get(stop_id, [])
        if not arrivals:
            status = (stop_status or {}).get(stop_id)
            if status == STATUS_ERROR:
                headline, subtext, headline_color = "LIVE DATA UNAVAILABLE", "Could not reach NJ Transit.\nRetrying automatically.", "#aa0000"
            else:
                headline, subtext, headline_color = "NO BUSES IN NEXT HOUR", "Off-peak schedule active or\nno buses currently tracked.", "#444444"
            draw.text((x0 + 24, y0 + (80 if is_tall else (70 if has_citibike else 120))), headline, fill=headline_color, font=font_stop_name if has_citibike else font_title)
            draw.text(
                (x0 + 24, y0 + (115 if is_tall else (100 if has_citibike else 155))),
                subtext,
                fill=("#aa0000" if status == STATUS_ERROR else "#666666"),
                font=font_detail,
            )
            box_h0 = y1 - (88 if is_tall else (66 if has_citibike else 85))
            box_h1 = y1 - (12 if has_citibike else 20)
            draw.rounded_rectangle(
                [x0 + 14, box_h0, x1 - 14, box_h1],
                radius=7,
                fill="#fafafa",
                outline="#bbbbbb",
                width=1,
            )
            tip = "Check your network / server connection" if status == STATUS_ERROR else "Tip: Check NJ Transit app for daily timetables"
            draw.text((x0 + 24, box_h0 + (18 if is_tall else (16 if has_citibike else 23))), tip, fill="#666666", font=font_footer)
        else:
            first_bus = arrivals[0]
            first_eta_raw = first_bus.get("eta", "")
            first_min = parse_minutes(first_eta_raw)
            walk_min = stop_cfg["walk_min"]

            cy = y0 + (60 if is_tall else (54 if has_citibike else 68))
            if first_min is not None:
                if first_min == 0:
                    cd_str = "DUE"
                    unit_str = ""
                else:
                    cd_str = str(first_min)
                    unit_str = "MIN"
            else:
                time_match = re.search(r"\b(\d{1,2}:\d{2}(?:\s*[AP]M)?)\b", first_eta_raw, re.IGNORECASE)
                if time_match:
                    cd_str = time_match.group(1)
                else:
                    cd_str = first_eta_raw[:7].strip() if first_eta_raw else "--"
                unit_str = ""

            draw.text((x0 + 14, cy), cd_str, fill="black", font=font_countdown_num)
            c_bbox = draw.textbbox((x0 + 14, cy), cd_str, font=font_countdown_num)
            unit_x = c_bbox[2] + 6

            if unit_str:
                draw.text((unit_x, cy + (34 if has_citibike else 40)), unit_str, fill="black", font=font_countdown_unit)

            badge_y = cy + (14 if has_citibike else 18)
            if first_min is not None:
                if "board" in first_eta_raw.lower() or "all aboard" in first_eta_raw.lower():
                    badge_label = "ALL ABOARD"
                    badge_bg = "black"
                    badge_fg = "white"
                elif first_min <= walk_min:
                    badge_label = "RUN! LEAVING SOON"
                    badge_bg = "black"
                    badge_fg = "white"
                elif first_min <= walk_min + 3:
                    badge_label = "WALK NOW"
                    badge_bg = "black"
                    badge_fg = "white"
                elif first_min <= walk_min + 7:
                    badge_label = "GET READY"
                    badge_bg = "#f0f0f0"
                    badge_fg = "black"
                else:
                    badge_label = "ON TIME"
                    badge_bg = "#f0f0f0"
                    badge_fg = "black"

                bb = draw.textbbox((0, 0), badge_label, font=font_badge)
                bw = bb[2] - bb[0]
                badge_x = x1 - bw - 24
                badge_h = 24 if has_citibike else 28
                draw.rounded_rectangle(
                    [badge_x, badge_y, badge_x + bw + 14, badge_y + badge_h],
                    radius=5,
                    fill=badge_bg,
                    outline="black",
                    width=2,
                )
                draw.text((badge_x + 7, badge_y + (4 if has_citibike else 6)), badge_label, fill=badge_fg, font=font_badge)

            card_text_max_w = (x1 - 10) - (x0 + 16)
            sched_str = ellipsize_to_width(draw, f"Estimated: {first_eta_raw}", font_detail, card_text_max_w)
            draw.text((x0 + 16, cy + (80 if is_tall else (74 if has_citibike else 82))), sched_str, fill="#333333", font=font_detail)

            bus_num = first_bus.get("vehicle_id")
            load = first_bus.get("occupancy")
            load_clean = load.replace("_", " ").title() if load and load != "EMPTY" else "Seats Available"
            bus_meta = f"Bus #{bus_num}  •  {load_clean}" if bus_num else f"Status: {load_clean}"
            bus_meta = ellipsize_to_width(draw, bus_meta, font_detail, card_text_max_w)
            draw.text((x0 + 16, cy + (102 if is_tall else (94 if has_citibike else 104))), bus_meta, fill="#444444", font=font_detail)

            box_y0 = y1 - (88 if is_tall else (66 if has_citibike else 85))
            box_y1 = y1 - (12 if has_citibike else 16)
            draw.rounded_rectangle([x0 + 12, box_y0, x1 - 12, box_y1], radius=7, fill="#f8f8f8", outline="black", width=1)

            # Text must stay inside the inner box (x0+12 .. x1-12), so clamp the
            # right edge to the box interior minus the left inset.
            inner_left = x0 + 20
            inner_right = x1 - 12
            inner_max_w = inner_right - inner_left - 4

            if len(arrivals) > 1:
                next_bus = arrivals[1]
                next_eta = next_bus.get("eta", "Scheduled")
                next_bus_num = f" (Bus #{next_bus['vehicle_id']})" if next_bus.get("vehicle_id") else ""
                draw.text((x0 + 20, box_y0 + (8 if is_tall else (9 if has_citibike else 12))), "UPCOMING BUSES:", fill="#555555", font=font_walk)
                line2 = ellipsize_to_width(draw, f"126 to NYC → {next_eta}{next_bus_num}", font_detail_bold, inner_max_w)
                draw.text((x0 + 20, box_y0 + (25 if is_tall else (27 if has_citibike else 32))), line2, fill="black", font=font_detail_bold)
                if is_tall and len(arrivals) > 2:
                    third_bus = arrivals[2]
                    third_eta = third_bus.get("eta", "Scheduled")
                    third_bus_num = f" (#{third_bus['vehicle_id']})" if third_bus.get("vehicle_id") else ""
                    line3 = ellipsize_to_width(draw, f"Following → {third_eta}{third_bus_num}", font_detail, inner_max_w)
                    draw.text((x0 + 20, box_y0 + 46), line3, fill="#555555", font=font_detail)
            else:
                draw.text((x0 + 20, box_y0 + (8 if is_tall else (9 if has_citibike else 12))), "UPCOMING BUSES:", fill="#555555", font=font_walk)
                draw.text((x0 + 20, box_y0 + (25 if is_tall else (27 if has_citibike else 32))), "No further buses in next 60 min", fill="#666666", font=font_detail)

    # 3. Citi Bike Bottom Section
    if has_citibike:
        cb_x0, cb_y0, cb_x1, cb_y1 = 20, (422 if is_tall else 348), width - 20, (542 if is_tall else 424)
        draw.rounded_rectangle([cb_x0, cb_y0, cb_x1, cb_y1], radius=10, fill="white", outline="black", width=2)

        header_h = 26 if is_tall else 24
        draw.rounded_rectangle([cb_x0, cb_y0, cb_x1, cb_y0 + header_h], radius=10, fill="#f4f4f4", outline="black", width=2)
        draw.rectangle([cb_x0 + 1, cb_y0 + 14, cb_x1 - 1, cb_y0 + header_h], fill="#f4f4f4")
        draw.line([(cb_x0, cb_y0 + header_h), (cb_x1, cb_y0 + header_h)], fill="black", width=1)
        draw.text((cb_x0 + 14, cb_y0 + (6 if is_tall else 5)), "CITI BIKE • NEAREST E-BIKE DOCKS TO 919 PARK AVE", fill="#444444", font=font_cb_tag)

        body_y0 = cb_y0 + header_h
        cb_display_data = citibike_data[:3]
        col_count = len(cb_display_data)
        col_w = (cb_x1 - cb_x0) // col_count

        # Row offsets below the section header. The compact (800x480) box body is
        # only 52px tall, so its rows sit tighter than the tall layout; the tall
        # offsets previously pushed the "Docks available" line past the border.
        if is_tall:
            r_name, r_stat, r_sub, r_badge = 10, 32, 52, 70
            badge_top, badge_bot, walk_top = 8, 24, 10
        else:
            r_name, r_stat, r_sub = 5, 23, 38
            badge_top, badge_bot, walk_top = 6, 22, 7

        for i, c in enumerate(cb_display_data):
            cx0 = cb_x0 + i * col_w
            cx1 = cx0 + col_w
            if i > 0:
                draw.line([(cx0, body_y0 + 6), (cx0, cb_y1 - 6)], fill="#dddddd", width=1)

            walk_str = f"{c['walk_min']} MIN"
            wb = draw.textbbox((0, 0), walk_str, font=font_cb_walk)
            ww = wb[2] - wb[0]
            badge_x0 = cx1 - ww - 18
            draw.rounded_rectangle([badge_x0, body_y0 + badge_top, cx1 - 10, body_y0 + badge_bot], radius=4, fill="#eeeeee", outline="black", width=1)
            draw.text((cx1 - ww - 14, body_y0 + walk_top), walk_str, fill="black", font=font_cb_walk)

            cb_name_font = font_cb_name
            max_name_w = badge_x0 - (cx0 + 14) - 6
            tb = draw.textbbox((0, 0), c["name"].upper(), font=cb_name_font)
            if (tb[2] - tb[0]) > max_name_w:
                cb_name_font = get_font(12, bold=True)
            draw.text((cx0 + 14, body_y0 + r_name), c["name"].upper(), fill="black", font=cb_name_font)

            if c.get("is_offline"):
                draw.text((cx0 + 14, body_y0 + r_stat), "STATION OFFLINE", fill="#777777", font=font_cb_stat)
                draw.text((cx0 + 14, body_y0 + r_sub), "Temporarily unavailable", fill="#777777", font=font_cb_sub)
            else:
                stat_str = f"{c['ebikes']} Ebikes  •  {c['classic']} Classic"
                draw.text((cx0 + 14, body_y0 + r_stat), stat_str, fill="black", font=font_cb_stat)
                docks_str = f"{c['docks']} Docks available"
                draw.text((cx0 + 14, body_y0 + r_sub), docks_str, fill="#555555", font=font_cb_sub)
                if is_tall:
                    if c["ebikes"] >= 4:
                        cb_badge = "● GOOD AVAILABILITY"
                    elif c["ebikes"] > 0:
                        cb_badge = "● LIMITED E-BIKES"
                    elif c["docks"] == 0:
                        cb_badge = "● DOCKS FULL"
                    else:
                        cb_badge = "● CLASSIC ONLY"
                    draw.text((cx0 + 14, body_y0 + r_badge), cb_badge, fill="#444444", font=font_cb_walk)

    # 4. Touch Button Bar (Interactive Actions)
    draw_bottom_button_bar(draw, width, height, active_view="evening")


def render_dashboard(
    stops_data: Dict[str, List[Dict[str, Any]]],
    citibike_data: Optional[List[Dict[str, Any]]] = None,
    output_path: str = "dashboard.png",
    view: str = "auto",
    is_mock: bool = False,
    batt_level: Optional[int] = None,
    is_charging: bool = False,
    stop_status: Optional[Dict[str, str]] = None,
    width: int = WIDTH,
    height: int = HEIGHT,
    scale: float = 1.0,
) -> str:
    """
    Renders a high-contrast black-and-white image optimized for e-ink
    or low-power dashboard screens (default 800x480, or 800x600 for 4:3 displays).
    Supports 'morning' (Citi Bike Hero) and 'evening' (Bus Hero) view modes.

    stop_status maps stop id -> fetch status ('ok'/'empty'/'error') so that an
    upstream outage can be distinguished from a genuine absence of buses.

    width/height are the *logical* design-space dimensions that drive layout.
    `scale` maps that design space onto a larger native canvas: the output image
    is (width*scale) x (height*scale) and all drawing is done natively at that
    resolution (fonts are reconstructed at scale), so no bitmap upscaling occurs.
    """
    if citibike_data is None:
        try:
            tracker = CitiBikeTracker()
            citibike_data = tracker.get_mock_data() if is_mock else tracker.get_station_status()
        except Exception:
            citibike_data = []

    active_view = resolve_view(view)

    # Create native-resolution canvas; view code draws in logical coordinates.
    native_w = max(1, int(round(width * scale)))
    native_h = max(1, int(round(height * scale)))
    img = Image.new("RGB", (native_w, native_h), color="white")
    draw = ScaledDraw(ImageDraw.Draw(img), scale)
    now = datetime.now()

    if active_view == "morning":
        render_morning_view(
            draw,
            stops_data,
            citibike_data,
            now,
            batt_level=batt_level,
            is_charging=is_charging,
            is_mock=is_mock,
            stop_status=stop_status,
            width=width,
            height=height,
        )
    else:
        render_evening_view(
            draw,
            stops_data,
            citibike_data,
            now,
            batt_level=batt_level,
            is_charging=is_charging,
            is_mock=is_mock,
            stop_status=stop_status,
            width=width,
            height=height,
        )

    # Save output
    img.save(output_path, "PNG")
    print(f"✓ Dashboard image successfully rendered [{active_view.upper()} VIEW]: {output_path} ({native_w}x{native_h}, scale={scale:g})")
    return output_path



def get_mock_data():
    """Generates realistic peak-hour commute data for layout previewing."""
    return {
        "20512": [
            {
                "route": "126",
                "destination": "126 NEW YORK",
                "eta": "in 5 mins (8:35 AM)",
                "occupancy": "SEATS_AVAILABLE",
                "vehicle_id": "25248",
            },
            {
                "route": "126",
                "destination": "126 NEW YORK",
                "eta": "in 17 mins (8:47 AM)",
                "occupancy": "HALF_EMPTY",
                "vehicle_id": "21045",
            },
        ],
        "20494": [
            {
                "route": "126",
                "destination": "126 NEW YORK VIA CLINTON",
                "eta": "in 12 mins (8:42 AM)",
                "occupancy": "EMPTY",
                "vehicle_id": "22119",
            },
            {
                "route": "126",
                "destination": "126 NEW YORK VIA CLINTON",
                "eta": "in 24 mins (8:54 AM)",
                "occupancy": "SEATS_AVAILABLE",
                "vehicle_id": "25301",
            },
        ],
    }


if __name__ == "__main__":
    use_mock = "--mock" in sys.argv
    view_arg = "auto"
    for arg in sys.argv:
        if arg.startswith("--view="):
            view_arg = arg.split("=", 1)[1]
        elif arg in ("--morning", "-morning"):
            view_arg = "morning"
        elif arg in ("--evening", "-evening"):
            view_arg = "evening"

    stops_data = {}

    stop_status = {}

    if use_mock:
        print("Rendering with mock peak-commute data...")
        stops_data = get_mock_data()
        stop_status = {stop["id"]: STATUS_OK for stop in STOPS}
    else:
        print("Fetching live data from NJ Transit API...")
        tracker = NJTransitBusTracker()
        for stop in STOPS:
            sid = stop["id"]
            status, trips = tracker.get_arrivals_with_status(stop_id=sid, route="126")
            stops_data[sid] = [normalize_arrival(t) for t in trips]
            stop_status[sid] = status

    render_dashboard(stops_data, output_path="dashboard.png", view=view_arg, is_mock=use_mock, stop_status=stop_status)
