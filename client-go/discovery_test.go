package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseDiscoveryOffer(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantURL     string
		wantVersion string
		wantErr     bool
	}{
		{
			name:        "Standard offer with version",
			input:       "TRANSIT_TRACKER_OFFER http://192.168.86.193:8000 1.4.0\n",
			wantURL:     "http://192.168.86.193:8000",
			wantVersion: "1.4.0",
			wantErr:     false,
		},
		{
			name:        "Trailing slash in offer URL gets trimmed",
			input:       "TRANSIT_TRACKER_OFFER http://10.0.0.5:8000/ 1.4.0",
			wantURL:     "http://10.0.0.5:8000",
			wantVersion: "1.4.0",
			wantErr:     false,
		},
		{
			name:        "Offer without version",
			input:       "TRANSIT_TRACKER_OFFER http://192.168.1.50:8000",
			wantURL:     "http://192.168.1.50:8000",
			wantVersion: "",
			wantErr:     false,
		},
		{
			name:        "Legacy BUS_TRACKER_OFFER still accepted",
			input:       "BUS_TRACKER_OFFER http://192.168.1.60:8000 1.4.0",
			wantURL:     "http://192.168.1.60:8000",
			wantVersion: "1.4.0",
			wantErr:     false,
		},
		{
			name:    "Invalid header",
			input:   "UNKNOWN_HEADER http://1.2.3.4",
			wantErr: true,
		},
		{
			name:    "Empty input",
			input:   "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDiscoveryOffer(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseDiscoveryOffer() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if got.URL != tt.wantURL {
					t.Errorf("got.URL = %q, want %q", got.URL, tt.wantURL)
				}
				if got.Version != tt.wantVersion {
					t.Errorf("got.Version = %q, want %q", got.Version, tt.wantVersion)
				}
			}
		})
	}
}

func TestGetBroadcastAddresses(t *testing.T) {
	addrs := GetBroadcastAddresses(8001)
	if len(addrs) == 0 {
		t.Fatal("expected at least fallback broadcast address")
	}
	if addrs[0] != "255.255.255.255:8001" {
		t.Errorf("expected first address to be 255.255.255.255:8001, got %s", addrs[0])
	}
}

func TestPersistServerURL(t *testing.T) {
	writtenFiles := make(map[string]string)
	mockWrite := func(path string, data []byte, perm os.FileMode) error {
		writtenFiles[path] = string(data)
		return nil
	}

	err := PersistServerURL("http://192.168.86.200:8000", mockWrite)
	if err != nil {
		t.Fatalf("PersistServerURL() error = %v", err)
	}

	if len(writtenFiles) == 0 {
		t.Fatal("expected files to be written")
	}

	for path, content := range writtenFiles {
		if !strings.Contains(content, "http://192.168.86.200:8000") {
			t.Errorf("file %s has unexpected content: %s", path, content)
		}
	}
}

func TestDiscoverViaUDPIntegration(t *testing.T) {
	// Spin up a mock HTTP server simulating server.py
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tracker-arm" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// Spin up a mock UDP responder on an ephemeral port
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("failed to bind test UDP listener: %v", err)
	}
	defer udpConn.Close()
	testUDPPort := udpConn.LocalAddr().(*net.UDPAddr).Port

	// Goroutine responding to BUS_TRACKER_DISCOVER
	stopChan := make(chan struct{})
	go func() {
		buf := make([]byte, 512)
		for {
			_ = udpConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, raddr, err := udpConn.ReadFrom(buf)
			if err != nil {
				select {
				case <-stopChan:
					return
				default:
					continue
				}
			}
			if strings.Contains(string(buf[:n]), "TRANSIT_TRACKER_DISCOVER") {
				reply := fmt.Sprintf("TRANSIT_TRACKER_OFFER %s 1.4.0\n", ts.URL)
				_, _ = udpConn.WriteTo([]byte(reply), raddr)
			}
		}
	}()
	defer close(stopChan)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	discovered, err := DiscoverViaUDP(ctx, testUDPPort, 1*time.Second)
	if err != nil {
		t.Fatalf("DiscoverViaUDP failed: %v", err)
	}

	if discovered != ts.URL {
		t.Errorf("got discovered URL %s, want %s", discovered, ts.URL)
	}
}
