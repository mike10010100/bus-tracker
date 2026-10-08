package main

import (
	"os"
	"strings"
)

const (
	DefaultServerURL = "http://192.168.1.100:8000"
)

// ResolveServerURL determines the target host server URL in order of priority:
// 1. Command-line argument (-server <url> or -server=<url>)
// 2. Environment variable (TRACKER_SERVER)
// 3. Configuration file in Kindle storage (/mnt/us/documents/tracker_server.txt)
// 4. Default fallback URL
func ResolveServerURL(
	args []string,
	getenv func(string) string,
	readFile func(string) ([]byte, error),
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
			"/mnt/us/documents/tracker_server.txt",
			"/tmp/tracker_server.txt",
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

	// 4. Fallback default
	return DefaultServerURL
}

// GetServerURL resolves server URL using production environment
func GetServerURL() string {
	return ResolveServerURL(os.Args, os.Getenv, os.ReadFile)
}
