package main

import "testing"

func TestNextFrontlightState(t *testing.T) {
	tests := []struct {
		name          string
		current       int
		wantIntensity int
		wantWarmth    int
	}{
		{"Off to Cozy", 0, 8, 12},
		{"Negative to Cozy", -1, 8, 12},
		{"Low intensity to Bright", 5, 18, 8},
		{"Cozy (8) to Bright", 8, 18, 8},
		{"Cozy boundary (12) to Bright", 12, 18, 8},
		{"Bright (18) to Off", 18, 0, 0},
		{"High intensity (24) to Off", 24, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotIntensity, gotWarmth := NextFrontlightState(tt.current)
			if gotIntensity != tt.wantIntensity || gotWarmth != tt.wantWarmth {
				t.Errorf("NextFrontlightState(%d) = (%d, %d), want (%d, %d)",
					tt.current, gotIntensity, gotWarmth, tt.wantIntensity, tt.wantWarmth)
			}
		})
	}
}

func TestFrontlightCycleFullLoop(t *testing.T) {
	// Starting at 0 (Off)
	state := 0

	// Step 1: 0 -> 8
	state, warmth := NextFrontlightState(state)
	if state != 8 || warmth != 12 {
		t.Fatalf("Step 1 expected (8, 12), got (%d, %d)", state, warmth)
	}

	// Step 2: 8 -> 18
	state, warmth = NextFrontlightState(state)
	if state != 18 || warmth != 8 {
		t.Fatalf("Step 2 expected (18, 8), got (%d, %d)", state, warmth)
	}

	// Step 3: 18 -> 0
	state, warmth = NextFrontlightState(state)
	if state != 0 || warmth != 0 {
		t.Fatalf("Step 3 expected (0, 0), got (%d, %d)", state, warmth)
	}

	// Step 4: 0 -> 8 (loop repeats)
	state, warmth = NextFrontlightState(state)
	if state != 8 || warmth != 12 {
		t.Fatalf("Step 4 expected (8, 12), got (%d, %d)", state, warmth)
	}
}
