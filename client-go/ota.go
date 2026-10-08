package main

import (
	"fmt"
)

// ShouldUpdate evaluates whether an OTA update should be downloaded and applied
func ShouldUpdate(serverVer, currentVer, serverLastMod, localLastMod string) (bool, string) {
	if serverVer != "" && currentVer != "" && serverVer != currentVer {
		return true, fmt.Sprintf("Version mismatch (running: %s, server: %s)", currentVer, serverVer)
	}

	if localLastMod != "" && serverLastMod != "" && serverLastMod != localLastMod {
		return true, fmt.Sprintf("Timestamp mismatch (running: %s, server: %s)", localLastMod, serverLastMod)
	}

	return false, ""
}
