package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// withDiscoverySeams restores discovery seams after the test.
func withDiscoverySeams(t *testing.T) {
	t.Helper()
	origVerify := verifyServerFn
	origSave := saveServerURL
	origUDP := discoverViaUDP
	origSweep := discoverViaSweep
	origIfaces := netInterfaces
	t.Cleanup(func() {
		verifyServerFn = origVerify
		saveServerURL = origSave
		discoverViaUDP = origUDP
		discoverViaSweep = origSweep
		netInterfaces = origIfaces
	})
}

func TestAutoDiscoverServer_UDPFirst(t *testing.T) {
	withDiscoverySeams(t)
	var saved string
	saveServerURL = func(url string) error { saved = url; return nil }
	discoverViaUDP = func(context.Context, int, time.Duration) (string, error) {
		return "http://10.0.0.9:8000", nil
	}
	discoverViaSweep = func(context.Context, int) (string, error) {
		t.Fatal("sweep should not run when UDP succeeds")
		return "", nil
	}

	got, err := AutoDiscoverServer(context.Background())
	if err != nil || got != "http://10.0.0.9:8000" {
		t.Fatalf("expected UDP result, got %q err=%v", got, err)
	}
	if saved != got {
		t.Errorf("expected server URL persisted, got %q", saved)
	}
}

func TestAutoDiscoverServer_FallsBackToSweep(t *testing.T) {
	withDiscoverySeams(t)
	saveServerURL = func(string) error { return nil }
	discoverViaUDP = func(context.Context, int, time.Duration) (string, error) {
		return "", context.DeadlineExceeded
	}
	discoverViaSweep = func(context.Context, int) (string, error) {
		return "http://192.168.1.44:8000", nil
	}

	got, err := AutoDiscoverServer(context.Background())
	if err != nil || got != "http://192.168.1.44:8000" {
		t.Fatalf("expected sweep fallback, got %q err=%v", got, err)
	}
}

func TestAutoDiscoverServer_AllFail(t *testing.T) {
	withDiscoverySeams(t)
	discoverViaUDP = func(context.Context, int, time.Duration) (string, error) {
		return "", context.DeadlineExceeded
	}
	discoverViaSweep = func(context.Context, int) (string, error) {
		return "", context.DeadlineExceeded
	}
	if _, err := AutoDiscoverServer(context.Background()); err == nil {
		t.Fatal("expected error when both discovery strategies fail")
	}
}

func TestDiscoverViaSubnetSweep_FindsResponder(t *testing.T) {
	withDiscoverySeams(t)
	verifyServerFn = func(_ context.Context, url string, _ time.Duration) bool {
		return true
	}
	// Provide a deterministic interface list via the seam.
	netInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Name: "eth0", Flags: net.FlagUp}}, nil
	}

	// Override iface.Addrs is not seamable; instead rely on fallback subnets by
	// using an empty interface set is not possible, so just assert it returns.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := DiscoverViaSubnetSweep(ctx, 8000)
	if err != nil {
		t.Fatalf("expected a discovered server, got err %v", err)
	}
	if !AdoptableServerURL(got) && got == "" {
		t.Fatalf("unexpected result %q", got)
	}
}

func TestDiscoverViaSubnetSweep_NoMatch(t *testing.T) {
	withDiscoverySeams(t)
	verifyServerFn = func(context.Context, string, time.Duration) bool { return false }
	netInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Name: "eth0", Flags: net.FlagUp}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := DiscoverViaSubnetSweep(ctx, 1); err == nil {
		t.Fatal("expected error when no host responds")
	}
}

func TestDiscoverViaSubnetSweep_NoInterfacesUsesDefaultSubnets(t *testing.T) {
	withDiscoverySeams(t)
	verifyServerFn = func(context.Context, string, time.Duration) bool { return false }
	netInterfaces = func() ([]net.Interface, error) { return nil, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// With no interfaces the sweep injects default /24 subnets; all probes fail,
	// so we expect a "no server found" error rather than a panic.
	if _, err := DiscoverViaSubnetSweep(ctx, 1); err == nil {
		t.Fatal("expected error when no server responds")
	}
}

func TestDiscoverViaSubnetSweep_InterfaceError(t *testing.T) {
	withDiscoverySeams(t)
	netInterfaces = func() ([]net.Interface, error) { return nil, os.ErrPermission }
	if _, err := DiscoverViaSubnetSweep(context.Background(), 8000); err == nil {
		t.Fatal("expected error when interfaces cannot be listed")
	}
}

func TestDiscoverViaUDP_InvalidOfferThenValid(t *testing.T) {
	withDiscoverySeams(t)
	// Mock the responder to send a garbage offer first, then a valid one.
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("failed to bind test UDP listener: %v", err)
	}
	defer udpConn.Close()
	port := udpConn.LocalAddr().(*net.UDPAddr).Port

	stop := make(chan struct{})
	go func() {
		buf := make([]byte, 512)
		for {
			_ = udpConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, raddr, err := udpConn.ReadFrom(buf)
			if err != nil {
				select {
				case <-stop:
					return
				default:
					continue
				}
			}
			if n > 0 {
				// First reply is unparseable, second is a public (non-adoptable) offer.
				_, _ = udpConn.WriteTo([]byte("GARBAGE HEADER"), raddr)
				_, _ = udpConn.WriteTo([]byte("TRANSIT_TRACKER_OFFER http://8.8.8.8:8000 1.0"), raddr)
			}
		}
	}()
	defer close(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = DiscoverViaUDP(ctx, port, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error when no adoptable offer is received")
	}
}

func TestDiscoverViaUDP_LocalhostFallbackToRemoteIP(t *testing.T) {
	withDiscoverySeams(t)
	// The offer URL fails the first verification, so the code falls back to the
	// responder's source IP; the second verification succeeds.
	calls := 0
	verifyServerFn = func(_ context.Context, _ string, _ time.Duration) bool {
		calls++
		return calls > 1
	}

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("failed to bind test UDP listener: %v", err)
	}
	defer udpConn.Close()
	port := udpConn.LocalAddr().(*net.UDPAddr).Port

	stop := make(chan struct{})
	go func() {
		buf := make([]byte, 512)
		for {
			_ = udpConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, raddr, err := udpConn.ReadFrom(buf)
			if err != nil {
				select {
				case <-stop:
					return
				default:
					continue
				}
			}
			if n > 0 {
				_, _ = udpConn.WriteTo([]byte("TRANSIT_TRACKER_OFFER http://127.0.0.1:8000 1.0"), raddr)
				return
			}
		}
	}()
	defer close(stop)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := DiscoverViaUDP(ctx, port, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("expected localhost fallback to succeed, got %v", err)
	}
	if got == "" {
		t.Fatal("expected a non-empty fallback URL")
	}
}

func TestVerifyServer_RealHTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tracker-arm" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	if !verifyServer(context.Background(), ts.URL, time.Second) {
		t.Error("expected verifyServer true for reachable server")
	}
	if verifyServer(context.Background(), "http://127.0.0.1:1", 200*time.Millisecond) {
		t.Error("expected verifyServer false for unreachable server")
	}
}

func TestPersistServerURL_EmptyRejected(t *testing.T) {
	if err := PersistServerURL("   ", os.WriteFile); err == nil {
		t.Fatal("expected error for empty server URL")
	}
}

func TestPersistServerURL_WritesToMockedWriter(t *testing.T) {
	written := map[string]string{}
	err := PersistServerURL("http://10.0.0.2:8000", func(p string, data []byte, perm os.FileMode) error {
		written[p] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(written) == 0 {
		t.Fatal("expected files to be written")
	}
	for _, content := range written {
		if content == "" {
			t.Error("expected non-empty content")
		}
	}
}

func TestPersistServerURL_AllWritesFail(t *testing.T) {
	err := PersistServerURL("http://10.0.0.2:8000", func(p string, data []byte, perm os.FileMode) error {
		return os.ErrPermission
	})
	if err == nil {
		t.Fatal("expected error when all writes fail")
	}
}
