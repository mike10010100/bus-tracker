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
	cfg.TopRightThresholdX = 1000
	cfg.TopRightThresholdY = 300
	cfg.TopLeftThresholdX = 300
	cfg.TopLeftThresholdY = 300
	cfg.BottomLeftThresholdX = 350
	cfg.BottomLeftThresholdY = 1300
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

	// 1. Top-Right corner tap
	gd.curX = 1100
	gd.curY = 150
	t0 := time.Now()
	gd.TriggerTap(t0)

	// 2. Top-Left corner tap
	gd.curX = 150
	gd.curY = 150
	t1 := t0.Add(50 * time.Millisecond)
	gd.TriggerTap(t1)

	// 3. Bottom-Left corner tap
	gd.curX = 150
	gd.curY = 1450
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

	// Send EV_ABS position with NO explicit release event (pt_mt driver behavior)
	gd.ProcessEvent(RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_POSITION_X, EvValue: 600})
	gd.ProcessEvent(RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_POSITION_Y, EvValue: 600})

	// Wait for inactivity timer (20ms) + single tap delay (20ms)
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	count := taps
	mu.Unlock()

	if count != 1 {
		t.Errorf("Inactivity fallback should have triggered 1 tap, got %d", count)
	}
}
