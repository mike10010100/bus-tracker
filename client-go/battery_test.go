package main

import (
	"errors"
	"testing"
)

func TestReadBatteryInfo(t *testing.T) {
	tests := []struct {
		name       string
		lipcGetter func(prop, key string) string
		readFile   func(string) ([]byte, error)
		wantLevel  int
		wantCharge bool
	}{
		{
			name: "LIPC charging",
			lipcGetter: func(prop, key string) string {
				if key == "battLevel" {
					return "87"
				}
				if key == "isCharging" {
					return "1"
				}
				return ""
			},
			readFile:   nil,
			wantLevel:  87,
			wantCharge: true,
		},
		{
			name: "LIPC discharging",
			lipcGetter: func(prop, key string) string {
				if key == "battLevel" {
					return "42\n"
				}
				if key == "isCharging" {
					return "0"
				}
				return ""
			},
			readFile:   nil,
			wantLevel:  42,
			wantCharge: false,
		},
		{
			name: "Sysfs fallback charging",
			lipcGetter: func(prop, key string) string {
				return ""
			},
			readFile: func(path string) ([]byte, error) {
				if path == "/sys/class/power_supply/battery/capacity" {
					return []byte("95\n"), nil
				}
				if path == "/sys/class/power_supply/battery/status" {
					return []byte("Charging\n"), nil
				}
				return nil, errors.New("not found")
			},
			wantLevel:  95,
			wantCharge: true,
		},
		{
			name: "Unavailable battery returns -1",
			lipcGetter: func(prop, key string) string {
				return ""
			},
			readFile: func(path string) ([]byte, error) {
				return nil, errors.New("not found")
			},
			wantLevel:  -1,
			wantCharge: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := ReadBatteryInfo(tt.lipcGetter, tt.readFile)
			if info.Level != tt.wantLevel || info.IsCharging != tt.wantCharge {
				t.Errorf("ReadBatteryInfo() = %+v, want level %d, charge %v", info, tt.wantLevel, tt.wantCharge)
			}
		})
	}
}
