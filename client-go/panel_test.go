package main

import (
	"os"
	"testing"
)

func TestParseFramebufferSize(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		wantW  int
		wantH  int
		wantOK bool
	}{
		{"PW5 portrait virtual_size", "1236,1648\n", 1648, 1236, true},
		{"Already landscape", "1648,1236", 1648, 1236, true},
		{"Whitespace tolerated", " 1236 , 1648 ", 1648, 1236, true},
		{"Missing comma", "1236 1648", 0, 0, false},
		{"Non-numeric", "abc,def", 0, 0, false},
		{"Zero dims", "0,0", 0, 0, false},
		{"Negative dims", "-1,100", 0, 0, false},
		{"Empty", "", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseFramebufferSize(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && (got.LandscapeW != tt.wantW || got.LandscapeH != tt.wantH) {
				t.Errorf("got %+v, want %dx%d", got, tt.wantW, tt.wantH)
			}
		})
	}
}

func TestParsePanelSpec(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		wantW  int
		wantH  int
		wantOK bool
	}{
		{"virtual_size", "1236,1648", 1648, 1236, true},
		{"fb0 modes string", "U:1236x1648p-0", 1648, 1236, true},
		{"mode with newline", "U:1236x1648p-0\n", 1648, 1236, true},
		{"garbage", "no dimensions here", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parsePanelSpec(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && (got.LandscapeW != tt.wantW || got.LandscapeH != tt.wantH) {
				t.Errorf("got %+v, want %dx%d", got, tt.wantW, tt.wantH)
			}
		})
	}
}

func TestDetectPanelSize_FallsBackToPW5(t *testing.T) {
	patchRuntime(t)
	osReadFile = func(string) ([]byte, error) { return nil, errTestRead }
	size := DetectPanelSize()
	if size != PW5Landscape {
		t.Errorf("expected PW5 fallback %+v, got %+v", PW5Landscape, size)
	}
}

func TestDetectPanelSize_PrefersModes(t *testing.T) {
	patchRuntime(t)
	osReadFile = func(path string) ([]byte, error) {
		switch path {
		case "/sys/class/graphics/fb0/modes":
			return []byte("U:1236x1648p-0\n"), nil
		case "/sys/class/graphics/fb0/virtual_size":
			// Buffer size: double-buffered + height-aligned, must be ignored.
			return []byte("3296,1248\n"), nil
		}
		return nil, errTestRead
	}
	size := DetectPanelSize()
	if size.LandscapeW != 1648 || size.LandscapeH != 1236 {
		t.Errorf("expected 1648x1236 from modes, got %+v", size)
	}
}

func TestDetectPanelSize_RejectsBufferSizedVirtuaSize(t *testing.T) {
	patchRuntime(t)
	osReadFile = func(path string) ([]byte, error) {
		if path == "/sys/class/graphics/fb0/virtual_size" {
			return []byte("3296,1248\n"), nil
		}
		return nil, errTestRead
	}
	// 3296x1248 is implausible and must be rejected in favour of the fallback.
	size := DetectPanelSize()
	if size != PW5Landscape {
		t.Errorf("expected PW5 fallback for buffer-sized input, got %+v", size)
	}
}

func TestDetectPanelSize_ReadsPlausibleVirtualSize(t *testing.T) {
	patchRuntime(t)
	osReadFile = func(path string) ([]byte, error) {
		if path == "/sys/class/graphics/fb0/virtual_size" {
			return []byte("1072,1448\n"), nil
		}
		return nil, errTestRead
	}
	size := DetectPanelSize()
	if size.LandscapeW != 1448 || size.LandscapeH != 1072 {
		t.Errorf("expected 1448x1072, got %+v", size)
	}
}

func TestPlausiblePanel(t *testing.T) {
	tests := []struct {
		name string
		p    PanelSize
		want bool
	}{
		{"PW5", PanelSize{1648, 1236}, true},
		{"Kobo-ish", PanelSize{1448, 1072}, true},
		{"buffer doubled", PanelSize{3296, 1248}, false},
		{"too small", PanelSize{100, 100}, false},
		{"too large", PanelSize{3000, 2000}, false},
		{"height too small", PanelSize{1000, 300}, false},
		{"height too large", PanelSize{1000, 1900}, false},
		{"bad ratio", PanelSize{2000, 600}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := plausiblePanel(tt.p); got != tt.want {
				t.Errorf("plausiblePanel(%+v) = %v, want %v", tt.p, got, tt.want)
			}
		})
	}
}

var errTestRead = os.ErrNotExist
