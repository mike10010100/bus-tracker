package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	DefaultDiscoveryPort = 8001
	DefaultServerPort    = 8000
	ServerConfigFile     = "/mnt/us/documents/tracker_server.txt"
	FallbackConfigFile   = "/tmp/tracker_server.txt"
)

// ServerOffer holds information received from a discovery offer
type ServerOffer struct {
	URL     string
	Version string
}

// ParseDiscoveryOffer parses a raw UDP payload such as:
// "TRANSIT_TRACKER_OFFER http://192.168.86.193:8000 1.4.0"
// The legacy "BUS_TRACKER_OFFER" header is still accepted for compatibility
// with older servers.
func ParseDiscoveryOffer(raw string) (*ServerOffer, error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "TRANSIT_TRACKER_OFFER") && !strings.HasPrefix(trimmed, "BUS_TRACKER_OFFER") {
		return nil, fmt.Errorf("invalid offer header: %s", trimmed)
	}

	parts := strings.Fields(trimmed)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed discovery offer: %s", trimmed)
	}

	offer := &ServerOffer{
		URL: strings.TrimRight(parts[1], "/"),
	}
	if len(parts) >= 3 {
		offer.Version = parts[2]
	}
	return offer, nil
}

// GetBroadcastAddresses discovers all broadcast destinations on active local interfaces
func GetBroadcastAddresses(port int) []string {
	var addrs []string
	addrs = append(addrs, fmt.Sprintf("255.255.255.255:%d", port))
	addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", port))

	ifaces, err := net.Interfaces()
	if err != nil {
		return addrs
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		unicastAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range unicastAddrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil {
				continue
			}
			ip4 := ipNet.IP.To4()
			mask := ipNet.Mask
			if len(mask) == 4 {
				broadcast := net.IPv4(
					ip4[0]|^mask[0],
					ip4[1]|^mask[1],
					ip4[2]|^mask[2],
					ip4[3]|^mask[3],
				)
				addrs = append(addrs, fmt.Sprintf("%s:%d", broadcast.String(), port))
			}
		}
	}
	return addrs
}

// DiscoverViaUDP broadcasts a discovery probe and waits for a server response
func DiscoverViaUDP(ctx context.Context, port int, timeout time.Duration) (string, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return "", fmt.Errorf("failed to open UDP listener: %w", err)
	}
	defer conn.Close()

	destAddrs := GetBroadcastAddresses(port)
	probeMsg := []byte("TRANSIT_TRACKER_DISCOVER\n")

	for _, dest := range destAddrs {
		udpDest, err := net.ResolveUDPAddr("udp4", dest)
		if err == nil {
			_, _ = conn.WriteTo(probeMsg, udpDest)
		}
	}

	// Buffer for response
	buf := make([]byte, 1024)
	deadline := time.Now().Add(timeout)
	_ = conn.SetReadDeadline(deadline)

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		n, remoteAddr, err := conn.ReadFrom(buf)
		if err != nil {
			return "", fmt.Errorf("no server replied to discovery UDP broadcast: %w", err)
		}

		offer, err := ParseDiscoveryOffer(string(buf[:n]))
		if err == nil && offer.URL != "" && AdoptableServerURL(offer.URL) {
			// Verify server reachability via quick HEAD probe
			if verifyServerFn(ctx, offer.URL, 800*time.Millisecond) {
				return offer.URL, nil
			}
			// If offer URL contains localhost or 0.0.0.0, fallback to remoteAddr IP
			if strings.Contains(offer.URL, "localhost") || strings.Contains(offer.URL, "127.0.0.1") {
				if udpRemote, ok := remoteAddr.(*net.UDPAddr); ok {
					altURL := fmt.Sprintf("http://%s:%d", udpRemote.IP.String(), DefaultServerPort)
					if verifyServerFn(ctx, altURL, 800*time.Millisecond) {
						return altURL, nil
					}
				}
			}
		}
	}
}

// DiscoverViaSubnetSweep scans the local /24 subnet for a running bus tracker server
func DiscoverViaSubnetSweep(ctx context.Context, httpPort int) (string, error) {
	ifaces, err := netInterfaces()
	if err != nil {
		return "", err
	}

	var baseSubnets []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil {
				continue
			}
			ip4 := ipNet.IP.To4()
			base := fmt.Sprintf("%d.%d.%d.", ip4[0], ip4[1], ip4[2])
			baseSubnets = append(baseSubnets, base)
		}
	}

	if len(baseSubnets) == 0 {
		baseSubnets = append(baseSubnets, "192.168.86.", "192.168.1.")
	}

	sweepCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Snapshot the verification function so probe goroutines (which may outlive
	// this call when we return early on a match) capture a stable local value
	// rather than re-reading the seam concurrently with test cleanup.
	verify := verifyServerFn

	resultChan := make(chan string, 1)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 48) // concurrent probe limit

	for _, prefix := range baseSubnets {
		for i := 1; i <= 254; i++ {
			host := fmt.Sprintf("http://%s%d:%d", prefix, i, httpPort)
			wg.Add(1)
			go func(candidate string) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-sweepCtx.Done():
					return
				}

				if verify(sweepCtx, candidate, 400*time.Millisecond) {
					select {
					case resultChan <- candidate:
						cancel()
					default:
					}
				}
			}(host)
		}
	}

	// Wait for completion or match in separate goroutine
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case found := <-resultChan:
		return found, nil
	case <-done:
		return "", fmt.Errorf("no server found in subnet sweep")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// verifyServer checks if candidate responds to HEAD /tracker-arm
func verifyServer(ctx context.Context, serverURL string, timeout time.Duration) bool {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "HEAD", serverURL+"/tracker-arm", nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// PersistServerURL writes the discovered server URL to persistent storage
func PersistServerURL(serverURL string, writeFile func(string, []byte, os.FileMode) error) error {
	trimmed := strings.TrimSpace(serverURL)
	if trimmed == "" {
		return fmt.Errorf("empty server url")
	}

	paths := []string{ServerConfigFile, FallbackConfigFile}
	var lastErr error
	success := false

	for _, p := range paths {
		dir := filepath.Dir(p)
		_ = os.MkdirAll(dir, 0755)
		if err := writeFile(p, []byte(trimmed+"\n"), 0644); err == nil {
			success = true
		} else {
			lastErr = err
		}
	}

	if success {
		return nil
	}
	return lastErr
}

// SaveServerURL uses production os.WriteFile
func SaveServerURL(serverURL string) error {
	return PersistServerURL(serverURL, os.WriteFile)
}

// AutoDiscoverServer tries UDP broadcast first, then falls back to subnet sweep
func AutoDiscoverServer(ctx context.Context) (string, error) {
	// 1. Try fast UDP broadcast discovery
	udpCtx, cancelUDP := context.WithTimeout(ctx, 2*time.Second)
	defer cancelUDP()

	if discovered, err := discoverViaUDP(udpCtx, DefaultDiscoveryPort, 1500*time.Millisecond); err == nil && discovered != "" {
		_ = saveServerURL(discovered)
		return discovered, nil
	}

	// 2. Fall back to fast local subnet sweep
	sweepCtx, cancelSweep := context.WithTimeout(ctx, 3*time.Second)
	defer cancelSweep()

	if discovered, err := discoverViaSweep(sweepCtx, DefaultServerPort); err == nil && discovered != "" {
		_ = saveServerURL(discovered)
		return discovered, nil
	}

	return "", fmt.Errorf("auto-discovery could not locate bus tracker server")
}
