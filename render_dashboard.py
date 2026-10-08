import os
import re
import sys
import time
from datetime import datetime
from typing import Dict, List, Any, Optional
from PIL import Image, ImageDraw, ImageFont

from bus_tracker import NJTransitBusTracker
from citibike import CitiBikeTracker

# Canvas Dimensions (standard 7.5" e-ink: TRMNL, Waveshare 7.5", etc.)
WIDTH = 800
HEIGHT = 480

# Walk times from home (in minutes)
WALK_TIMES = {
    "20512": 3,  # Washington St at 9th St
    "20494": 4,  # Clinton St at 9th St
}

STOPS = [
    {
        "id": "20512",
        "name": "Washington & 9th",
        "subtitle": "Stop #20512 • via Lincoln Tunnel",
        "walk_min": 3,
    },
    {
        "id": "20494",
        "name": "Clinton & 9th",
        "subtitle": "Stop #20494 • via Clinton Ave",
        "walk_min": 4,
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
                return ImageFont.truetype(p, size)
            except Exception:
                continue
    try:
        return ImageFont.load_default()
    except Exception:
        return None


def parse_minutes(eta_text: str) -> Optional[int]:
    """
    Extracts the arrival minute countdown from ETA strings like:
    'in 9 mins (11:36 PM)', 'in 3 mins', 'APPROACHING', etc.
    """
    if not eta_text:
        return None
    text = eta_text.lower()
    if "approach" in text or "due" in text or "now" in text:
        return 0
    match = re.search(r"(\d+)\s*min", text)
    if match:
        return int(match.group(1))
    return None


def draw_rounded_card(draw: ImageDraw.ImageDraw, xy, radius=12, fill="white", outline="black", width=2):
    draw.rounded_rectangle(xy, radius=radius, fill=fill, outline=outline, width=width)


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


def render_morning_view(
    draw: ImageDraw.ImageDraw,
    stops_data: Dict[str, List[Dict[str, Any]]],
    citibike_data: List[Dict[str, Any]],
    now: datetime,
    batt_level: Optional[int],
    is_charging: bool,
    is_mock: bool,
    view_mode: str = "auto",
):
    """
    Morning Commute View:
    Citi Bike dock status is the primary hero display (3 large cards for Clinton & 9th,
    Washington & 11th, and Washington & 8th).
    NJ Transit 126 bus arrivals are displayed in a compact bottom commute bar.
    """
    font_title = get_font(21, bold=True)
    font_header_sub = get_font(12, bold=False)
    font_time = get_font(18, bold=True)
    font_card_title = get_font(15, bold=True)
    font_walk = get_font(11, bold=True)
    font_hero_num = get_font(52, bold=True)
    font_hero_label = get_font(13, bold=True)
    font_sub_stat = get_font(13, bold=False)
    font_badge = get_font(11, bold=True)
    font_footer = get_font(11, bold=False)
    font_bus_bar_title = get_font(11, bold=True)
    font_bus_stop = get_font(13, bold=True)
    font_bus_eta = get_font(13, bold=True)
    font_bus_meta = get_font(12, bold=False)

    now_time_str = now.strftime("%-I:%M %p")
    now_date_str = now.strftime("%A, %b %-d")

    # 1. Header
    draw.rounded_rectangle([20, 14, 120, 46], radius=6, fill="black")
    draw.text((28, 18), "CITI BIKE", fill="white", font=font_title)

    draw.text((130, 15), "HOBOKEN COMMUTE • MORNING DOCKS", fill="black", font=font_title)
    draw.text((130, 39), "919 PARK AVE • CITI BIKE HERO & 126 BUS", fill="#555555", font=font_header_sub)

    time_bbox = draw.textbbox((0, 0), now_time_str, font=font_time)
    time_w = time_bbox[2] - time_bbox[0]
    time_x = WIDTH - 20 - time_w
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
    draw.text((WIDTH - 20 - date_w, 39), now_date_str, fill="#555555", font=font_header_sub)

    draw.line([(20, 62), (WIDTH - 20, 62)], fill="black", width=2)

    # 2. Citi Bike Hero Cards (3 Columns)
    cb_y0 = 70
    cb_y1 = 346
    col_w = 246
    gap = 11

    for i, c in enumerate(citibike_data[:3]):
        cx0 = 20 + i * (col_w + gap)
        cx1 = cx0 + col_w

        draw.rounded_rectangle([cx0, cb_y0, cx1, cb_y1], radius=10, fill="white", outline="black", width=2)

        pill_h = 44
        draw.rounded_rectangle([cx0, cb_y0, cx1, cb_y0 + pill_h], radius=10, fill="#f2f2f2", outline="black", width=2)
        draw.rectangle([cx0 + 1, cb_y0 + pill_h - 12, cx1 - 1, cb_y0 + pill_h], fill="#f2f2f2")
        draw.line([(cx0, cb_y0 + pill_h), (cx1, cb_y0 + pill_h)], fill="black", width=2)

        draw.text((cx0 + 10, cb_y0 + 6), c["name"].upper(), fill="black", font=font_card_title)

        walk_text = f"{c['walk_min']} MIN"
        wb = draw.textbbox((0, 0), walk_text, font=font_walk)
        ww = wb[2] - wb[0]
        draw.rounded_rectangle([cx1 - ww - 16, cb_y0 + 11, cx1 - 8, cb_y0 + 33], radius=4, fill="black")
        draw.text((cx1 - ww - 12, cb_y0 + 15), walk_text, fill="white", font=font_walk)

        if c.get("is_offline"):
            draw.text((cx0 + 14, cb_y0 + 80), "STATION OFFLINE", fill="#666666", font=font_card_title)
            draw.text((cx0 + 14, cb_y0 + 110), "Temporarily not renting", fill="#888888", font=font_sub_stat)
        else:
            stat_y = cb_y0 + 54
            draw.text((cx0 + 12, stat_y), str(c["ebikes"]), fill="black", font=font_hero_num)
            num_box = draw.textbbox((cx0 + 12, stat_y), str(c["ebikes"]), font=font_hero_num)
            draw.text((num_box[2] + 8, stat_y + 16), "E-BIKES", fill="black", font=font_hero_label)
            draw.text((num_box[2] + 8, stat_y + 34), "AVAILABLE", fill="#555555", font=font_walk)

            div_y = stat_y + 70
            draw.line([(cx0 + 10, div_y), (cx1 - 10, div_y)], fill="#e0e0e0", width=1)

            draw.text((cx0 + 12, div_y + 12), f"{c['classic']} Classic Bikes", fill="#333333", font=font_sub_stat)
            draw.text((cx0 + 12, div_y + 32), f"{c['docks']} Open Docks", fill="#333333", font=font_sub_stat)

            badge_box_y0 = cb_y1 - 42
            badge_box_y1 = cb_y1 - 12
            draw.rounded_rectangle([cx0 + 10, badge_box_y0, cx1 - 10, badge_box_y1], radius=6, fill="#f8f8f8", outline="black", width=1)
            if c["ebikes"] >= 4:
                status_label = "E-BIKES READY"
            elif c["ebikes"] > 0:
                status_label = "LOW E-BIKES"
            elif c["docks"] == 0:
                status_label = "STATION FULL"
            else:
                status_label = "NO E-BIKES"
            draw.text((cx0 + 18, badge_box_y0 + 8), status_label, fill="black", font=font_badge)

    # 3. Compact 126 Bus Section (Bottom Bar)
    bus_y0 = 354
    bus_y1 = 442
    draw.rounded_rectangle([20, bus_y0, WIDTH - 20, bus_y1], radius=10, fill="white", outline="black", width=2)

    header_h = 24
    draw.rounded_rectangle([20, bus_y0, WIDTH - 20, bus_y0 + header_h], radius=10, fill="#f4f4f4", outline="black", width=2)
    draw.rectangle([21, bus_y0 + 14, WIDTH - 21, bus_y0 + header_h], fill="#f4f4f4")
    draw.line([(20, bus_y0 + header_h), (WIDTH - 20, bus_y0 + header_h)], fill="black", width=1)
    draw.text((34, bus_y0 + 5), "NJ TRANSIT 126 BUS • UPCOMING PORT AUTHORITY ARRIVALS", fill="#444444", font=font_bus_bar_title)

    bus_col_w = (WIDTH - 40) // 2
    for i, stop_cfg in enumerate(STOPS):
        sid = stop_cfg["id"]
        bx0 = 20 + i * bus_col_w
        bx1 = bx0 + bus_col_w
        if i > 0:
            draw.line([(bx0, bus_y0 + header_h + 6), (bx0, bus_y1 - 6)], fill="#dddddd", width=1)

        draw.text((bx0 + 14, bus_y0 + header_h + 8), stop_cfg["name"].upper(), fill="black", font=font_bus_stop)

        walk_badge = f"{stop_cfg['walk_min']}m walk"
        wb = draw.textbbox((0, 0), walk_badge, font=font_walk)
        ww = wb[2] - wb[0]
        draw.rounded_rectangle([bx1 - ww - 18, bus_y0 + header_h + 6, bx1 - 10, bus_y0 + header_h + 22], radius=4, fill="#eeeeee", outline="black", width=1)
        draw.text((bx1 - ww - 14, bus_y0 + header_h + 8), walk_badge, fill="black", font=font_walk)

        arrivals = stops_data.get(sid, [])
        if arrivals:
            first_bus = arrivals[0]
            eta = first_bus.get("eta", "")
            b_num = f" (Bus #{first_bus['vehicle_id']})" if first_bus.get("vehicle_id") else ""
            draw.text((bx0 + 14, bus_y0 + header_h + 28), f"Next: {eta}{b_num}", fill="black", font=font_bus_eta)
            if len(arrivals) > 1:
                next_eta = arrivals[1].get("eta", "")
                draw.text((bx0 + 14, bus_y0 + header_h + 45), f"Following: {next_eta}", fill="#555555", font=font_bus_meta)
        else:
            draw.text((bx0 + 14, bus_y0 + header_h + 30), "No buses tracked in next hour", fill="#666666", font=font_bus_eta)

    # 4. Footer
    footer_y = 452
    draw.line([(20, footer_y), (WIDTH - 20, footer_y)], fill="black", width=1)

    sync_status = "● AM CITI BIKE HERO"
    draw.text((20, footer_y + 9), sync_status, fill="black", font=font_footer)

    center_text = f"Double-tap: Exit  •  Tap: Light  •  Last Synced: {now_time_str}"
    cb = draw.textbbox((0, 0), center_text, font=font_footer)
    cw = cb[2] - cb[0]
    draw.text(((WIDTH - cw) // 2, footer_y + 9), center_text, fill="#555555", font=font_footer)

    if batt_level is not None:
        charge_str = " (CHARGING)" if is_charging else ""
        right_text = f"BATTERY: {batt_level}%{charge_str}  •  READY"
    else:
        right_text = "E-INK DISPLAY READY"
    rb = draw.textbbox((0, 0), right_text, font=font_footer)
    rw = rb[2] - rb[0]
    draw.text((WIDTH - 20 - rw, footer_y + 9), right_text, fill="black", font=font_footer)


def render_evening_view(
    draw: ImageDraw.ImageDraw,
    stops_data: Dict[str, List[Dict[str, Any]]],
    citibike_data: List[Dict[str, Any]],
    now: datetime,
    batt_level: Optional[int],
    is_charging: bool,
    is_mock: bool,
    view_mode: str = "auto",
):
    """
    Afternoon / Evening View:
    NJ Transit 126 bus arrivals are the primary hero display with large minute countdowns
    and decision badges. Citi Bike dock inventory is summarized across the bottom.
    """
    has_citibike = bool(citibike_data)

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
    draw.rounded_rectangle([20, 14, 75, 46], radius=6, fill="black")
    draw.text((31, 18), "126", fill="white", font=font_title)

    draw.text((88, 15), "HOBOKEN → NYC PORT AUTHORITY", fill="black", font=font_title)
    sub_title = "NJ TRANSIT 126 & CITI BIKE LIVE TRACKER" if has_citibike else "NJ TRANSIT REAL-TIME TRACKER"
    draw.text((88, 39), sub_title, fill="#555555", font=font_header_sub)

    time_bbox = draw.textbbox((0, 0), now_time_str, font=font_time)
    time_w = time_bbox[2] - time_bbox[0]
    time_x = WIDTH - 20 - time_w
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
    draw.text((WIDTH - 20 - date_w, 39), now_date_str, fill="#555555", font=font_header_sub)

    draw.line([(20, 62), (WIDTH - 20, 62)], fill="black", width=2)

    # 2. Dual Bus Cards (Side-by-Side)
    col_w = 370
    col_h = 276 if has_citibike else 345
    card_y = 70 if has_citibike else 80
    xs = [20, 410]

    for i, stop_cfg in enumerate(STOPS):
        stop_id = stop_cfg["id"]
        x0 = xs[i]
        x1 = x0 + col_w
        y0 = card_y
        y1 = y0 + col_h

        draw.rounded_rectangle([x0, y0, x1, y1], radius=10, fill="white", outline="black", width=2)

        pill_h = 44 if has_citibike else 52
        draw.rounded_rectangle([x0, y0, x1, y0 + pill_h], radius=10, fill="#f2f2f2", outline="black", width=2)
        draw.rectangle([x0 + 1, y0 + pill_h - 12, x1 - 1, y0 + pill_h], fill="#f2f2f2")
        draw.line([(x0, y0 + pill_h), (x1, y0 + pill_h)], fill="black", width=2)

        draw.text((x0 + 12, y0 + (5 if has_citibike else 8)), stop_cfg["name"].upper(), fill="black", font=font_stop_name)
        draw.text((x0 + 12, y0 + (26 if has_citibike else 32)), stop_cfg["subtitle"], fill="#555555", font=font_header_sub)

        walk_text = f"{stop_cfg['walk_min']} MIN WALK"
        wb = draw.textbbox((0, 0), walk_text, font=font_walk)
        ww = wb[2] - wb[0]
        walk_btn_h = 22 if has_citibike else 24
        walk_btn_y = y0 + (11 if has_citibike else 14)
        draw.rounded_rectangle([x1 - ww - 20, walk_btn_y, x1 - 10, walk_btn_y + walk_btn_h], radius=5, fill="black")
        draw.text((x1 - ww - 15, walk_btn_y + (4 if has_citibike else 5)), walk_text, fill="white", font=font_walk)

        arrivals = stops_data.get(stop_id, [])
        if not arrivals:
            draw.text((x0 + 24, y0 + (70 if has_citibike else 120)), "NO BUSES IN NEXT HOUR", fill="#444444", font=font_stop_name if has_citibike else font_title)
            draw.text(
                (x0 + 24, y0 + (100 if has_citibike else 155)),
                "Off-peak schedule active or\nno buses currently tracked.",
                fill="#666666",
                font=font_detail,
            )
            box_h0 = y1 - (66 if has_citibike else 85)
            box_h1 = y1 - (12 if has_citibike else 20)
            draw.rounded_rectangle(
                [x0 + 14, box_h0, x1 - 14, box_h1],
                radius=7,
                fill="#fafafa",
                outline="#bbbbbb",
                width=1,
            )
            draw.text((x0 + 24, box_h0 + (16 if has_citibike else 23)), "Tip: Check NJ Transit app for daily timetables", fill="#666666", font=font_footer)
        else:
            first_bus = arrivals[0]
            first_eta_raw = first_bus.get("eta", "")
            first_min = parse_minutes(first_eta_raw)
            walk_min = stop_cfg["walk_min"]

            cy = y0 + (54 if has_citibike else 68)
            if first_min is not None:
                if first_min == 0:
                    cd_str = "DUE"
                    unit_str = ""
                else:
                    cd_str = str(first_min)
                    unit_str = "MIN"
            else:
                cd_str = first_eta_raw[:5] if first_eta_raw else "--"
                unit_str = ""

            draw.text((x0 + 14, cy), cd_str, fill="black", font=font_countdown_num)
            c_bbox = draw.textbbox((x0 + 14, cy), cd_str, font=font_countdown_num)
            unit_x = c_bbox[2] + 6

            if unit_str:
                draw.text((unit_x, cy + (34 if has_citibike else 40)), unit_str, fill="black", font=font_countdown_unit)

            badge_y = cy + (14 if has_citibike else 18)
            if first_min is not None:
                if first_min <= walk_min:
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

            sched_str = first_eta_raw
            draw.text((x0 + 16, cy + (74 if has_citibike else 82)), f"Estimated: {sched_str}", fill="#333333", font=font_detail)

            bus_num = first_bus.get("vehicle_id")
            load = first_bus.get("occupancy")
            load_clean = load.replace("_", " ").title() if load and load != "EMPTY" else "Seats Available"
            bus_meta = f"Bus #{bus_num}  •  {load_clean}" if bus_num else f"Status: {load_clean}"
            draw.text((x0 + 16, cy + (94 if has_citibike else 104)), bus_meta, fill="#444444", font=font_detail)

            box_y0 = y1 - (66 if has_citibike else 85)
            box_y1 = y1 - (12 if has_citibike else 16)
            draw.rounded_rectangle([x0 + 12, box_y0, x1 - 12, box_y1], radius=7, fill="#f8f8f8", outline="black", width=1)

            if len(arrivals) > 1:
                next_bus = arrivals[1]
                next_eta = next_bus.get("eta", "Scheduled")
                next_bus_num = f" (Bus #{next_bus['vehicle_id']})" if next_bus.get("vehicle_id") else ""
                draw.text((x0 + 20, box_y0 + (9 if has_citibike else 12)), "NEXT UPCOMING BUS:", fill="#555555", font=font_walk)
                draw.text((x0 + 20, box_y0 + (27 if has_citibike else 32)), f"126 to NYC → {next_eta}{next_bus_num}", fill="black", font=font_detail_bold)
            else:
                draw.text((x0 + 20, box_y0 + (9 if has_citibike else 12)), "NEXT UPCOMING BUS:", fill="#555555", font=font_walk)
                draw.text((x0 + 20, box_y0 + (27 if has_citibike else 32)), "No further buses in next 60 min", fill="#666666", font=font_detail)

    # 3. Citi Bike Bottom Section
    if has_citibike:
        cb_x0, cb_y0, cb_x1, cb_y1 = 20, 354, 780, 442
        draw.rounded_rectangle([cb_x0, cb_y0, cb_x1, cb_y1], radius=10, fill="white", outline="black", width=2)

        header_h = 24
        draw.rounded_rectangle([cb_x0, cb_y0, cb_x1, cb_y0 + header_h], radius=10, fill="#f4f4f4", outline="black", width=2)
        draw.rectangle([cb_x0 + 1, cb_y0 + 14, cb_x1 - 1, cb_y0 + header_h], fill="#f4f4f4")
        draw.line([(cb_x0, cb_y0 + header_h), (cb_x1, cb_y0 + header_h)], fill="black", width=1)
        draw.text((cb_x0 + 14, cb_y0 + 5), "CITI BIKE • CLOSEST DOCKS TO 919 PARK AVE", fill="#444444", font=font_cb_tag)

        body_y0 = cb_y0 + header_h
        col_count = len(citibike_data)
        col_w = (cb_x1 - cb_x0) // col_count

        for i, c in enumerate(citibike_data):
            cx0 = cb_x0 + i * col_w
            cx1 = cx0 + col_w
            if i > 0:
                draw.line([(cx0, body_y0 + 6), (cx0, cb_y1 - 6)], fill="#dddddd", width=1)

            draw.text((cx0 + 14, body_y0 + 8), c["name"].upper(), fill="black", font=font_cb_name)
            walk_str = f"{c['walk_min']} MIN"
            wb = draw.textbbox((0, 0), walk_str, font=font_cb_walk)
            ww = wb[2] - wb[0]
            draw.rounded_rectangle([cx1 - ww - 18, body_y0 + 6, cx1 - 10, body_y0 + 22], radius=4, fill="#eeeeee", outline="black", width=1)
            draw.text((cx1 - ww - 14, body_y0 + 8), walk_str, fill="black", font=font_cb_walk)

            if c.get("is_offline"):
                draw.text((cx0 + 14, body_y0 + 27), "STATION OFFLINE", fill="#777777", font=font_cb_stat)
                draw.text((cx0 + 14, body_y0 + 44), "Temporarily unavailable", fill="#777777", font=font_cb_sub)
            else:
                stat_str = f"{c['ebikes']} Ebikes  •  {c['classic']} Classic"
                draw.text((cx0 + 14, body_y0 + 27), stat_str, fill="black", font=font_cb_stat)
                docks_str = f"{c['docks']} Docks available"
                draw.text((cx0 + 14, body_y0 + 44), docks_str, fill="#555555", font=font_cb_sub)

    # 4. Footer
    footer_y = 452 if has_citibike else 442
    draw.line([(20, footer_y), (WIDTH - 20, footer_y)], fill="black", width=1)

    sync_status = "● PM BUS HERO"
    draw.text((20, footer_y + 9), sync_status, fill="black", font=font_footer)

    center_text = f"Double-tap: Exit  •  Tap: Light  •  Last Synced: {now_time_str}"
    cb = draw.textbbox((0, 0), center_text, font=font_footer)
    cw = cb[2] - cb[0]
    draw.text(((WIDTH - cw) // 2, footer_y + 9), center_text, fill="#555555", font=font_footer)

    if batt_level is not None:
        charge_str = " (CHARGING)" if is_charging else ""
        right_text = f"BATTERY: {batt_level}%{charge_str}  •  READY"
    else:
        right_text = "E-INK DISPLAY READY"
    rb = draw.textbbox((0, 0), right_text, font=font_footer)
    rw = rb[2] - rb[0]
    draw.text((WIDTH - 20 - rw, footer_y + 9), right_text, fill="black", font=font_footer)


def render_dashboard(
    stops_data: Dict[str, List[Dict[str, Any]]],
    citibike_data: Optional[List[Dict[str, Any]]] = None,
    output_path: str = "dashboard.png",
    view: str = "auto",
    is_mock: bool = False,
    batt_level: Optional[int] = None,
    is_charging: bool = False,
) -> str:
    """
    Renders an 800x480 high-contrast black-and-white image
    optimized for e-ink or low-power dashboard screens.
    Supports 'morning' (Citi Bike Hero) and 'evening' (Bus Hero) view modes.
    """
    if citibike_data is None:
        try:
            tracker = CitiBikeTracker()
            citibike_data = tracker.get_mock_data() if is_mock else tracker.get_station_status()
        except Exception:
            citibike_data = []

    active_view = resolve_view(view)

    # Create white canvas
    img = Image.new("RGB", (WIDTH, HEIGHT), color="white")
    draw = ImageDraw.Draw(img)
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
            view_mode=view,
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
            view_mode=view,
        )

    # Save output
    img.save(output_path, "PNG")
    print(f"✓ Dashboard image successfully rendered [{active_view.upper()} VIEW]: {output_path} ({WIDTH}x{HEIGHT})")
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

    if use_mock:
        print("Rendering with mock peak-commute data...")
        stops_data = get_mock_data()
    else:
        print("Fetching live data from NJ Transit API...")
        try:
            tracker = NJTransitBusTracker()
            # Map stop ID directly to arrivals
            for stop in STOPS:
                sid = stop["id"]
                trips = tracker.get_arrivals(stop_id=sid, route="126")
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
                stops_data[sid] = arrivals
        except Exception as e:
            print(f"Live fetch error ({e}), falling back to mock preview data...")
            stops_data = get_mock_data()
            use_mock = True

    render_dashboard(stops_data, output_path="dashboard.png", view=view_arg, is_mock=use_mock)
