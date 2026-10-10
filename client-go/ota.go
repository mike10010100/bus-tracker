package main

import (
	"fmt"

	"github.com/mike10010100/transit-tracker/client-go/internal/otasig"
)

// ShouldUpdate evaluates whether an OTA update should be applied based on strict semver comparison.
func ShouldUpdate(serverVer, currentVer string) (bool, string) {
	if !otasig.ValidVersion(serverVer) || !otasig.ValidVersion(currentVer) {
		return false, ""
	}
	if otasig.IsNewer(serverVer, currentVer) {
		return true, fmt.Sprintf("Newer version available: %s > %s", serverVer, currentVer)
	}
	return false, ""
}
