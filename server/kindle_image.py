from typing import Any, Optional

from PIL import Image

# Kindle Paperwhite 5 native framebuffer (portrait). Landscape = 1648x1236.
PW5_NATIVE: tuple[int, int] = (1236, 1648)
PW5_LANDSCAPE: tuple[int, int] = (1648, 1236)


def native_render_scale(
    landscape_w: int = 1648, landscape_h: int = 1236, logical_w: int = 800
) -> float:
    """
    Computes the scale factor that maps the logical 800px design space onto a
    native landscape panel. We render at this scale so glyphs are rasterized
    natively rather than upscaled from an 800px bitmap.
    """
    return landscape_w / logical_w


def sanitize_kindle_panel(w: Any, h: Any) -> tuple[int, int]:
    """
    Validates client-reported landscape panel dimensions, falling back to the
    PW5 default when they are implausible. Guards against a client that reports
    a backing-buffer size (double-buffered / height-aligned, e.g. 3296x1248)
    rather than the true visible resolution, which would render a distorted,
    needlessly huge canvas.
    """
    try:
        w = int(w)
        h = int(h)
    except (TypeError, ValueError):
        return PW5_LANDSCAPE
    if not (600 <= w <= 2200 and 400 <= h <= 1800):
        return PW5_LANDSCAPE
    ratio = w / h
    if not (1.2 <= ratio <= 1.6):
        return PW5_LANDSCAPE
    return (w, h)


def format_for_kindle(
    base_img: Image.Image,
    orientation: str = "landscape",
    rotation: int = 90,
    target: Optional[tuple[int, int]] = None,
) -> Image.Image:
    """
    Prepares an already-rendered dashboard for the Kindle Paperwhite 5.

    When the server renders natively (the base image is already the exact
    landscape panel size) this only rotates it to portrait and converts to
    8-bit grayscale -- no resampling. For backward compatibility with callers
    that pass an 800x480/800x600 base image, it falls back to a single LANCZOS
    fit into the target landscape size.
    """
    if orientation == "landscape":
        target_w, target_h = target or PW5_LANDSCAPE
        if (base_img.width, base_img.height) == (target_w, target_h):
            canvas = base_img
        else:
            ratio = min(target_w / base_img.width, target_h / base_img.height)
            new_w = int(base_img.width * ratio)
            new_h = int(base_img.height * ratio)
            resized = base_img.resize((new_w, new_h), Image.Resampling.LANCZOS)
            canvas = Image.new("RGB", (target_w, target_h), "white")
            offset_x = (target_w - new_w) // 2
            offset_y = (target_h - new_h) // 2
            canvas.paste(resized, (offset_x, offset_y))

        # Rotate to match Kindle's portrait framebuffer
        if rotation != 0:
            canvas = canvas.rotate(rotation, expand=True)

        # Kindle's eips expects 8-bit grayscale ('L')
        # If given RGB, eips reads 3 bytes per pixel, squishing the image by 3x!
        return canvas.convert("L")
    return base_img.convert("L")
