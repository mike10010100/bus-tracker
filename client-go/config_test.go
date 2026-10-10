package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestResolveServerURL(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		getenv   func(string) string
		readFile func(string) ([]byte, error)
		want     string
	}{
		{
			name: "CLI flag with space",
			args: []string{"/tmp/tracker", "-server", "http://10.0.0.5:8000"},
			want: "http://10.0.0.5:8000",
		},
		{
			name: "CLI flag with equals",
			args: []string{"/tmp/tracker", "-server=http://10.0.0.6:8000"},
			want: "http://10.0.0.6:8000",
		},
		{
			name: "CLI double-dash flag",
			args: []string{"/tmp/tracker", "--server", "http://10.0.0.7:8000"},
			want: "http://10.0.0.7:8000",
		},
		{
			name: "Environment variable priority over file",
			args: []string{"/tmp/tracker"},
			getenv: func(k string) string {
				if k == "TRACKER_SERVER" {
					return "http://env-server:8000"
				}
				return ""
			},
			readFile: func(path string) ([]byte, error) {
				return []byte("http://file-server:8000"), nil
			},
			want: "http://env-server:8000",
		},
		{
			name:   "File fallback when no CLI or ENV",
			args:   []string{"/tmp/tracker"},
			getenv: func(k string) string { return "" },
			readFile: func(path string) ([]byte, error) {
				if path == "/mnt/us/documents/tracker_server.txt" {
					return []byte("http://kindle-file:8000\n"), nil
				}
				return nil, errors.New("not found")
			},
			want: "http://kindle-file:8000",
		},
		{
			name:   "Default fallback",
			args:   []string{"/tmp/tracker"},
			getenv: func(k string) string { return "" },
			readFile: func(path string) ([]byte, error) {
				return nil, errors.New("not found")
			},
			want: DefaultCandidateServers[0],
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveServerURL(tt.args, tt.getenv, tt.readFile)
			if got != tt.want {
				t.Errorf("ResolveServerURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveServerURLWithDiscoverer(t *testing.T) {
	mockDiscover := func() (string, error) {
		return "http://192.168.86.77:8000", nil
	}

	got := ResolveServerURLWithDiscoverer(
		[]string{"/tmp/tracker"},
		func(k string) string { return "" },
		func(p string) ([]byte, error) { return nil, errors.New("no file") },
		mockDiscover,
	)

	want := "http://192.168.86.77:8000"
	if got != want {
		t.Errorf("ResolveServerURLWithDiscoverer() = %q, want %q", got, want)
	}
}

func TestGetServerURL_EnvOverride(t *testing.T) {
	originalArgs := os.Args
	os.Args = []string{"/tmp/tracker"}
	defer func() { os.Args = originalArgs }()

	t.Setenv("TRACKER_SERVER", "http://env-injected:8000")
	if got := GetServerURL(); got != "http://env-injected:8000" {
		t.Errorf("GetServerURL() = %q, want env override", got)
	}
}

func TestGetServerURL_DefaultFallback(t *testing.T) {
	originalArgs := os.Args
	os.Args = []string{"/tmp/tracker"}
	defer func() { os.Args = originalArgs }()

	// Clear env override; unreadable config files fall through to discovery
	// (disabled) and finally the default candidate list.
	t.Setenv("TRACKER_SERVER", "")
	GetServerURL() // must not panic; returns some default
}

func TestResolveRunMode(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want RunMode
	}{
		{"default resident", []string{"/tmp/tracker"}, ModeResident},
		{"explicit resident", []string{"/tmp/tracker", "-resident"}, ModeResident},
		{"oneshot", []string{"/tmp/tracker", "-oneshot"}, ModeOneshot},
		{"oneshot double dash", []string{"/tmp/tracker", "--oneshot"}, ModeOneshot},
		{"sleep", []string{"/tmp/tracker", "-sleep"}, ModeSleep},
		{"sleep double dash", []string{"/tmp/tracker", "--sleep"}, ModeSleep},
		{"oneshot wins over sleep", []string{"/tmp/tracker", "-sleep", "-oneshot"}, ModeOneshot},
		{"oneshot wins regardless of order", []string{"/tmp/tracker", "-oneshot", "-sleep"}, ModeOneshot},
		{"resident after sleep", []string{"/tmp/tracker", "-sleep", "-resident"}, ModeResident},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveRunMode(tt.args); got != tt.want {
				t.Errorf("ResolveRunMode(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestWantsSuspend(t *testing.T) {
	if wantsSuspend([]string{"/tmp/tracker", "-sleep"}) {
		t.Error("sleep without -suspend should not enable suspend")
	}
	if !wantsSuspend([]string{"/tmp/tracker", "-sleep", "-suspend"}) {
		t.Error("-suspend should enable suspend")
	}
	if !wantsSuspend([]string{"/tmp/tracker", "--suspend"}) {
		t.Error("--suspend should enable suspend")
	}
}

func TestModeFlags(t *testing.T) {
	cases := map[string][]string{
		"resident":      {"-resident"},
		"oneshot":       {"-oneshot"},
		"sleep":         {"-sleep"},
		"sleep-suspend": {"-sleep", "-suspend"},
		"bogus":         nil,
	}
	for name, want := range cases {
		got := modeFlags(name)
		if len(got) != len(want) {
			t.Errorf("modeFlags(%q) = %v, want %v", name, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("modeFlags(%q) = %v, want %v", name, got, want)
			}
		}
	}
}

func TestCurrentModeName(t *testing.T) {
	origMode, origArgs := currentRunMode, os.Args
	defer func() { currentRunMode, os.Args = origMode, origArgs }()

	currentRunMode = ModeResident
	if got := currentModeName(); got != "resident" {
		t.Errorf("got %q, want resident", got)
	}
	currentRunMode = ModeSleep
	os.Args = []string{"/tmp/tracker", "-sleep"}
	if got := currentModeName(); got != "sleep" {
		t.Errorf("got %q, want sleep", got)
	}
	os.Args = []string{"/tmp/tracker", "-sleep", "-suspend"}
	if got := currentModeName(); got != "sleep-suspend" {
		t.Errorf("got %q, want sleep-suspend", got)
	}
}

func TestRunModeString(t *testing.T) {
	if ModeResident.String() != "resident" || ModeOneshot.String() != "oneshot" || ModeSleep.String() != "sleep" {
		t.Errorf("unexpected mode strings: %s %s %s", ModeResident, ModeOneshot, ModeSleep)
	}
}

func TestResolveViewMode(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "Default auto without flags",
			args: []string{"/tmp/tracker"},
			want: "auto",
		},
		{
			name: "CLI flag morning with space",
			args: []string{"/tmp/tracker", "-view", "morning"},
			want: "morning",
		},
		{
			name: "CLI flag evening with space",
			args: []string{"/tmp/tracker", "--view", "evening"},
			want: "evening",
		},
		{
			name: "CLI flag morning with equals",
			args: []string{"/tmp/tracker", "-view=morning"},
			want: "morning",
		},
		{
			name: "CLI flag evening with equals",
			args: []string{"/tmp/tracker", "--view=evening"},
			want: "evening",
		},
		{
			name: "Case insensitive",
			args: []string{"/tmp/tracker", "-view=MORNING"},
			want: "morning",
		},
		{
			name: "Unknown value falls back to auto",
			args: []string{"/tmp/tracker", "-view=unknown"},
			want: "auto",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveViewMode(tt.args)
			if got != tt.want {
				t.Errorf("ResolveViewMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveServerURLWithDiscoverer_CandidateProbeWithContext(t *testing.T) {
	origCandidates := DefaultCandidateServers
	defer func() { DefaultCandidateServers = origCandidates }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && r.URL.Path == "/tracker-arm" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	DefaultCandidateServers = []string{srv.URL}
	got := ResolveServerURLWithDiscoverer(
		[]string{"/tmp/tracker"},
		func(string) string { return "" },
		func(string) ([]byte, error) { return nil, os.ErrNotExist },
		func() (string, error) { return "", os.ErrNotExist },
	)
	if got != srv.URL {
		t.Errorf("got %q, want %q", got, srv.URL)
	}
}
