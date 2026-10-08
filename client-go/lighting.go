package main

// NextFrontlightState calculates the next brightness and warmth values
// when manually cycling the frontlight: Off (0) -> Cozy (8) -> Bright (18) -> Off (0)
func NextFrontlightState(currentIntensity int) (nextIntensity, nextWarmth int) {
	switch {
	case currentIntensity <= 0:
		return 8, 12 // Cozy Amber Glow
	case currentIntensity <= 12:
		return 18, 8 // Bright Reading Light
	default:
		return 0, 0 // Off
	}
}
