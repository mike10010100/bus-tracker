package main

import (
	"testing"
	"time"
)

func TestGetNextPollInterval(t *testing.T) {
	tc := NewTrackerClient("http://localhost:8000", "auto")

	t.Run("Honors server interval when no manual interaction active", func(t *testing.T) {
		interval := tc.getNextPollInterval(600)
		if interval != 10*time.Minute {
			t.Errorf("Expected 10m from server interval 600, got %v", interval)
		}

		intervalRush := tc.getNextPollInterval(45)
		if intervalRush != 45*time.Second {
			t.Errorf("Expected 45s from server interval 45, got %v", intervalRush)
		}
	})

	t.Run("Manual interaction boosts to 45s even if server requests 10m", func(t *testing.T) {
		tc.markManualInteraction()
		interval := tc.getNextPollInterval(600)
		if interval != 45*time.Second {
			t.Errorf("Expected 45s boost after manual interaction, got %v", interval)
		}
	})

	t.Run("Manual boost expires after ManualHoldDuration", func(t *testing.T) {
		tc.mu.Lock()
		// Simulate manual tap 46 minutes ago
		tc.manualRefreshTime = time.Now().Add(-46 * time.Minute)
		tc.mu.Unlock()

		interval := tc.getNextPollInterval(600)
		if interval != 10*time.Minute {
			t.Errorf("Expected 10m after boost expiration, got %v", interval)
		}
	})

	t.Run("Fallback interval when server sends 0 uses local time", func(t *testing.T) {
		tc.mu.Lock()
		tc.manualRefreshTime = time.Time{}
		tc.mu.Unlock()

		interval := tc.getNextPollInterval(0)
		if interval != 45*time.Second && interval != 10*time.Minute {
			t.Errorf("Expected either 45s or 10m fallback, got %v", interval)
		}
	})
}
