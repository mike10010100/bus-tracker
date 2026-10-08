package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Diagnostics is a point-in-time report of what the device and jailbreak
// expose. It is purely observational: every probe is read-only so it is safe
// to run on every startup.
type Diagnostics struct {
	Version      string
	GoOS         string
	GoArch       string
	Hostname     string
	Uptime       string
	BootTime     time.Time
	Files        map[string]string // path -> content (or "<missing>"/"<directory>")
	Commands     map[string]string // name -> resolved path (or "<not found>")
	Capabilities map[string]bool   // feature -> available
	Processes    string
}

// diagnosticPaths are files worth dumping (firmware markers, jailbreak layout,
// cron configs).
var diagnosticPaths = []string{
	"/etc/version",
	"/etc/prettyversion.txt",
	"/proc/uptime",
	"/proc/version",
	"/etc/os-release",
	"/etc/crontab",
	"/var/spool/cron",
	"/var/spool/cron/crontabs/root",
	"/mnt/us/extensions",
	"/mnt/us/documents",
	"/mnt/us/kron",
	"/usr/local/bin/kron",
	"/usr/bin/kron",
	"/etc/kron.conf",
}

// diagnosticCommands are binaries whose presence reveals what scheduling,
// networking, and jailbreak tooling exists.
var diagnosticCommands = []string{
	"cron", "crond", "crontab", "kron", "kcron", "kual",
	"rtcwake", "hwclock", "sh", "bash", "go",
	"lipc-set-prop", "lipc-get-prop", "eips", "curl", "wget",
	"dropbear", "ssh", "python3", "busybox",
}

// GatherDiagnostics probes the device read-only and returns a report. It never
// mutates state and tolerates every individual probe failing.
func GatherDiagnostics() Diagnostics {
	d := Diagnostics{
		Version:      Version,
		GoOS:         runtime.GOOS,
		GoArch:       runtime.GOARCH,
		Files:        make(map[string]string),
		Commands:     make(map[string]string),
		Capabilities: make(map[string]bool),
	}

	if hn, err := hostname(); err == nil {
		d.Hostname = strings.TrimSpace(hn)
	}

	for _, p := range diagnosticPaths {
		d.Files[p] = readPathOrMissing(p)
	}

	if up, ok := d.Files["/proc/uptime"]; ok && up != "<missing>" && up != "<directory>" {
		d.Uptime = strings.TrimSpace(up)
		if secs := parseUptimeSeconds(d.Uptime); secs > 0 {
			d.BootTime = time.Now().Add(-time.Duration(secs * float64(time.Second)))
		}
	}

	for _, c := range diagnosticCommands {
		d.Commands[c] = lookupCommand(c)
	}

	found := func(name string) bool { return d.Commands[name] != "<not found>" }
	exists := func(path string) bool {
		v, ok := d.Files[path]
		return ok && v != "<missing>"
	}

	d.Capabilities["has_cron"] = found("cron") || found("crond") || found("crontab")
	d.Capabilities["has_kron"] = found("kron") || found("kcron") || exists("/mnt/us/kron")
	d.Capabilities["has_rtcwake"] = found("rtcwake")
	d.Capabilities["has_hwclock"] = found("hwclock")
	d.Capabilities["has_kual"] = found("kual") || exists("/mnt/us/extensions")
	d.Capabilities["has_lipc"] = found("lipc-set-prop")
	d.Capabilities["has_eips"] = found("eips")
	d.Capabilities["has_ssh"] = found("dropbear") || found("ssh")
	d.Capabilities["is_jailbroken"] = exists("/mnt/us/extensions") || found("kual")

	d.Processes = listProcesses()
	return d
}

// Format renders the report as a plain-text block suitable for POSTing to the
// server's diagnostic endpoint.
func (d Diagnostics) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== DIAGNOSTICS v%s ===\n", d.Version)
	fmt.Fprintf(&b, "runtime: %s/%s host=%s\n", d.GoOS, d.GoArch, d.Hostname)
	if !d.BootTime.IsZero() {
		fmt.Fprintf(&b, "uptime: %s (booted ~%s)\n", d.Uptime, d.BootTime.Format(time.RFC3339))
	} else {
		fmt.Fprintf(&b, "uptime: %s\n", d.Uptime)
	}

	fmt.Fprintf(&b, "--- capabilities ---\n")
	for _, k := range sortedBoolKeys(d.Capabilities) {
		fmt.Fprintf(&b, "  %-16s %t\n", k+":", d.Capabilities[k])
	}

	fmt.Fprintf(&b, "--- commands ---\n")
	for _, k := range sortedStringKeys(d.Commands) {
		fmt.Fprintf(&b, "  %-14s %s\n", k+":", d.Commands[k])
	}

	fmt.Fprintf(&b, "--- files ---\n")
	for _, k := range sortedStringKeys(d.Files) {
		v := d.Files[k]
		if v == "<missing>" {
			fmt.Fprintf(&b, "  %-40s <missing>\n", k)
			continue
		}
		if v == "<directory>" {
			fmt.Fprintf(&b, "  %-40s <directory>\n", k)
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) > 1200 {
			v = v[:1200] + " …[truncated]"
		}
		fmt.Fprintf(&b, "  %s:\n    %s\n", k, strings.ReplaceAll(v, "\n", "\n    "))
	}

	if d.Processes != "" {
		fmt.Fprintf(&b, "--- processes ---\n%s\n", d.Processes)
	}
	fmt.Fprintf(&b, "=== END DIAGNOSTICS ===")
	return b.String()
}

func readPathOrMissing(path string) string {
	if fi, err := osStat(path); err == nil && fi.IsDir() {
		return "<directory>"
	}
	data, err := osReadFile(path)
	if err != nil {
		return "<missing>"
	}
	return string(data)
}

func lookupCommand(name string) string {
	path, err := lookPath(name)
	if err != nil || path == "" {
		return "<not found>"
	}
	return path
}

func parseUptimeSeconds(s string) float64 {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	var secs float64
	fmt.Sscanf(fields[0], "%f", &secs)
	return secs
}

func listProcesses() string {
	cmd := execCommandContext(context.Background(), "ps", "w")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if len(s) > 4000 {
		s = s[:4000] + " …[truncated]"
	}
	return s
}

func sortedBoolKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hostname, osStat and lookPath are seams so the probe is testable without a
// real Kindle.
var (
	hostname = func() (string, error) {
		out, err := execCommand("hostname").Output()
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
	osStat   = os.Stat
	lookPath = exec.LookPath
)
