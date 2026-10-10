package main

import (
	"sync"
	"time"
)

// GestureDetectorConfig holds timing and threshold settings for touch recognition.
//
// All thresholds are expressed in the renderer's *design* coordinate space: an
// 800px-wide landscape layout (see render_dashboard.py). Raw touch events arrive
// in the panel's portrait framebuffer space and are mapped into this space by
// Transform before hit-testing, so the zones always match what is drawn.
type GestureDetectorConfig struct {
	DoubleTapWindow   time.Duration
	SingleTapDelay    time.Duration
	InactivityTimeout time.Duration
	DebounceDuration  time.Duration
	// Transform maps a raw touch coordinate to design space. Nil = identity.
	Transform             func(x, y int32) (int32, int32)
	TopRightThresholdX    int32
	TopRightThresholdY    int32
	TopLeftThresholdX     int32
	TopLeftThresholdY     int32
	BottomLeftThresholdX  int32
	BottomLeftThresholdY  int32
	BottomBarThresholdY   int32
	ButtonBusesThresholdX int32
	ButtonBikesThresholdX int32
	ButtonLightThresholdX int32
}

// DesignWidth/DesignHeightMirror the renderer's logical layout for the PW5
// panel (800 wide, and 600 tall for the 1648x1236 panel). The bottom button bar
// is drawn at y in [DesignHeight-44, DesignHeight-10] with five equal columns.
const DesignWidth = 800

// DefaultGestureConfig returns production settings in design space, matching
// the 800x600 layout used on the Paperwhite 5.
func DefaultGestureConfig() GestureDetectorConfig {
	return GestureDetectorConfig{
		DoubleTapWindow:   380 * time.Millisecond,
		SingleTapDelay:    200 * time.Millisecond,
		InactivityTimeout: 100 * time.Millisecond,
		DebounceDuration:  80 * time.Millisecond,
		// Bottom button bar (design y ~= 556..590 for a 600-tall layout). Four
		// buttons: BUSES | CITI BIKE | LIGHT | REFRESH. Column boundaries mirror
		// draw_bottom_button_bar (start_x=20, gap=10, col_w=182).
		BottomBarThresholdY:   556,
		ButtonBusesThresholdX: 202,
		ButtonBikesThresholdX: 394,
		ButtonLightThresholdX: 586,
		// Corner zones in design space.
		TopRightThresholdX:   640,
		TopRightThresholdY:   120,
		TopLeftThresholdX:    160,
		TopLeftThresholdY:    120,
		BottomLeftThresholdX: 160,
		BottomLeftThresholdY: 480,
	}
}

// GestureDetector handles tap recognition, gesture classification, and debouncing
type GestureDetector struct {
	cfg             GestureDetectorConfig
	curX            int32
	curY            int32
	rawX            int32
	rawY            int32
	touchActive     bool
	inactivityTimer *time.Timer
	singleTapTimer  *time.Timer
	lastTapTime     time.Time
	lastTriggerTime time.Time
	mu              sync.Mutex

	OnSingleTap     func(x, y int32)
	OnDoubleTap     func(x, y int32)
	OnTopRightTap   func(x, y int32)
	OnTopLeftTap    func(x, y int32)
	OnBottomLeftTap func(x, y int32)
	OnBusesTap      func(x, y int32)
	OnBikesTap      func(x, y int32)
	OnLightTap      func(x, y int32)
	OnRefreshTap    func(x, y int32)
	OnLog           func(msg string)
}

// NewGestureDetector creates an initialized GestureDetector
func NewGestureDetector(cfg GestureDetectorConfig) *GestureDetector {
	return &GestureDetector{
		cfg: cfg,
	}
}

func (gd *GestureDetector) log(msg string) {
	if gd.OnLog != nil {
		gd.OnLog(msg)
	}
}

// TriggerTap processes a completed tap at the given timestamp
func (gd *GestureDetector) TriggerTap(now time.Time) {
	gd.mu.Lock()
	defer gd.mu.Unlock()

	// Debounce rapid duplicate trigger within threshold
	if !gd.lastTriggerTime.IsZero() && now.Sub(gd.lastTriggerTime) < gd.cfg.DebounceDuration {
		return
	}
	gd.lastTriggerTime = now

	x, y := gd.curX, gd.curY

	// 1. Bottom Button Bar (Interactive Tactile Buttons)
	if gd.cfg.BottomBarThresholdY > 0 && y >= gd.cfg.BottomBarThresholdY {
		if gd.singleTapTimer != nil {
			gd.singleTapTimer.Stop()
		}
		gd.lastTapTime = time.Time{}

		switch {
		case x < gd.cfg.ButtonBusesThresholdX:
			if gd.OnBusesTap != nil {
				gd.OnBusesTap(x, y)
				return
			}
			if gd.OnBottomLeftTap != nil {
				gd.OnBottomLeftTap(x, y)
				return
			}
		case x < gd.cfg.ButtonBikesThresholdX:
			if gd.OnBikesTap != nil {
				gd.OnBikesTap(x, y)
				return
			}
			if gd.OnBottomLeftTap != nil {
				gd.OnBottomLeftTap(x, y)
				return
			}
		case x < gd.cfg.ButtonLightThresholdX:
			if gd.OnLightTap != nil {
				gd.OnLightTap(x, y)
				return
			}
		default:
			if gd.OnRefreshTap != nil {
				gd.OnRefreshTap(x, y)
				return
			}
		}
		return
	}

	// 2. Corner Touch Gestures (Dedicated Action Zones)
	// Top-Right Corner Tap -> Immediate Exit
	if x > gd.cfg.TopRightThresholdX && y < gd.cfg.TopRightThresholdY {
		if gd.singleTapTimer != nil {
			gd.singleTapTimer.Stop()
		}
		gd.lastTapTime = time.Time{}
		if gd.OnTopRightTap != nil {
			gd.OnTopRightTap(x, y)
		}
		return
	}

	// Top-Left Corner Tap -> Force Immediate Refresh
	if x < gd.cfg.TopLeftThresholdX && y < gd.cfg.TopLeftThresholdY {
		if gd.singleTapTimer != nil {
			gd.singleTapTimer.Stop()
		}
		gd.lastTapTime = time.Time{}
		if gd.OnTopLeftTap != nil {
			gd.OnTopLeftTap(x, y)
		}
		return
	}

	// Bottom-Left Corner Tap -> Cycle View Mode (Auto -> Morning -> Evening)
	if x < gd.cfg.BottomLeftThresholdX && y > gd.cfg.BottomLeftThresholdY {
		if gd.singleTapTimer != nil {
			gd.singleTapTimer.Stop()
		}
		gd.lastTapTime = time.Time{}
		if gd.OnBottomLeftTap != nil {
			gd.OnBottomLeftTap(x, y)
		}
		return
	}

	// 2. Double-Tap Check
	if !gd.lastTapTime.IsZero() {
		sinceLast := now.Sub(gd.lastTapTime)
		if sinceLast < gd.cfg.DoubleTapWindow && sinceLast > gd.cfg.DebounceDuration/2 {
			if gd.singleTapTimer != nil {
				gd.singleTapTimer.Stop()
			}
			gd.lastTapTime = time.Time{} // Reset after double tap
			if gd.OnDoubleTap != nil {
				gd.OnDoubleTap(x, y)
			}
			return
		}
	}
	gd.lastTapTime = now

	// 3. Normal Single Tap
	if gd.singleTapTimer != nil {
		gd.singleTapTimer.Stop()
	}
	gd.singleTapTimer = time.AfterFunc(gd.cfg.SingleTapDelay, func() {
		if gd.OnSingleTap != nil {
			gd.OnSingleTap(x, y)
		}
	})
}

// ProcessEvent feeds an input event into the gesture recognizer
func (gd *GestureDetector) ProcessEvent(ev RawEventMsg) {
	// Coordinate extraction, mapped into design space so the zone thresholds
	// match the rendered layout.
	if rawX, rawY, updated := ExtractCoordinates(ev, gd.rawX, gd.rawY); updated {
		dx, dy := rawX, rawY
		if gd.cfg.Transform != nil {
			dx, dy = gd.cfg.Transform(rawX, rawY)
		}
		gd.mu.Lock()
		gd.rawX = rawX
		gd.rawY = rawY
		gd.curX = dx
		gd.curY = dy
		gd.mu.Unlock()
	}

	if !IsTouchEvent(ev) {
		return
	}

	gd.mu.Lock()
	gd.touchActive = true

	// Reset inactivity fallback timer
	if gd.inactivityTimer != nil {
		gd.inactivityTimer.Stop()
	}
	gd.inactivityTimer = time.AfterFunc(gd.cfg.InactivityTimeout, func() {
		gd.mu.Lock()
		if gd.touchActive {
			gd.touchActive = false
			gd.mu.Unlock()
			gd.TriggerTap(time.Now())
			return
		}
		gd.mu.Unlock()
	})
	gd.mu.Unlock()

	// Check explicit release
	if IsExplicitTouchRelease(ev) {
		gd.mu.Lock()
		if gd.inactivityTimer != nil {
			gd.inactivityTimer.Stop()
		}
		gd.touchActive = false
		gd.mu.Unlock()
		gd.TriggerTap(time.Now())
	}
}

// Stop cleanly cancels any pending timers
func (gd *GestureDetector) Stop() {
	gd.mu.Lock()
	defer gd.mu.Unlock()

	if gd.inactivityTimer != nil {
		gd.inactivityTimer.Stop()
	}
	if gd.singleTapTimer != nil {
		gd.singleTapTimer.Stop()
	}
}
