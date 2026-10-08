package main

import (
	"errors"
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
