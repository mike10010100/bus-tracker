package main

import (
	"strconv"
	"strings"
)

// PanelSize holds a framebuffer's landscape dimensions in pixels.
type PanelSize struct {
	LandscapeW int
	LandscapeH int
}

// PW5Landscape is the known Kindle Paperwhite 5 landscape panel, used as the
// fallback when the framebuffer size cannot be read from the device.
var PW5Landscape = PanelSize{LandscapeW: 1648, LandscapeH: 1236}

// landscapeFromPortrait converts native (portrait) framebuffer dimensions to
// landscape by ensuring width >= height.
func landscapeFromPortrait(w, h int) PanelSize {
	if h >= w {
		return PanelSize{LandscapeW: h, LandscapeH: w}
	}
	return PanelSize{LandscapeW: w, LandscapeH: h}
}

// parseFramebufferSize parses /sys/class/graphics/fb0/virtual_size, formatted
// as "W,H" in the panel's native portrait orientation, and returns landscape
// dimensions. Returns false when unparseable.
func parseFramebufferSize(raw string) (PanelSize, bool) {
	fields := strings.Split(strings.TrimSpace(raw), ",")
	if len(fields) != 2 {
		return PanelSize{}, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(fields[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return PanelSize{}, false
	}
	return landscapeFromPortrait(w, h), true
}

// parsePanelSpec accepts either "W,H" (virtual_size) or a fb0/modes string
// containing a "WxH" token (e.g. "U:1236x1648p-0") and returns landscape
// dimensions.
func parsePanelSpec(raw string) (PanelSize, bool) {
	if size, ok := parseFramebufferSize(raw); ok {
		return size, true
	}
	for _, tok := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ':' || r == ' ' || r == '\n' || r == '\t' || r == '-'
	}) {
		tok = strings.TrimRight(tok, "pU")
		parts := strings.SplitN(tok, "x", 2)
		if len(parts) != 2 {
			continue
		}
		w, err1 := strconv.Atoi(parts[0])
		h, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil && w > 0 && h > 0 {
			return landscapeFromPortrait(w, h), true
		}
	}
	return PanelSize{}, false
}

// DetectPanelSize reads the e-ink framebuffer dimensions, falling back to the
// known Paperwhite 5 panel when the sysfs nodes are unavailable or malformed.
//
// fb0/modes is preferred because it reports the true visible resolution
// (xres x yres). fb0/virtual_size must NOT be trusted directly: it describes
// the backing buffer, which on the PW5 is double-buffered and height-aligned
// (e.g. "1248,3296" for a 1236x1648 panel). Any implausible value is rejected
// by plausiblePanel.
func DetectPanelSize() PanelSize {
	for _, path := range []string{
		"/sys/class/graphics/fb0/modes",
		"/sys/class/graphics/fb0/virtual_size",
	} {
		if data, err := osReadFile(path); err == nil {
			if size, ok := parsePanelSpec(string(data)); ok && plausiblePanel(size) {
				return size
			}
		}
	}
	return PW5Landscape
}

// plausiblePanel rejects buffer-sized or misparsed dimensions. E-ink panels in
// scope are roughly 4:3 and a few hundred to ~2000px per side.
func plausiblePanel(p PanelSize) bool {
	if p.LandscapeW < 600 || p.LandscapeW > 2200 {
		return false
	}
	if p.LandscapeH < 400 || p.LandscapeH > 1800 {
		return false
	}
	ratio := float64(p.LandscapeW) / float64(p.LandscapeH)
	return ratio >= 1.2 && ratio <= 1.6
}
