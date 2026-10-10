package main

import (
	"fmt"
	"time"
)

const (
	PeakPollInterval = 60 * time.Second
	EcoPollInterval  = 10 * time.Minute
	// Polls of a minute or more are aligned to the wall clock and delayed by
	// this offset so the refresh lands just *after* the on-screen clock ticks
	// over, rather than a hair before it.
	PollSettleOffset = 500 * time.Millisecond
	// FastPollHoldDuration bounds how long a *data-affecting* interaction
	// (view switch / refresh) keeps the radio polling at the fast cadence.
	// Screen-only actions (frontlight, exit) deliberately do not arm it.
	FastPollHoldDuration = 10 * time.Minute
)

// dataInteraction records a user action that changes what is fetched/rendered
// (view switch or explicit refresh). It arms the fast-poll hold. Screen-only
// actions (frontlight, exit) deliberately do not call this.
func (tc *TrackerClient) dataInteraction() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.lastDataInteraction = time.Now()
}

// setInteractionLighting lights the panel for an interaction session and, while
// on, suppresses the schedule auto-lighting. On session end (on=false) the
// manual hold is cleared so the schedule lighting resumes (e.g. off overnight).
func (tc *TrackerClient) setInteractionLighting(on bool) {
	tc.mu.Lock()
	if on {
		tc.manualLightTime = time.Now()
	} else {
		tc.manualLightTime = time.Time{}
	}
	tc.mu.Unlock()
	if !on {
		return
	}
	curr := lipcGet("com.lab126.powerd", "flIntensity")
	if curr == "" || curr == "0" {
		lipcSet("com.lab126.powerd", "flIntensity", "8")
		lipcSet("com.lab126.powerd", "schedAmberLevel", "12")
		tc.logRemote(fmt.Sprintf("Interaction lighting on (was %q).", curr))
	}
}

// noteTouch signals that the user touched the screen. Used to keep an awake
// interaction session alive while the user is interacting, even for a tap that
// only changes (say) the frontlight. Non-blocking.
func (tc *TrackerClient) noteTouch() {
	select {
	case tc.touchCh <- struct{}{}:
	default:
	}
}

func (tc *TrackerClient) getNextPollInterval(serverIntervalSec int) time.Duration {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	// If a data-affecting interaction occurred recently, stay in fast mode.
	if !tc.lastDataInteraction.IsZero() && time.Since(tc.lastDataInteraction) < FastPollHoldDuration {
		return PeakPollInterval
	}

	// Use server guidance if provided
	if serverIntervalSec > 0 {
		return time.Duration(serverIntervalSec) * time.Second
	}

	// Fallback calculation based on local time: 60s during rush, 10m off-peak
	now := time.Now()
	hour := float64(now.Hour()) + float64(now.Minute())/60.0
	if (hour >= 7.5 && hour < 9.5) || (hour >= 16.5 && hour < 19.0) {
		return PeakPollInterval
	}
	return EcoPollInterval
}

// alignDelay returns how long to wait (from `now`) so that the next poll lands
// on the next wall-clock multiple of interval, offset by PollSettleOffset so it
// fires just after the target boundary. Intervals below a minute are returned
// unchanged (drifting polls are fine there).
func alignDelay(now time.Time, interval time.Duration) time.Duration {
	if interval < time.Minute {
		return interval
	}
	untilBoundary := interval - time.Duration(now.UnixNano()%int64(interval))
	return untilBoundary + PollSettleOffset
}
