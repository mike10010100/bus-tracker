package main

import (
	"sync"
	"time"
)

// DesignWidth mirrors the renderer's logical layout for Kindle Paperwhite 5
const DesignWidth = 800

// GestureDetectorConfig specifies thresholds and timing windows for gesture recognition
type GestureDetectorConfig struct {
	BottomBarThresholdY   int32
	ButtonBusesThresholdX int32
	ButtonBikesThresholdX int32
	ButtonLightThresholdX int32

	TopRightThresholdX   int32
	TopRightThresholdY   int32
	TopLeftThresholdX    int32
	TopLeftThresholdY    int32
	BottomLeftThresholdX int32
	BottomLeftThresholdY int32

	DoubleTapWindow   time.Duration
	DebounceDuration  time.Duration
	SingleTapDelay    time.Duration
	InactivityTimeout time.Duration

	// Transform maps raw evdev coordinates to layout/design coordinates.
	// Injected by TrackerClient based on the detected panel geometry.
	Transform func(rawX, rawY int32) (designX, designY int32)
}

// DefaultGestureConfig constructs standard layout coordinates for Kindle Paperwhite 5
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
	tapEmitted      bool
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

// TriggerTap processes a completed tap at the given timestamp.
// Gesture callbacks run outside gd.mu to avoid blocking the event pump (finding L6).
func (gd *GestureDetector) TriggerTap(now time.Time) {
	gd.mu.Lock()

	// Debounce rapid duplicate trigger within threshold
	if !gd.lastTriggerTime.IsZero() && now.Sub(gd.lastTriggerTime) < gd.cfg.DebounceDuration {
		gd.mu.Unlock()
		return
	}
	gd.lastTriggerTime = now

	x, y := gd.curX, gd.curY
	var cb func(x, y int32)

	// 1. Bottom Button Bar (Interactive Tactile Buttons)
	if gd.cfg.BottomBarThresholdY > 0 && y >= gd.cfg.BottomBarThresholdY {
		if gd.singleTapTimer != nil {
			gd.singleTapTimer.Stop()
		}
		gd.lastTapTime = time.Time{}

		switch {
		case x < gd.cfg.ButtonBusesThresholdX:
			if gd.OnBusesTap != nil {
				cb = gd.OnBusesTap
			} else if gd.OnBottomLeftTap != nil {
				cb = gd.OnBottomLeftTap
			}
		case x < gd.cfg.ButtonBikesThresholdX:
			if gd.OnBikesTap != nil {
				cb = gd.OnBikesTap
			} else if gd.OnBottomLeftTap != nil {
				cb = gd.OnBottomLeftTap
			}
		case x < gd.cfg.ButtonLightThresholdX:
			if gd.OnLightTap != nil {
				cb = gd.OnLightTap
			}
		default:
			if gd.OnRefreshTap != nil {
				cb = gd.OnRefreshTap
			}
		}
		gd.mu.Unlock()
		if cb != nil {
			cb(x, y)
		}
		return
	}

	// 2. Corner Touch Gestures (Dedicated Action Zones)
	// Top-Right Corner Tap -> Refresh
	if x > gd.cfg.TopRightThresholdX && y < gd.cfg.TopRightThresholdY {
		if gd.singleTapTimer != nil {
			gd.singleTapTimer.Stop()
		}
		gd.lastTapTime = time.Time{}
		cb = gd.OnTopRightTap
		gd.mu.Unlock()
		if cb != nil {
			cb(x, y)
		}
		return
	}

	// Top-Left Corner Tap -> Force Immediate Refresh
	if x < gd.cfg.TopLeftThresholdX && y < gd.cfg.TopLeftThresholdY {
		if gd.singleTapTimer != nil {
			gd.singleTapTimer.Stop()
		}
		gd.lastTapTime = time.Time{}
		cb = gd.OnTopLeftTap
		gd.mu.Unlock()
		if cb != nil {
			cb(x, y)
		}
		return
	}

	// Bottom-Left Corner Tap -> Cycle View Mode (Auto -> Morning -> Evening)
	if x < gd.cfg.BottomLeftThresholdX && y > gd.cfg.BottomLeftThresholdY {
		if gd.singleTapTimer != nil {
			gd.singleTapTimer.Stop()
		}
		gd.lastTapTime = time.Time{}
		cb = gd.OnBottomLeftTap
		gd.mu.Unlock()
		if cb != nil {
			cb(x, y)
		}
		return
	}

	// Double-Tap Check
	if !gd.lastTapTime.IsZero() {
		sinceLast := now.Sub(gd.lastTapTime)
		if sinceLast < gd.cfg.DoubleTapWindow && sinceLast > gd.cfg.DebounceDuration/2 {
			if gd.singleTapTimer != nil {
				gd.singleTapTimer.Stop()
			}
			gd.lastTapTime = time.Time{} // Reset after double tap
			cb = gd.OnDoubleTap
			gd.mu.Unlock()
			if cb != nil {
				cb(x, y)
			}
			return
		}
	}
	gd.lastTapTime = now

	// Normal Single Tap
	if gd.singleTapTimer != nil {
		gd.singleTapTimer.Stop()
	}
	singleCb := gd.OnSingleTap
	gd.singleTapTimer = time.AfterFunc(gd.cfg.SingleTapDelay, func() {
		if singleCb != nil {
			singleCb(x, y)
		}
	})
	gd.mu.Unlock()
}

// ProcessEvent feeds an input event into the gesture recognizer.
// Uses per-contact tapEmitted flag to prevent double taps on hold-then-release (finding M13).
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

	isTouchDown := (ev.EvType == EV_KEY && (ev.EvCode == BTN_TOUCH || ev.EvCode == BTN_LEFT) && ev.EvValue == 1) ||
		(ev.EvType == EV_ABS && ev.EvCode == ABS_MT_TRACKING_ID && ev.EvValue >= 0)

	isTouchRelease := IsExplicitTouchRelease(ev)

	gd.mu.Lock()
	if isTouchDown {
		gd.tapEmitted = false
	}
	gd.touchActive = !isTouchRelease

	// Reset inactivity fallback timer
	if gd.inactivityTimer != nil {
		gd.inactivityTimer.Stop()
	}

	if !isTouchRelease {
		gd.inactivityTimer = time.AfterFunc(gd.cfg.InactivityTimeout, func() {
			gd.mu.Lock()
			if gd.touchActive && !gd.tapEmitted {
				gd.tapEmitted = true
				gd.touchActive = false
				gd.mu.Unlock()
				gd.TriggerTap(time.Now())
				return
			}
			gd.mu.Unlock()
		})
		gd.mu.Unlock()
		return
	}

	// Explicit touch release
	shouldTrigger := !gd.tapEmitted
	gd.tapEmitted = true
	gd.mu.Unlock()

	if shouldTrigger {
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
