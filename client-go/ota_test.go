package main

import "testing"

func TestShouldUpdate(t *testing.T) {
	tests := []struct {
		name          string
		serverVer     string
		currentVer    string
		serverLastMod string
		localLastMod  string
		wantUpdate    bool
	}{
		{
			name:          "Version mismatch triggers update",
			serverVer:     "1.3.0",
			currentVer:    "1.2.1",
			serverLastMod: "Wed, 01 Jan 2026 00:00:00 GMT",
			localLastMod:  "Wed, 01 Jan 2026 00:00:00 GMT",
			wantUpdate:    true,
		},
		{
			name:          "Timestamp mismatch triggers update",
			serverVer:     "1.2.1",
			currentVer:    "1.2.1",
			serverLastMod: "Wed, 01 Jan 2026 01:00:00 GMT",
			localLastMod:  "Wed, 01 Jan 2026 00:00:00 GMT",
			wantUpdate:    true,
		},
		{
			name:          "Identical version and timestamp does not trigger update",
			serverVer:     "1.2.1",
			currentVer:    "1.2.1",
			serverLastMod: "Wed, 01 Jan 2026 00:00:00 GMT",
			localLastMod:  "Wed, 01 Jan 2026 00:00:00 GMT",
			wantUpdate:    false,
		},
		{
			name:          "Empty server version falls back to timestamp comparison",
			serverVer:     "",
			currentVer:    "1.2.1",
			serverLastMod: "Wed, 01 Jan 2026 02:00:00 GMT",
			localLastMod:  "Wed, 01 Jan 2026 00:00:00 GMT",
			wantUpdate:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := ShouldUpdate(tt.serverVer, tt.currentVer, tt.serverLastMod, tt.localLastMod)
			if got != tt.wantUpdate {
				t.Errorf("ShouldUpdate() = %v, want %v", got, tt.wantUpdate)
			}
		})
	}
}
