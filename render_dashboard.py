import os
import re
import sys
import time
from datetime import datetime
from typing import Dict, List, Any, Optional
from PIL import Image, ImageDraw, ImageFont

from bus_tracker import NJTransitBusTracker

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


def render_dashboard(
    stops_data: Dict[str, List[Dict[str, Any]]],
    output_path: str = "dashboard.png",
    is_mock: bool = False,
    batt_level: Optional[int] = None,
    is_charging: bool = False,
):
    """
    Renders an 800x480 high-contrast black-and-white image
    optimized for e-ink or low-power dashboard screens.
    """
    # Create white canvas
    img = Image.new("RGB", (WIDTH, HEIGHT), color="white")
    draw = ImageDraw.Draw(img)

    # Load fonts
    font_title = get_font(22, bold=True)
    font_header_sub = get_font(13, bold=False)
    font_time = get_font(18, bold=True)
    font_stop_name = get_font(22, bold=True)
    font_walk = get_font(13, bold=True)
    font_countdown_num = get_font(74, bold=True)
    font_countdown_unit = get_font(24, bold=True)
    font_badge = get_font(13, bold=True)
    font_detail = get_font(14, bold=False)
    font_detail_bold = get_font(14, bold=True)
    font_footer = get_font(12, bold=False)

    now = datetime.now()
    now_time_str = now.strftime("%-I:%M %p")
    now_date_str = now.strftime("%A, %b %-d")

    # ==========================================
    # 1. TOP HEADER BAR
    # ==========================================
    # Route pill badge
    draw.rounded_rectangle([20, 16, 75, 48], radius=6, fill="black")
    draw.text((31, 21), "126", fill="white", font=font_title)

    # Route title
    draw.text((88, 17), "HOBOKEN → NYC PORT AUTHORITY", fill="black", font=font_title)
    draw.text((88, 43), "NJ TRANSIT REAL-TIME TRACKER", fill="#555555", font=font_header_sub)

    # Clock on right
    time_bbox = draw.textbbox((0, 0), now_time_str, font=font_time)
    time_w = time_bbox[2] - time_bbox[0]
    time_x = WIDTH - 20 - time_w
    draw.text((time_x, 16), now_time_str, fill="black", font=font_time)

    # Battery indicator to the left of the clock (if available)
    if batt_level is not None:
        font_batt = get_font(13, bold=True)
        label = f"{batt_level}%"
        bbox = draw.textbbox((0, 0), label, font=font_batt)
        label_w = bbox[2] - bbox[0]
        bolt_w = 12 if is_charging else 0
        total_batt_w = bolt_w + label_w + 6 + 28 + 3
        batt_x = time_x - total_batt_w - 18
        draw_battery_indicator(draw, batt_x, 18, batt_level, is_charging=is_charging, font=font_batt)

    # Date beneath clock
    date_bbox = draw.textbbox((0, 0), now_date_str, font=font_header_sub)
    date_w = date_bbox[2] - date_bbox[0]
    draw.text((WIDTH - 20 - date_w, 42), now_date_str, fill="#555555", font=font_header_sub)

    # Header Divider Line
    draw.line([(20, 68), (WIDTH - 20, 68)], fill="black", width=2)

    # ==========================================
    # 2. DUAL CARDS (Side-by-Side)
    # ==========================================
    col_w = 370
    col_h = 345
    card_y = 80
    xs = [20, 410]  # Left and right column start coordinates

    for i, stop_cfg in enumerate(STOPS):
        stop_id = stop_cfg["id"]
        x0 = xs[i]
        x1 = x0 + col_w
        y0 = card_y
        y1 = y0 + col_h

        # Outer Card Border
        draw.rounded_rectangle([x0, y0, x1, y1], radius=10, fill="white", outline="black", width=2)

        # Card Header Pill (Stop Name + Walk Time)
        draw.rounded_rectangle([x0, y0, x1, y0 + 52], radius=10, fill="#f2f2f2", outline="black", width=2)
        # Fix bottom corners of header pill so it joins cleanly
        draw.rectangle([x0 + 1, y0 + 40, x1 - 1, y0 + 52], fill="#f2f2f2")
        draw.line([(x0, y0 + 52), (x1, y0 + 52)], fill="black", width=2)

        draw.text((x0 + 14, y0 + 8), stop_cfg["name"].upper(), fill="black", font=font_stop_name)
        draw.text((x0 + 14, y0 + 32), stop_cfg["subtitle"], fill="#555555", font=font_header_sub)

        # Walk time indicator on the right of card header
        walk_text = f"{stop_cfg['walk_min']} MIN WALK"
        wb = draw.textbbox((0, 0), walk_text, font=font_walk)
        ww = wb[2] - wb[0]
        draw.rounded_rectangle([x1 - ww - 22, y0 + 14, x1 - 12, y0 + 38], radius=6, fill="black")
        draw.text((x1 - ww - 17, y0 + 19), walk_text, fill="white", font=font_walk)

        # Get arrivals for this stop
        arrivals = stops_data.get(stop_id, [])

        if not arrivals:
            # Empty / No bus state
            draw.text((x0 + 30, y0 + 120), "NO BUSES IN NEXT HOUR", fill="#444444", font=font_title)
            draw.text(
                (x0 + 30, y0 + 155),
                "Off-peak schedule active or\nno buses currently tracked.",
                fill="#666666",
                font=font_detail,
            )
            # Dotted placeholder box
            draw.rounded_rectangle(
                [x0 + 16, y1 - 85, x1 - 16, y1 - 20],
                radius=8,
                fill="#fafafa",
                outline="#bbbbbb",
                width=1,
            )
            draw.text((x0 + 30, y1 - 62), "Tip: Check NJ Transit app for daily timetables", fill="#666666", font=font_footer)
        else:
            first_bus = arrivals[0]
            first_eta_raw = first_bus.get("eta", "")
            first_min = parse_minutes(first_eta_raw)
            walk_min = stop_cfg["walk_min"]

            # Large Countdown Display
            cy = y0 + 68
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

            draw.text((x0 + 16, cy), cd_str, fill="black", font=font_countdown_num)
            c_bbox = draw.textbbox((x0 + 16, cy), cd_str, font=font_countdown_num)
            unit_x = c_bbox[2] + 8

            if unit_str:
                draw.text((unit_x, cy + 40), unit_str, fill="black", font=font_countdown_unit)

            # Decision / Action Badge (RUN / LEAVE NOW / ON TIME)
            badge_y = cy + 18
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
                badge_x = x1 - bw - 28
                draw.rounded_rectangle(
                    [badge_x, badge_y, badge_x + bw + 16, badge_y + 28],
                    radius=6,
                    fill=badge_bg,
                    outline="black",
                    width=2,
                )
                draw.text((badge_x + 8, badge_y + 6), badge_label, fill=badge_fg, font=font_badge)

            # Scheduled / Expected time
            sched_str = first_eta_raw
            draw.text((x0 + 18, cy + 82), f"Estimated: {sched_str}", fill="#333333", font=font_detail)

            # Bus Number & Occupancy
            bus_num = first_bus.get("vehicle_id")
            load = first_bus.get("occupancy")
            load_clean = load.replace("_", " ").title() if load and load != "EMPTY" else "Seats Available"
            bus_meta = f"Bus #{bus_num}  •  {load_clean}" if bus_num else f"Status: {load_clean}"
            draw.text((x0 + 18, cy + 104), bus_meta, fill="#444444", font=font_detail)

            # Secondary Box (Upcoming Next Bus)
            box_y0 = y1 - 85
            box_y1 = y1 - 16
            draw.rounded_rectangle([x0 + 14, box_y0, x1 - 14, box_y1], radius=8, fill="#f8f8f8", outline="black", width=1)

            if len(arrivals) > 1:
                next_bus = arrivals[1]
                next_eta = next_bus.get("eta", "Scheduled")
                next_bus_num = f" (Bus #{next_bus['vehicle_id']})" if next_bus.get("vehicle_id") else ""
                draw.text((x0 + 26, box_y0 + 12), "NEXT UPCOMING BUS:", fill="#555555", font=font_walk)
                draw.text((x0 + 26, box_y0 + 32), f"126 to NYC → {next_eta}{next_bus_num}", fill="black", font=font_detail_bold)
            else:
                draw.text((x0 + 26, box_y0 + 12), "NEXT UPCOMING BUS:", fill="#555555", font=font_walk)
                draw.text((x0 + 26, box_y0 + 32), "No further buses in next 60 min", fill="#666666", font=font_detail)

    # ==========================================
    # 3. FOOTER STATUS BAR
    # ==========================================
    footer_y = 442
    draw.line([(20, footer_y), (WIDTH - 20, footer_y)], fill="black", width=1)

    sync_status = "● LIVE FEED CONNECTED" if not is_mock else "● PREVIEW MODE (MOCK DATA)"
    draw.text((20, footer_y + 12), sync_status, fill="black", font=font_footer)

    center_text = f"Double-tap: Exit  •  Tap: Light  •  Last Synced: {now.strftime('%-I:%M %p')}"
    cb = draw.textbbox((0, 0), center_text, font=font_footer)
    cw = cb[2] - cb[0]
    draw.text(((WIDTH - cw) // 2, footer_y + 12), center_text, fill="#555555", font=font_footer)

    if batt_level is not None:
        charge_str = " (CHARGING)" if is_charging else ""
        right_text = f"BATTERY: {batt_level}%{charge_str}  •  READY"
    else:
        right_text = "E-INK DISPLAY READY"
    rb = draw.textbbox((0, 0), right_text, font=font_footer)
    rw = rb[2] - rb[0]
    draw.text((WIDTH - 20 - rw, footer_y + 12), right_text, fill="black", font=font_footer)

    # Save output
    img.save(output_path, "PNG")
    print(f"✓ Dashboard image successfully rendered: {output_path} ({WIDTH}x{HEIGHT})")
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

    render_dashboard(stops_data, output_path="dashboard.png", is_mock=use_mock)
