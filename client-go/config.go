package main

import (
	"context"
	"os"
	"strings"
	"time"
)

// DefaultCandidateServers lists fallback server addresses probed in sequence.
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
				if !strings.HasPrefix(val, "http://") && !strings.HasPrefix(val, "https://") {
					val = "http://" + val
				}
				return val
			}
		}
		if strings.HasPrefix(arg, "-server=") || strings.HasPrefix(arg, "--server=") {
			parts := strings.SplitN(arg, "=", 2)
			val := strings.TrimSpace(parts[1])
			if val != "" {
				if !strings.HasPrefix(val, "http://") && !strings.HasPrefix(val, "https://") {
					val = "http://" + val
				}
				return val
			}
		}
	}

	// 2. Environment variable
	if getenv != nil {
		if envVal := strings.TrimSpace(getenv("TRACKER_SERVER")); envVal != "" {
			if !strings.HasPrefix(envVal, "http://") && !strings.HasPrefix(envVal, "https://") {
				envVal = "http://" + envVal
			}
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
					if !strings.HasPrefix(val, "http://") && !strings.HasPrefix(val, "https://") {
						val = "http://" + val
					}
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
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		ok := verifyServerFn(ctx, candidate, 400*time.Millisecond)
		cancel()
		if ok {
			return candidate
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
			return autoDiscover(ctx)
		},
	)
}

// RunMode selects how long the client runs.
type RunMode int

const (
	// ModeResident is the default: stay alive and poll on a timer.
	ModeResident RunMode = iota
	// ModeOneshot fetches and draws exactly once, then exits, leaving the
	// rendered image on screen (it does NOT clear the display).
	ModeOneshot
	// ModeSleep renders once, programs an RTC wake for the next interval, and
	// lets the device suspend, looping on each resume. This is the low-power
	// mode for RTC-capable devices (Kindle + rtcwake/powerd).
	ModeSleep
)

// wantsSuspend reports whether the `-suspend` flag was passed. Sleep mode only
// actually suspends the device when this is set, so the risky suspend path is
// strictly opt-in.
func wantsSuspend(args []string) bool {
	for _, arg := range args {
		switch strings.ToLower(strings.TrimSpace(arg)) {
		case "-suspend", "--suspend":
			return true
		}
	}
	return false
}

// currentRunMode holds the mode this process is running in, so the fetch path
// can compare it against a server-requested mode and relaunch on change.
var currentRunMode = ModeResident

// currentModeName returns the name of the running mode, including the distinct
// "sleep-suspend" variant, for comparison against server-requested modes.
func currentModeName() string {
	if currentRunMode == ModeSleep && wantsSuspend(os.Args) {
		return "sleep-suspend"
	}
	return currentRunMode.String()
}

// modeFlags maps a server-requested mode name to the CLI flags that select it.
// Returns nil for an unknown mode.
func modeFlags(name string) []string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "resident":
		return []string{"-resident"}
	case "oneshot":
		return []string{"-oneshot"}
	case "sleep":
		return []string{"-sleep"}
	case "sleep-suspend":
		return []string{"-sleep", "-suspend"}
	default:
		return nil
	}
}

// String renders the mode for logging.
func (m RunMode) String() string {
	switch m {
	case ModeOneshot:
		return "oneshot"
	case ModeSleep:
		return "sleep"
	default:
		return "resident"
	}
}

// ResolveRunMode parses the run mode from arguments. Flags:
//
//	-oneshot            -> ModeOneshot
//	-sleep              -> ModeSleep
//	-resident (default) -> ModeResident
//
// -oneshot wins if both appear (it is the more conservative, single-shot mode).
func ResolveRunMode(args []string) RunMode {
	mode := ModeResident
	for _, arg := range args {
		switch strings.ToLower(strings.TrimSpace(arg)) {
		case "-oneshot", "--oneshot":
			return ModeOneshot
		case "-sleep", "--sleep":
			mode = ModeSleep
		case "-resident", "--resident":
			mode = ModeResident
		}
	}
	return mode
}

// ResolveViewMode parses the initial view mode from command-line arguments ('morning', 'evening', 'auto').
// Defaults to 'auto'.
func ResolveViewMode(args []string) string {
	for i, arg := range args {
		if (arg == "-view" || arg == "--view") && i+1 < len(args) {
			val := strings.ToLower(strings.TrimSpace(args[i+1]))
			if val == "morning" || val == "evening" || val == "auto" {
				return val
			}
		}
		if strings.HasPrefix(arg, "-view=") || strings.HasPrefix(arg, "--view=") {
			parts := strings.SplitN(arg, "=", 2)
			val := strings.ToLower(strings.TrimSpace(parts[1]))
			if val == "morning" || val == "evening" || val == "auto" {
				return val
			}
		}
	}
	return "auto"
}
