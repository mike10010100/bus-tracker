package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// patchDiagSeams restores the diagnostics probe seams after the test. Note the
// process-listing seam (execCommandContext) is left to the real implementation
// because it returns a concrete *exec.Cmd; on a machine without `ps` it simply
// yields an empty string, which the code tolerates.
func patchDiagSeams(t *testing.T) {
	t.Helper()
	origHostname := hostname
	origStat := osStat
	origLookPath := lookPath
	origRead := osReadFile
	t.Cleanup(func() {
		hostname = origHostname
		osStat = origStat
		lookPath = origLookPath
		osReadFile = origRead
	})
}

type fakeFileInfo struct{ dir bool }

func (f fakeFileInfo) Name() string       { return "x" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() interface{}   { return nil }

func TestGatherDiagnostics_ProbesAndCapabilities(t *testing.T) {
	patchDiagSeams(t)
	hostname = func() (string, error) { return "kindle-test\n", nil }
	osStat = func(path string) (os.FileInfo, error) {
		if path == "/mnt/us/extensions" {
			return fakeFileInfo{dir: true}, nil
		}
		return nil, os.ErrNotExist
	}
	osReadFile = func(path string) ([]byte, error) {
		switch path {
		case "/proc/uptime":
			return []byte("3600.00 100.00\n"), nil
		case "/etc/version":
			return []byte("Kindle 5.16.2.1.1\n"), nil
		}
		return nil, os.ErrNotExist
	}
	lookPath = func(name string) (string, error) {
		switch name {
		case "kron":
			return "/usr/local/bin/kron", nil
		case "rtcwake":
			return "/sbin/rtcwake", nil
		case "eips":
			return "/usr/bin/eips", nil
		case "lipc-set-prop":
			return "/usr/bin/lipc-set-prop", nil
		}
		return "", errors.New("not found")
	}

	d := GatherDiagnostics()
	if d.Hostname != "kindle-test" {
		t.Errorf("hostname = %q, want kindle-test", d.Hostname)
	}
	if !d.Capabilities["has_kron"] {
		t.Error("expected has_kron true")
	}
	if !d.Capabilities["has_rtcwake"] {
		t.Error("expected has_rtcwake true")
	}
	if !d.Capabilities["has_lipc"] {
		t.Error("expected has_lipc true")
	}
	if !d.Capabilities["is_jailbroken"] {
		t.Error("expected is_jailbroken true (extensions dir present)")
	}
	if d.Capabilities["has_cron"] {
		t.Error("expected has_cron false")
	}
	if d.Uptime != "3600.00 100.00" {
		t.Errorf("uptime = %q", d.Uptime)
	}
	if d.BootTime.IsZero() {
		t.Error("expected boot time to be derived from uptime")
	}
	// The directory probe should be classified as a directory, not missing.
	if d.Files["/mnt/us/extensions"] != "<directory>" {
		t.Errorf("expected extensions dir marker, got %q", d.Files["/mnt/us/extensions"])
	}
}

func TestGatherDiagnostics_MissingEverythingDoesNotPanic(t *testing.T) {
	patchDiagSeams(t)
	hostname = func() (string, error) { return "", errors.New("no hostname") }
	osStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	osReadFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	lookPath = func(string) (string, error) { return "", errors.New("nope") }

	d := GatherDiagnostics()
	if d.Capabilities["has_kron"] || d.Capabilities["has_cron"] || d.Capabilities["is_jailbroken"] {
		t.Error("expected all capabilities false when nothing is present")
	}
	if d.Files["/proc/uptime"] != "<missing>" {
		t.Errorf("expected /proc/uptime missing, got %q", d.Files["/proc/uptime"])
	}
	if !d.BootTime.IsZero() {
		t.Error("expected zero boot time when uptime is missing")
	}
}

func TestDiagnosticsFormat_IncludesBattery(t *testing.T) {
	d := Diagnostics{
		Version:         "1",
		BatteryLevel:    83,
		BatteryCharging: true,
		Files:           map[string]string{},
		Commands:        map[string]string{},
		Capabilities:    map[string]bool{},
	}
	out := d.Format()
	if !strings.Contains(out, "battery: 83% (charging)") {
		t.Errorf("expected battery line, got:\n%s", out)
	}
	if !strings.Contains(out, "battery_level=83 charging=true") {
		t.Errorf("expected machine-readable battery line, got:\n%s", out)
	}
}

func TestDiagnosticsFormat_UnknownBattery(t *testing.T) {
	d := Diagnostics{
		Version:      "1",
		BatteryLevel: -1,
		Files:        map[string]string{},
		Commands:     map[string]string{},
		Capabilities: map[string]bool{},
	}
	out := d.Format()
	if !strings.Contains(out, "battery: <unknown>") {
		t.Errorf("expected unknown battery line, got:\n%s", out)
	}
	if strings.Contains(out, "battery_level=") {
		t.Errorf("unknown battery must not emit a machine-readable line, got:\n%s", out)
	}
}

func TestDiagnosticsFormat_ContainsKeySections(t *testing.T) {
	d := Diagnostics{
		Version:  "9.9.9",
		GoOS:     "linux",
		GoArch:   "arm",
		Hostname: "kindle",
		Uptime:   "12.0 3.0",
		Files: map[string]string{
			"/etc/version": "5.16.2.1.1",
			"/etc/crontab": "<missing>",
			"/mnt/us/kron": "<directory>",
		},
		Commands:     map[string]string{"kron": "/usr/local/bin/kron", "cron": "<not found>"},
		Capabilities: map[string]bool{"has_kron": true, "has_cron": false},
		Processes:    "PID CMD\n1 init",
	}
	out := d.Format()
	for _, want := range []string{
		"=== DIAGNOSTICS v9.9.9 ===",
		"runtime: linux/arm host=kindle",
		"--- capabilities ---",
		"has_kron:",
		"/usr/local/bin/kron",
		"5.16.2.1.1",
		"<missing>",
		"<directory>",
		"--- processes ---",
		"=== END DIAGNOSTICS ===",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Format() missing %q\n---\n%s", want, out)
		}
	}
}

func TestDiagnosticsFormat_TruncatesLargeFiles(t *testing.T) {
	d := Diagnostics{
		Version:      "1",
		Files:        map[string]string{"/proc/cpuinfo": strings.Repeat("x", 5000)},
		Commands:     map[string]string{},
		Capabilities: map[string]bool{},
	}
	out := d.Format()
	if !strings.Contains(out, "…[truncated]") {
		t.Error("expected large file to be truncated")
	}
	if strings.Count(out, "x") >= 5000 {
		t.Error("expected truncated output to be shorter than input")
	}
}

func TestRunActiveProbe_FormatsReport(t *testing.T) {
	origGlob := globInputs
	origCmdCtx := execCommandContext
	t.Cleanup(func() {
		globInputs = origGlob
		execCommandContext = origCmdCtx
	})

	globInputs = func(pattern string) ([]string, error) {
		switch pattern {
		case "/dev/rtc*":
			return []string{"/dev/rtc0", "/dev/rtc1"}, nil
		case "/sys/class/rtc/*":
			return []string{"/sys/class/rtc/rtc0"}, nil
		}
		return nil, nil
	}
	osReadFile = func(path string) ([]byte, error) {
		if path == "/etc/upstart/custom-login" {
			return []byte("#!/bin/sh\nexec /usr/bin/tracker\n"), nil
		}
		return nil, os.ErrNotExist
	}
	// Every probe command succeeds via `echo`, giving predictable stdout.
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		_ = ctx
		return exec.Command("echo", append([]string{name}, arg...)...)
	}

	p := RunActiveProbe()
	out := p.Format()
	for _, want := range []string{
		"=== ACTIVE PROBE v",
		"--- identity ---",
		"--- crontab -l ---",
		"--- rtc devices ---",
		"/dev/rtc0",
		"--- /etc/upstart/custom-login ---",
		"--- launcher hunt ---",
		"--- powerd lipc props ---",
		"=== END ACTIVE PROBE ===",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("probe output missing %q\n---\n%s", want, out)
		}
	}
}

func TestRunActiveProbe_NoRTCDevices(t *testing.T) {
	origGlob := globInputs
	origCmdCtx := execCommandContext
	t.Cleanup(func() {
		globInputs = origGlob
		execCommandContext = origCmdCtx
	})
	globInputs = func(string) ([]string, error) { return nil, nil }
	osReadFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		return exec.Command("echo", name)
	}

	p := RunActiveProbe()
	out := p.Format()
	if !strings.Contains(out, "<none>") {
		t.Errorf("expected '<none>' for absence of RTC devices, got:\n%s", out)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("  hello  ", 10); got != "hello" {
		t.Errorf("truncate trim = %q", got)
	}
	long := strings.Repeat("x", 20)
	if got := truncate(long, 5); !strings.HasPrefix(got, "xxxxx") || !strings.Contains(got, "truncated") {
		t.Errorf("truncate long = %q", got)
	}
}

func TestGatherLauncherInfo_ListsPaths(t *testing.T) {
	origCmdCtx := execCommandContext
	origGlob := globInputs
	t.Cleanup(func() {
		execCommandContext = origCmdCtx
		globInputs = origGlob
	})
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		return exec.Command("echo", name)
	}
	globInputs = func(pattern string) ([]string, error) {
		if pattern == "/etc/upstart/*.conf" {
			return []string{"/etc/upstart/framework.conf"}, nil
		}
		return nil, nil
	}
	osStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	osReadFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }

	out := gatherLauncherInfo()
	for _, want := range []string{
		"/etc/upstart",
		"/mnt/us/emergency.sh",
		"--- /etc/crontab/root ---",
		"--- /var/local/kmc/run_hotfix.sh ---",
		"--- /mnt/us/documents/BusTracker.sh ---",
		"--- process tree ---",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("launcher info missing %q\n%s", want, out)
		}
	}
}

func TestGatherLauncherInfo_DumpsSchedulerContents(t *testing.T) {
	origCmdCtx := execCommandContext
	origGlob := globInputs
	t.Cleanup(func() {
		execCommandContext = origCmdCtx
		globInputs = origGlob
	})
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		return exec.Command("echo", name)
	}
	globInputs = func(string) ([]string, error) { return nil, nil }
	osStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	osReadFile = func(path string) ([]byte, error) {
		if path == "/var/local/kmc/run_hotfix.sh" {
			return []byte("#!/bin/sh\nexec BusTracker.sh\n"), nil
		}
		if path == "/etc/crontab/root" {
			return []byte("# cron dir\n"), nil
		}
		return nil, os.ErrNotExist
	}

	out := gatherLauncherInfo()
	if !strings.Contains(out, "exec BusTracker.sh") {
		t.Errorf("expected KMC hotifx contents, got:\n%s", out)
	}
	if !strings.Contains(out, "# cron dir") {
		t.Errorf("expected crontab contents, got:\n%s", out)
	}
}

func TestIndent(t *testing.T) {
	if got := indent(""); got != "  <empty>" {
		t.Errorf("indent empty = %q", got)
	}
	if got := indent("a\nb"); got != "  a\n  b" {
		t.Errorf("indent = %q", got)
	}
}

func TestParseUptimeSeconds(t *testing.T) {
	if got := parseUptimeSeconds("3600.00 100.00"); got != 3600 {
		t.Errorf("got %v", got)
	}
	if got := parseUptimeSeconds(""); got != 0 {
		t.Errorf("expected 0 for empty, got %v", got)
	}
	if got := parseUptimeSeconds("garbage"); got != 0 {
		t.Errorf("expected 0 for garbage, got %v", got)
	}
}

// ensure exec seam is referenced so the import stays meaningful if the real
// process listing is exercised.
var _ = exec.Command
