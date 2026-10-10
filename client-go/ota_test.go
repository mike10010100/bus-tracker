package main

import "testing"

func TestShouldUpdate(t *testing.T) {
	tests := []struct {
		name       string
		serverVer  string
		currentVer string
		wantUpdate bool
	}{
		{
			name:       "Newer version triggers update",
			serverVer:  "1.3.0",
			currentVer: "1.2.1",
			wantUpdate: true,
		},
		{
			name:       "Older version does not trigger update (downgrade protection)",
			serverVer:  "1.2.0",
			currentVer: "1.2.1",
			wantUpdate: false,
		},
		{
			name:       "Identical version does not trigger update",
			serverVer:  "1.2.1",
			currentVer: "1.2.1",
			wantUpdate: false,
		},
		{
			name:       "Invalid server version does not trigger update",
			serverVer:  "v1.3.0",
			currentVer: "1.2.1",
			wantUpdate: false,
		},
		{
			name:       "Empty server version does not trigger update",
			serverVer:  "",
			currentVer: "1.2.1",
			wantUpdate: false,
		},
		{
			name:       "Invalid current version fails closed",
			serverVer:  "1.3.0",
			currentVer: "dev",
			wantUpdate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := ShouldUpdate(tt.serverVer, tt.currentVer)
			if got != tt.wantUpdate {
				t.Errorf("ShouldUpdate(%q, %q) = %v, want %v", tt.serverVer, tt.currentVer, got, tt.wantUpdate)
			}
		})
	}
}
