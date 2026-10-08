package main

import (
	"strings"
	"testing"
)

func TestAdoptableServerURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"Loopback IPv4", "http://127.0.0.1:8000", true},
		{"Private 192.168", "http://192.168.1.100:8000", true},
		{"Private 10.x", "http://10.0.0.5:8000", true},
		{"Private 172.16", "http://172.16.4.5:8000", true},
		{"Localhost name", "http://localhost:8000", true},
		{"Public IP rejected", "http://8.8.8.8:8000", false},
		{"Non-http scheme rejected", "ftp://192.168.1.1", false},
		{"Empty rejected", "", false},
		{"Garbage rejected", "not a url", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AdoptableServerURL(tt.raw); got != tt.want {
				t.Errorf("AdoptableServerURL(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestAdoptableServerURL_HostnameResolvesToLoopback(t *testing.T) {
	// "localhost" is a hostname (not a literal IP) that resolves to loopback.
	if !AdoptableServerURL("http://localhost:8000") {
		t.Error("expected localhost hostname to be adoptable")
	}
}

func TestNewTrackerClient_DefaultsEmptyViewToAuto(t *testing.T) {
	tc := NewTrackerClient("http://127.0.0.1:8000", "")
	if tc.viewMode != "auto" {
		t.Errorf("expected empty initial view to default to auto, got %q", tc.viewMode)
	}
	if tc.client == nil || tc.refreshCh == nil {
		t.Fatal("expected client and refresh channel to be initialized")
	}
}

func TestVerifySHA256(t *testing.T) {
	const digest = "61d247c404d23b5020960924f86d395b531aa3bfccee2b78cc2f39fab133cc79"

	tests := []struct {
		name     string
		actual   string
		expected string
		want     bool
	}{
		{"Exact match", digest, digest, true},
		{"Case-insensitive match", digest, strings.ToUpper(digest), true},
		{"Mismatch", digest, "deadbeef", false},
		{"Empty expected fails closed", digest, "", false},
		{"Empty actual fails", "", digest, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifySHA256(tt.actual, tt.expected); got != tt.want {
				t.Errorf("VerifySHA256() = %v, want %v", got, tt.want)
			}
		})
	}
}
