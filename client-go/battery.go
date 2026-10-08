package main

import (
	"path/filepath"
	"strconv"
	"strings"
)

// BatteryInfo represents Kindle power supply status
type BatteryInfo struct {
	Level      int  // Percentage 0-100, or -1 if unknown
	IsCharging bool // True if connected to external power
}

// ReadBatteryInfo extracts battery percentage and charging status
// using lipc property getter or sysfs fallback
func ReadBatteryInfo(
	lipcGetter func(prop, key string) string,
	readFile func(string) ([]byte, error),
) BatteryInfo {
	// 1. Try LIPC powerd properties
	if lipcGetter != nil {
		lvlStr := strings.TrimSpace(lipcGetter("com.lab126.powerd", "battLevel"))
		if lvl, err := strconv.Atoi(lvlStr); err == nil && lvl >= 0 && lvl <= 100 {
			isCharge := strings.TrimSpace(lipcGetter("com.lab126.powerd", "isCharging")) == "1"
			return BatteryInfo{Level: lvl, IsCharging: isCharge}
		}
	}

	// 2. Sysfs power supply fallback
	if readFile != nil {
		capacityPaths := []string{
			"/sys/class/power_supply/battery/capacity",
			"/sys/class/power_supply/bd71828-battery/capacity",
			"/sys/class/power_supply/max77696-battery/capacity",
		}
		for _, capPath := range capacityPaths {
			if data, err := readFile(capPath); err == nil {
				lvlStr := strings.TrimSpace(string(data))
				if lvl, err := strconv.Atoi(lvlStr); err == nil && lvl >= 0 && lvl <= 100 {
					statusPath := filepath.Join(filepath.Dir(capPath), "status")
					isCharge := false
					if sData, err := readFile(statusPath); err == nil {
						s := strings.ToLower(string(sData))
						isCharge = strings.Contains(s, "charg")
					}
					return BatteryInfo{Level: lvl, IsCharging: isCharge}
				}
			}
		}
	}

	return BatteryInfo{Level: -1, IsCharging: false}
}

// GetBatteryInfo reads battery info using production system calls.
// It is a variable so tests can substitute deterministic battery readings.
var GetBatteryInfo = func() BatteryInfo {
	return ReadBatteryInfo(lipcGet, osReadFile)
}
