package main

import (
	"sync"
	"testing"
	"time"
)

func TestGestureDetectorSingleTap(t *testing.T) {
	cfg := DefaultGestureConfig()
	cfg.SingleTapDelay = 30 * time.Millisecond
	cfg.InactivityTimeout = 15 * time.Millisecond
	cfg.DebounceDuration = 10 * time.Millisecond

	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	var (
		singleTaps int
		mu         sync.Mutex
	)
	gd.OnSingleTap = func(x, y int32) {
		mu.Lock()
		singleTaps++
		mu.Unlock()
	}

	now := time.Now()
	gd.curX = 500
	gd.curY = 500

	// Trigger single tap
	gd.TriggerTap(now)

	// Wait for single tap timer to fire
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	count := singleTaps
	mu.Unlock()

	if count != 1 {
		t.Errorf("Expected 1 single tap, got %d", count)
	}
}

func TestGestureDetectorDoubleTap(t *testing.T) {
	cfg := DefaultGestureConfig()
	cfg.DoubleTapWindow = 100 * time.Millisecond
	cfg.SingleTapDelay = 80 * time.Millisecond
	cfg.DebounceDuration = 10 * time.Millisecond

	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	var (
		singleTaps int
		doubleTaps int
		mu         sync.Mutex
	)
	gd.OnSingleTap = func(x, y int32) {
		mu.Lock()
		singleTaps++
		mu.Unlock()
	}
	gd.OnDoubleTap = func(x, y int32) {
		mu.Lock()
		doubleTaps++
		mu.Unlock()
	}

	gd.curX = 500
	gd.curY = 500

	t0 := time.Now()
	gd.TriggerTap(t0)

	// Second tap arrives 50ms later (well within DoubleTapWindow of 100ms)
	t1 := t0.Add(50 * time.Millisecond)
	gd.TriggerTap(t1)

	// Wait for any timers to resolve
	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	sCount := singleTaps
	dCount := doubleTaps
	mu.Unlock()

	if dCount != 1 {
		t.Errorf("Expected 1 double tap, got %d", dCount)
	}
	if sCount != 0 {
		t.Errorf("Expected 0 single taps on double-tap, got %d", sCount)
	}
}

func TestGestureDetectorSlowTapsDoNotDoubleTap(t *testing.T) {
	cfg := DefaultGestureConfig()
	cfg.DoubleTapWindow = 50 * time.Millisecond
	cfg.SingleTapDelay = 20 * time.Millisecond
	cfg.DebounceDuration = 10 * time.Millisecond

	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	var (
		singleTaps int
		doubleTaps int
		mu         sync.Mutex
	)
	gd.OnSingleTap = func(x, y int32) {
		mu.Lock()
		singleTaps++
		mu.Unlock()
	}
	gd.OnDoubleTap = func(x, y int32) {
		mu.Lock()
		doubleTaps++
		mu.Unlock()
	}

	gd.curX = 500
	gd.curY = 500

	t0 := time.Now()
	gd.TriggerTap(t0)

	time.Sleep(30 * time.Millisecond) // First single tap fires

	// Second tap arrives 70ms after first (outside DoubleTapWindow of 50ms)
	t1 := t0.Add(70 * time.Millisecond)
	gd.TriggerTap(t1)

	time.Sleep(40 * time.Millisecond) // Second single tap fires

	mu.Lock()
	sCount := singleTaps
	dCount := doubleTaps
	mu.Unlock()

	if dCount != 0 {
		t.Errorf("Slow taps should not trigger double tap, got %d", dCount)
	}
	if sCount != 2 {
		t.Errorf("Expected 2 separate single taps, got %d", sCount)
	}
}

func TestGestureDetectorCornerTaps(t *testing.T) {
	cfg := DefaultGestureConfig()
	cfg.DebounceDuration = 10 * time.Millisecond

	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	var (
		topRightTaps   int
		topLeftTaps    int
		bottomLeftTaps int
		mu             sync.Mutex
	)
	gd.OnTopRightTap = func(x, y int32) {
		mu.Lock()
		topRightTaps++
		mu.Unlock()
	}
	gd.OnTopLeftTap = func(x, y int32) {
		mu.Lock()
		topLeftTaps++
		mu.Unlock()
	}
	gd.OnBottomLeftTap = func(x, y int32) {
		mu.Lock()
		bottomLeftTaps++
		mu.Unlock()
	}

	// 1. Top-Right corner tap (design space)
	gd.curX = 700
	gd.curY = 50
	t0 := time.Now()
	gd.TriggerTap(t0)

	// 2. Top-Left corner tap
	gd.curX = 50
	gd.curY = 50
	t1 := t0.Add(50 * time.Millisecond)
	gd.TriggerTap(t1)

	// 3. Bottom-Left corner tap (above the button bar)
	gd.curX = 50
	gd.curY = 550
	t2 := t1.Add(50 * time.Millisecond)
	gd.TriggerTap(t2)

	mu.Lock()
	trCount := topRightTaps
	tlCount := topLeftTaps
	blCount := bottomLeftTaps
	mu.Unlock()

	if trCount != 1 {
		t.Errorf("Expected 1 top-right tap, got %d", trCount)
	}
	if tlCount != 1 {
		t.Errorf("Expected 1 top-left tap, got %d", tlCount)
	}
	if blCount != 1 {
		t.Errorf("Expected 1 bottom-left tap, got %d", blCount)
	}
}

func TestGestureDetectorInactivityFallback(t *testing.T) {
	cfg := DefaultGestureConfig()
	cfg.InactivityTimeout = 20 * time.Millisecond
	cfg.SingleTapDelay = 20 * time.Millisecond
	cfg.DebounceDuration = 5 * time.Millisecond

	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	var (
		taps int
		mu   sync.Mutex
	)
	gd.OnSingleTap = func(x, y int32) {
		mu.Lock()
		taps++
		mu.Unlock()
	}

	// Send EV_ABS position with NO explicit release event (pt_mt driver behavior).
	// Use a main-area point (design space) so it lands on a single tap.
	gd.ProcessEvent(RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_POSITION_X, EvValue: 400})
	gd.ProcessEvent(RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_POSITION_Y, EvValue: 300})

	// Wait for inactivity timer (20ms) + single tap delay (20ms)
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	count := taps
	mu.Unlock()

	if count != 1 {
		t.Errorf("Inactivity fallback should have triggered 1 tap, got %d", count)
	}
}

func TestGestureDetectorInactivityTimerAfterReleaseIsNoop(t *testing.T) {
	cfg := DefaultGestureConfig()
	cfg.InactivityTimeout = 25 * time.Millisecond
	cfg.SingleTapDelay = 10 * time.Millisecond
	cfg.DebounceDuration = 5 * time.Millisecond

	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	// Explicit release clears touchActive, so the pending inactivity timer must
	// find touchActive == false and do nothing (the else branch).
	gd.ProcessEvent(RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_POSITION_X, EvValue: 100})
	gd.ProcessEvent(RawEventMsg{EvType: EV_KEY, EvCode: BTN_TOUCH, EvValue: 0})
	time.Sleep(50 * time.Millisecond)
	// No assertion beyond "does not panic / double-fire"; reaching here is success.
}

func TestGestureDetectorLogCallback(t *testing.T) {
	cfg := DefaultGestureConfig()
	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	var logged []string
	gd.OnLog = func(msg string) { logged = append(logged, msg) }
	gd.log("hello")
	if len(logged) != 1 || logged[0] != "hello" {
		t.Errorf("expected log callback to receive message, got %v", logged)
	}

	// Nil-safe when no callback is set.
	gd2 := NewGestureDetector(cfg)
	gd2.log("no panic")
}

func TestGestureDetectorButtonFallbacks(t *testing.T) {
	cfg := DefaultGestureConfig()
	cfg.DebounceDuration = 5 * time.Millisecond
	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	var bottomLeft int
	gd.OnBottomLeftTap = func(x, y int32) { bottomLeft++ }

	// Buses zone with no OnBusesTap falls back to OnBottomLeftTap.
	gd.curX, gd.curY = 50, 580
	gd.TriggerTap(time.Now())
	// Bikes zone with no OnBikesTap also falls back.
	gd.curX, gd.curY = 250, 580
	gd.TriggerTap(time.Now().Add(20 * time.Millisecond))

	if bottomLeft != 2 {
		t.Errorf("expected 2 bottom-left fallback taps, got %d", bottomLeft)
	}
}

func TestGestureDetectorLightAndRefreshZones(t *testing.T) {
	cfg := DefaultGestureConfig()
	cfg.DebounceDuration = 5 * time.Millisecond
	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	var light, refresh int
	gd.OnLightTap = func(x, y int32) { light++ }
	gd.OnRefreshTap = func(x, y int32) { refresh++ }

	gd.curX, gd.curY = 490, 580 // light zone 394..586
	gd.TriggerTap(time.Now())
	gd.curX, gd.curY = 650, 580 // refresh zone (>= 586)
	gd.TriggerTap(time.Now().Add(20 * time.Millisecond))

	if light != 1 || refresh != 1 {
		t.Errorf("expected light=1 refresh=1, got light=%d refresh=%d", light, refresh)
	}
}

func TestGestureDetectorNoopZonesWithoutCallbacks(t *testing.T) {
	cfg := DefaultGestureConfig()
	cfg.DebounceDuration = 5 * time.Millisecond
	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	// No callbacks registered: these paths must simply return without panic.
	gd.curX, gd.curY = 400, 580 // light zone, nil OnLightTap
	gd.TriggerTap(time.Now())
	gd.curX, gd.curY = 700, 580 // refresh zone, nil OnRefreshTap
	gd.TriggerTap(time.Now().Add(20 * time.Millisecond))
	gd.curX, gd.curY = 700, 50 // top-right, nil callback
	gd.TriggerTap(time.Now().Add(40 * time.Millisecond))
	gd.curX, gd.curY = 50, 50 // top-left, nil callback
	gd.TriggerTap(time.Now().Add(60 * time.Millisecond))
	gd.curX, gd.curY = 50, 550 // bottom-left, nil callback
	gd.TriggerTap(time.Now().Add(80 * time.Millisecond))
}

func TestGestureDetectorButtonTaps(t *testing.T) {
	cfg := DefaultGestureConfig()
	cfg.DebounceDuration = 5 * time.Millisecond

	gd := NewGestureDetector(cfg)
	defer gd.Stop()

	var (
		busesTaps   int
		bikesTaps   int
		lightTaps   int
		refreshTaps int
		mu          sync.Mutex
	)

	gd.OnBusesTap = func(x, y int32) {
		mu.Lock()
		busesTaps++
		mu.Unlock()
	}
	gd.OnBikesTap = func(x, y int32) {
		mu.Lock()
		bikesTaps++
		mu.Unlock()
	}
	gd.OnLightTap = func(x, y int32) {
		mu.Lock()
		lightTaps++
		mu.Unlock()
	}
	gd.OnRefreshTap = func(x, y int32) {
		mu.Lock()
		refreshTaps++
		mu.Unlock()
	}

	t0 := time.Now()

	// 1. Buses button (x < 164, y >= 556)
	gd.curX = 50
	gd.curY = 580
	gd.TriggerTap(t0)

	// 2. Bikes button (202 <= x < 394, y >= 556)
	gd.curX = 300
	gd.curY = 580
	t1 := t0.Add(20 * time.Millisecond)
	gd.TriggerTap(t1)

	// 3. Light button (394 <= x < 586, y >= 556)
	gd.curX = 490
	gd.curY = 580
	t2 := t1.Add(20 * time.Millisecond)
	gd.TriggerTap(t2)

	// 4. Refresh button (x >= 586, y >= 556)
	gd.curX = 700
	gd.curY = 580
	t3 := t2.Add(20 * time.Millisecond)
	gd.TriggerTap(t3)

	mu.Lock()
	defer mu.Unlock()

	if busesTaps != 1 {
		t.Errorf("Expected 1 buses button tap, got %d", busesTaps)
	}
	if bikesTaps != 1 {
		t.Errorf("Expected 1 bikes button tap, got %d", bikesTaps)
	}
	if lightTaps != 1 {
		t.Errorf("Expected 1 light button tap, got %d", lightTaps)
	}
	if refreshTaps != 1 {
		t.Errorf("Expected 1 refresh button tap, got %d", refreshTaps)
	}
}
