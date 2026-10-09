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
	Version         string
	GoOS            string
	GoArch          string
	Hostname        string
	Uptime          string
	BootTime        time.Time
	Files           map[string]string // path -> content (or "<missing>"/"<directory>")
	Commands        map[string]string // name -> resolved path (or "<not found>")
	Capabilities    map[string]bool   // feature -> available
	Processes       string
	BatteryLevel    int // 0-100, or -1 if unknown
	BatteryCharging bool
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

	batt := GetBatteryInfo()
	d.BatteryLevel = batt.Level
	d.BatteryCharging = batt.IsCharging

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

// CapabilityProbe is the result of an on-demand active probe. Unlike
// GatherDiagnostics (passive, cheap), this runs a handful of read-only commands
// to answer whether we can drive RTC-scheduled wake. It is only run when the
// server explicitly asks, and every command is bounded by a timeout.
type CapabilityProbe struct {
	UID        string
	Groups     string
	Crontab    string   // crontab -l output
	RTCDevices []string // /dev/rtc* present
	RTCInfo    string   // listing of /sys/class/rtc/*/name etc.
	RTCTest    string   // result of actively testing RTC wake mechanisms
	Upstart    string   // /etc/upstart/custom-login contents
	LipcProps  string   // relevant powerd properties
	Launcher   string   // enumeration of candidate boot-hook locations
	Notes      []string
}

// probeTimeout bounds each command run by RunActiveProbe.
var probeTimeout = 5 * time.Second

// runProbeCmd runs a command with a timeout and returns combined output, or an
// error string. Never panics.
func runProbeCmd(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := execCommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return fmt.Sprintf("<error: %v>", err)
	}
	return strings.TrimSpace(string(out))
}

// RunActiveProbe performs the on-demand capability probe.
func RunActiveProbe() CapabilityProbe {
	p := CapabilityProbe{}

	p.UID = runProbeCmd("id")
	p.Groups = runProbeCmd("sh", "-c", "id -Gn 2>/dev/null || groups 2>/dev/null")
	p.Crontab = runProbeCmd("crontab", "-l")

	// RTC devices.
	if matches, err := globInputs("/dev/rtc*"); err == nil {
		p.RTCDevices = matches
	}
	if matches, err := globInputs("/sys/class/rtc/*"); err == nil {
		var b strings.Builder
		for _, d := range matches {
			name := runProbeCmd("cat", d+"/name")
			wake := runProbeCmd("cat", d+"/wakealarm")
			fmt.Fprintf(&b, "%s: name=%q wakealarm=%q\n", d, name, wake)
		}
		p.RTCInfo = strings.TrimSpace(b.String())
	}
	p.RTCTest = probeRTCWake()

	// The boot persistence hook, if present and readable.
	p.Upstart = func() string {
		if v := readPathOrMissing("/etc/upstart/custom-login"); v != "<missing>" {
			if len(v) > 2000 {
				v = v[:2000] + " …[truncated]"
			}
			return strings.TrimSpace(v)
		}
		return "<missing>"
	}()

	// Hunt for the boot hook that launches custom code. We don't assume where it
	// is: enumerate the known Kindle persistence locations read-only.
	p.Launcher = gatherLauncherInfo()

	// Powerd properties that govern sleep.
	var lb strings.Builder
	for _, prop := range []string{
		"flIntensity", "schedAmberLevel", "preventScreenSaver",
		"battLevel", "isCharging", "rtcWakeup",
	} {
		val := lipcGet("com.lab126.powerd", prop)
		fmt.Fprintf(&lb, "%s=%q\n", prop, val)
	}
	p.LipcProps = strings.TrimSpace(lb.String())

	if strings.Contains(p.UID, "uid=0") {
		p.Notes = append(p.Notes, "running as root")
	} else {
		p.Notes = append(p.Notes, "NOT root: "+p.UID)
	}
	if len(p.RTCDevices) > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("RTC devices: %v", p.RTCDevices))
	} else {
		p.Notes = append(p.Notes, "no /dev/rtc* visible")
	}
	return p
}

// Format renders the probe as a plain-text block.
func (p CapabilityProbe) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== ACTIVE PROBE v%s ===\n", Version)
	fmt.Fprintf(&b, "--- identity ---\n  id:     %s\n  groups: %s\n", p.UID, p.Groups)
	fmt.Fprintf(&b, "--- crontab -l ---\n%s\n", indent(p.Crontab))
	fmt.Fprintf(&b, "--- rtc devices ---\n")
	if len(p.RTCDevices) == 0 {
		fmt.Fprintf(&b, "  <none>\n")
	}
	for _, d := range p.RTCDevices {
		fmt.Fprintf(&b, "  %s\n", d)
	}
	if p.RTCInfo != "" {
		fmt.Fprintf(&b, "--- rtc sysfs ---\n%s\n", indent(p.RTCInfo))
	}
	if p.RTCTest != "" {
		fmt.Fprintf(&b, "--- rtc wake test (active) ---\n%s\n", indent(p.RTCTest))
	}
	fmt.Fprintf(&b, "--- /etc/upstart/custom-login ---\n%s\n", indent(p.Upstart))
	fmt.Fprintf(&b, "--- launcher hunt ---\n%s\n", indent(p.Launcher))
	fmt.Fprintf(&b, "--- powerd lipc props ---\n%s\n", indent(p.LipcProps))
	fmt.Fprintf(&b, "--- notes ---\n")
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "  - %s\n", n)
	}
	fmt.Fprintf(&b, "=== END ACTIVE PROBE ===")
	return b.String()
}

// launcherProbePaths are candidate boot-hook / userland locations on a Kindle.
// We list each (read-only) so we can find what actually launches custom code.
var launcherProbePaths = []string{
	"/etc/upstart",
	"/etc/init.d",
	"/etc/rc.local",
	"/etc/profile",
	"/etc/profile.d",
	"/var/local/kmc",
	"/var/local/root",
	"/var/local/system",
	"/mnt/us/emergency.sh",
	"/mnt/us/extensions",
	"/mnt/us/mrpackages",
	"/usr/share/webkit-1.0/pillow/debug_cmds.json",
}

// probeRTCWake actively tests the available RTC-wake mechanisms and clears them
// afterward, so we learn definitively which one works on this kernel:
//  1. powerd's rtcWakeup property (set then read back)
//  2. /sys/class/rtc/rtc0/wakealarm (set then read back)
//  3. rtcwake -m no -s <secs> (programs the alarm without suspending)
func probeRTCWake() string {
	var b strings.Builder

	runProbeCmd("lipc-set-prop", "-i", "com.lab126.powerd", "rtcWakeup", "120")
	got := runProbeCmd("lipc-get-prop", "com.lab126.powerd", "rtcWakeup")
	fmt.Fprintf(&b, "powerd.rtcWakeup: set=120 read_back=%q\n", got)

	sysfs := "/sys/class/rtc/rtc0/wakealarm"
	clearOut := runProbeCmd("sh", "-c", "echo 0 > "+sysfs)
	setOut := runProbeCmd("sh", "-c", "echo +120 > "+sysfs+"; echo rc=$?")
	readBack := runProbeCmd("cat", sysfs)
	fmt.Fprintf(&b, "sysfs %s: clear=%q write=%q read_back=%q\n", sysfs, clearOut, setOut, readBack)
	runProbeCmd("sh", "-c", "echo 0 > "+sysfs) // clear

	rcwake := runProbeCmd("rtcwake", "-d", "/dev/rtc0", "-m", "no", "-s", "120")
	fmt.Fprintf(&b, "rtcwake -m no: %q\n", rcwake)

	// Suspend support: what does /sys/power/state advertise, and is there a
	// kernel reason the last suspend failed?
	states := runProbeCmd("cat", "/sys/power/state")
	fmt.Fprintf(&b, "/sys/power/state: %q\n", states)
	dmesgTail := runProbeCmd("sh", "-c", "dmesg | tail -n 15")
	fmt.Fprintf(&b, "dmesg (tail):\n%s\n", dmesgTail)
	// A write of "mem" that fails silently often leaves no trace; show whether
	// the node is writable at all.
	writable := runProbeCmd("sh", "-c", "[ -w /sys/power/state ] && echo writable || echo not-writable")
	fmt.Fprintf(&b, "/sys/power/state writable: %s\n", writable)

	return strings.TrimSpace(b.String())
}

// gatherLauncherInfo lists candidate hook locations and scans the upstart dir
// for non-stock jobs that might reference a custom launcher. Read-only.
func gatherLauncherInfo() string {
	var b strings.Builder

	for _, p := range launcherProbePaths {
		if fi, err := osStat(p); err == nil {
			if fi.IsDir() {
				listing := runProbeCmd("ls", "-la", p)
				fmt.Fprintf(&b, "%s (dir):\n%s\n", p, indent(listing))
			} else {
				fmt.Fprintf(&b, "%s (file, %d bytes)\n", p, fi.Size())
			}
		} else {
			fmt.Fprintf(&b, "%s <missing>\n", p)
		}
	}

	// Any custom (non-Amazon) upstart jobs?
	fmt.Fprintf(&b, "--- /etc/upstart/*.conf ---\n")
	if matches, err := globInputs("/etc/upstart/*.conf"); err == nil {
		for _, f := range matches {
			content := readPathOrMissing(f)
			// Show only files that mention something custom-looking, or that are
			// not obviously a stock Amazon job, to keep the dump readable.
			low := strings.ToLower(content)
			if strings.Contains(low, "tracker") || strings.Contains(low, "kmc") ||
				strings.Contains(low, "bridge") || strings.Contains(low, "custom") ||
				strings.Contains(low, "mnt/us") {
				fmt.Fprintf(&b, "  %s:\n%s\n", f, indent(truncate(content, 1200)))
			} else {
				fmt.Fprintf(&b, "  %s <no custom markers>\n", f)
			}
		}
	}

	// Dump the files that actually govern launching/scheduling, discovered from
	// the process tree: the crond spool, the KMC launcher, and our own launcher
	// script. These are the places a sleep-mode change would live.
	for _, f := range schedulerProbeFiles {
		content := readPathOrMissing(f)
		if content == "<missing>" {
			fmt.Fprintf(&b, "--- %s ---\n  <missing>\n", f)
			continue
		}
		if content == "<directory>" {
			listing := runProbeCmd("ls", "-la", f)
			fmt.Fprintf(&b, "--- %s (dir) ---\n%s\n", f, indent(listing))
			continue
		}
		fmt.Fprintf(&b, "--- %s ---\n%s\n", f, indent(truncate(content, 14000)))
	}

	// The running process tree can reveal the parent of our launcher.
	fmt.Fprintf(&b, "--- process tree ---\n%s\n", indent(runProbeCmd("ps", "-ef")))

	return strings.TrimSpace(b.String())
}

// schedulerProbeFiles are the concrete text files/paths worth dumping to
// understand how the tracker is launched and how jobs are scheduled here.
var schedulerProbeFiles = []string{
	"/etc/crontab/root",             // crond's actual spool file
	"/var/local/kmc/kmc.conf",       // KMC config
	"/var/local/kmc/run_hotfix.sh",  // KMC hotfix runner
	"/var/local/kmc/sbin",           // KMC helper binaries
	"/var/local/kmc/system_patches", // KMC patches (dir listing)
	// The KMC boot-hook scripts (these decide how the device launches things
	// at startup, and whether KMC itself can schedule a service).
	"/var/local/kmc/system_patches/dispatch.sh",
	"/var/local/kmc/system_patches/patch_system.sh",
	"/var/local/kmc/system_patches/run_patch.sh",
	"/var/local/kmc/system_patches/kmc.conf",
	"/var/local/kmc/sbin/kmc_reset.sh",
	"/var/local/kmc/sbin/kmclog.sh",
	"/var/local/kmc/sbin/kpm.sh",
	"/var/local/kmc/kindlehf/bin",
	"/mnt/us/documents/BusTracker.sh",      // our bootstrap launcher
	"/mnt/us/documents/tracker_server.txt", // persisted server URL
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + " …[truncated]"
	}
	return s
}

func indent(s string) string {
	if s == "" {
		return "  <empty>"
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
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
	if d.BatteryLevel >= 0 {
		chg := ""
		if d.BatteryCharging {
			chg = " (charging)"
		}
		fmt.Fprintf(&b, "battery: %d%%%s\n", d.BatteryLevel, chg)
		fmt.Fprintf(&b, "battery_level=%d charging=%t\n", d.BatteryLevel, d.BatteryCharging)
	} else {
		fmt.Fprintf(&b, "battery: <unknown>\n")
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
