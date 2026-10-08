package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
)

var DefaultCandidateServers = []string{
	"http://192.168.86.193:8000",
	"http://192.168.1.100:8000",
}

// ResolveServerURL determines the target host server URL in order of priority:
// 1. Command-line argument (-server <url> or -server=<url>)
// 2. Environment variable (TRACKER_SERVER)
// 3. Configuration file in Kindle storage (/mnt/us/documents/tracker_server.txt)
// 4. LAN Auto-discovery (UDP broadcast & Subnet sweep)
// 5. Default candidate servers probe
func ResolveServerURL(
	args []string,
	getenv func(string) string,
	readFile func(string) ([]byte, error),
) string {
	return ResolveServerURLWithDiscoverer(args, getenv, readFile, nil)
}

// ResolveServerURLWithDiscoverer allows dependency injection of auto-discovery for testing
func ResolveServerURLWithDiscoverer(
	args []string,
	getenv func(string) string,
	readFile func(string) ([]byte, error),
	discoverer func() (string, error),
) string {
	// 1. Command-line flags
	for i, arg := range args {
		if (arg == "-server" || arg == "--server") && i+1 < len(args) {
			val := strings.TrimSpace(args[i+1])
			if val != "" {
				return val
			}
		}
		if strings.HasPrefix(arg, "-server=") || strings.HasPrefix(arg, "--server=") {
			parts := strings.SplitN(arg, "=", 2)
			val := strings.TrimSpace(parts[1])
			if val != "" {
				return val
			}
		}
	}

	// 2. Environment variable
	if getenv != nil {
		if envVal := strings.TrimSpace(getenv("TRACKER_SERVER")); envVal != "" {
			return envVal
		}
	}

	// 3. Configuration files
	if readFile != nil {
		paths := []string{
			ServerConfigFile,
			FallbackConfigFile,
		}
		for _, p := range paths {
			if data, err := readFile(p); err == nil {
				val := strings.TrimSpace(string(data))
				if val != "" {
					return val
				}
			}
		}
	}

	// 4. LAN Auto-Discovery
	if discoverer != nil {
		if discovered, err := discoverer(); err == nil && discovered != "" {
			return discovered
		}
	}

	// 5. Test candidate servers with short timeout to detect reachable host
	for _, candidate := range DefaultCandidateServers {
		client := &http.Client{Timeout: 400 * time.Millisecond}
		resp, err := client.Head(candidate + "/tracker-arm")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return candidate
			}
		}
	}

	return DefaultCandidateServers[0]
}

// GetServerURL resolves server URL using production environment and auto-discovery
func GetServerURL() string {
	return ResolveServerURLWithDiscoverer(
		os.Args,
		os.Getenv,
		os.ReadFile,
		func() (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return AutoDiscoverServer(ctx)
		},
	)
}
