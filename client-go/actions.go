package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DeviceAction is a named, allowlisted maintenance action the server can ask a
// client to run. Actions are fixed in code (never arbitrary shell from the
// network), so a compromised/misconfigured server can only trigger these.
type DeviceAction struct {
	Fn func(ctx context.Context) string
	// Timeout bounds the action. A suspend test blocks for its whole duration,
	// so it needs a larger budget than a quick probe.
	Timeout time.Duration
}

// defaultActionTimeout bounds actions that don't specify their own.
var defaultActionTimeout = 60 * time.Second

// deviceActions maps action names to their implementations.
var deviceActions = map[string]DeviceAction{
	"disable-ads":      {Fn: actionDisableAds},
	"stop-framework":   {Fn: actionStopFramework},
	"start-framework":  {Fn: actionStartFramework},
	"framework-state":  {Fn: actionFrameworkState},
	"sleep-test":       {Fn: actionSleepTest},
	"rtc-suspend":      {Fn: actionRTCSuspend, Timeout: 5 * time.Minute},
	"input-wake-probe": {Fn: actionInputWakeProbe},
	"touch-wake-test":  {Fn: actionTouchWakeTest, Timeout: 5 * time.Minute},
}

// runAction executes a named action and returns a human-readable result.
func runAction(ctx context.Context, name string) (string, bool) {
	act, ok := deviceActions[name]
	if !ok {
		return fmt.Sprintf("unknown action %q", name), false
	}
	timeout := act.Timeout
	if timeout == 0 {
		timeout = defaultActionTimeout
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return act.Fn(actx), true
}

func shell(ctx context.Context, script string) string {
	cmd := execCommandContext(ctx, "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil && s == "" {
		return fmt.Sprintf("<error: %v>", err)
	}
	if err != nil {
		return fmt.Sprintf("%s\n<exit: %v>", s, err)
	}
	return s
}

// actionDisableAds removes Amazon Special Offers so the ad screensaver no longer
// covers the dashboard. Idempotent; reversible via the Amazon account.
func actionDisableAds(ctx context.Context) string {
	var b strings.Builder

	// Which sqlite tool exists? The Kindle image usually ships none of these.
	b.WriteString("sqlite tools: " + shell(ctx, "command -v sqlite3; command -v sqlite; command -v dbclient") + "\n")

	// Flip adunit.viewable=false via sqlite3 if present.
	db := "/var/local/appreg.db"
	if shell(ctx, "command -v sqlite3") != "" {
		sql := `sqlite3 ` + db + ` "UPDATE properties SET value='false' WHERE name='adunit.viewable';" 2>&1`
		b.WriteString("sqlite3 update: " + shell(ctx, sql) + "\n")
	}

	// Always remove the ad unit assets and the store marker.
	b.WriteString("rm adunits: " + shell(ctx, "rm -rf /var/local/adunits /mnt/us/.assets 2>&1; echo done") + "\n")

	// Verify the flag's raw bytes and the asset dirs.
	b.WriteString("flag: " + shell(ctx, "grep -a -o 'adunit.viewable[^ ]*' "+db+" 2>/dev/null | head -3; echo") + "\n")
	b.WriteString("assets: " + shell(ctx, "ls -la /var/local/adunits 2>&1 | head -2; ls -la /mnt/us/.assets 2>&1 | head -2") + "\n")
	return strings.TrimSpace(b.String())
}

// actionStopFramework stops the Amazon UI framework (lab126_gui/cvm) so nothing
// repaints over the dashboard and the device can idle-suspend. powerd and eips
// are unaffected.
func actionStopFramework(ctx context.Context) string {
	return shell(ctx, "stop lab126_gui 2>&1; stop framework 2>&1; initctl stop lab126_gui 2>&1; echo done; ps -eo pid,comm | grep -iE 'lab126|cvm|framework' 2>&1")
}

// actionStartFramework restarts the Amazon UI (for maintenance/reading).
func actionStartFramework(ctx context.Context) string {
	return shell(ctx, "start lab126_gui 2>&1; echo done")
}

// actionFrameworkState reports which framework daemons are running.
func actionFrameworkState(ctx context.Context) string {
	return shell(ctx, "ps -eo pid,user,comm 2>/dev/null | grep -iE 'lab126|powerd|blanket|framework|appmgrd|cvm' ; echo '---'; cat /sys/power/wake_lock 2>/dev/null; echo '---wakeup_count---'; cat /sys/power/wakeup_count 2>/dev/null")
}

// actionSleepTest reports the conditions relevant to a suspend attempt without
// actually suspending: wakelocks, framework presence, and the RTC alarm node.
func actionSleepTest(ctx context.Context) string {
	var b strings.Builder
	b.WriteString("wake_lock: " + shell(ctx, "cat /sys/power/wake_lock 2>/dev/null; echo") + "\n")
	b.WriteString("framework: " + shell(ctx, "ps -eo comm | grep -iE 'cvm|lab126' | tr '\\n' ' '") + "\n")
	b.WriteString("rtc: " + shell(ctx, "cat /sys/class/rtc/rtc0/wakealarm 2>/dev/null; echo") + "\n")
	b.WriteString("ads: " + shell(ctx, "ls /var/local/adunits 2>/dev/null && echo present || echo absent") + "\n")
	return strings.TrimSpace(b.String())
}

// actionRTCSuspend performs ONE proven suspend/wake cycle to validate the
// mechanism end to end:
//
//	stop lab126_gui, unload screensavers, disable wi-fi (releases "WLAN timeout"),
//	arm /sys/class/rtc/rtc0/wakealarm +90, echo mem > /sys/power/state,
//	(wake) re-enable wi-fi (the client's next poll proves the control channel).
//
// It reports what happened at each step. This is the decisive experiment.
func actionRTCSuspend(ctx context.Context) string {
	var b strings.Builder

	b.WriteString("stop framework: " + shell(ctx, "stop lab126_gui 2>&1; echo done") + "\n")
	b.WriteString("unload screensaver: " + shell(ctx, "lipc-set-prop com.lab126.blanket unload screensaver 2>&1; lipc-set-prop com.lab126.blanket unload splash 2>&1; echo done") + "\n")
	b.WriteString("frontlight off: " + shell(ctx, "lipc-set-prop -i com.lab126.powerd flIntensity 0 2>&1; echo done") + "\n")

	// Arm the RTC BEFORE suspending: clear then +90s.
	b.WriteString("arm rtc: " + shell(ctx, "echo 0 > /sys/class/rtc/rtc0/wakealarm; echo +90 > /sys/class/rtc/rtc0/wakealarm; echo rc=$?; cat /sys/class/rtc/rtc0/wakealarm") + "\n")

	// Free the Wi-Fi wakeup source, then suspend. The go binary is a separate
	// process; this whole cycle runs inside it, so it survives the freeze.
	b.WriteString("wifi off: " + shell(ctx, "lipc-set-prop com.lab126.cmd wirelessEnable 0 2>&1; echo done") + "\n")
	time.Sleep(1 * time.Second)

	before := time.Now()
	// This write blocks until the RTC wakes the device (or fails).
	b.WriteString("suspend: " + shell(ctx, "echo mem > /sys/power/state 2>&1; echo rc=$?") + "\n")
	elapsed := time.Since(before)

	b.WriteString("wifi on: " + shell(ctx, "lipc-set-prop com.lab126.cmd wirelessEnable 1 2>&1; echo done") + "\n")
	b.WriteString(fmt.Sprintf("RESULT: suspended+resumed in %s (if ~90s, the RTC wake worked)", elapsed.Round(time.Second)))
	return b.String()
}

// actionInputWakeProbe is read-only: it enumerates every input device, its name,
// and whether it is (or can be) registered as a wakeup source. This tells us if
// a screen tap could ever resume the SoC from suspend, or only the power button
// and RTC can.
func actionInputWakeProbe(ctx context.Context) string {
	var b strings.Builder
	b.WriteString("input devices:\n")
	b.WriteString(shell(ctx, "for d in /sys/class/input/event*; do "+
		"name=$(cat $d/device/name 2>/dev/null); "+
		"echo \"  $(basename $d): $name\"; done") + "\n")

	b.WriteString("wakeup capability (device/power/wakeup):\n")
	b.WriteString(shell(ctx, "for d in /sys/class/input/event*; do "+
		"name=$(cat $d/device/name 2>/dev/null); "+
		"w=$(cat $d/device/power/wakeup 2>/dev/null || echo '<none>'); "+
		"c=$(cat $d/device/power/control 2>/dev/null || echo '<none>'); "+
		"echo \"  $(basename $d) [$name] wakeup=$w control=$c\"; done") + "\n")

	b.WriteString("armed wakeup sources (from /sys/power/wakeup_count + wake_lock):\n")
	b.WriteString(shell(ctx, "echo '  wake_lock:'; sed 's/^/    /' /sys/power/wake_lock 2>/dev/null; "+
		"echo '  touch-like devices:'; "+
		"for d in /sys/class/input/event*; do "+
		"n=$(cat $d/device/name 2>/dev/null); "+
		"case \"$n\" in *ouch*|*yttsp*|*orce*|*lan*|*ynaptics*) echo \"    $d: $n\";; esac; done") + "\n")
	return strings.TrimSpace(b.String())
}

// actionTouchWakeTest is the decisive experiment: register any touch input device
// as a wakeup source, then suspend with a short RTC alarm as a SAFETY NET so the
// device can never stay stuck. If the device resumes before the RTC fires, a
// touch woke it (tap-to-wake is possible). If it resumes at ~the RTC delay, only
// the RTC woke it. Wi-Fi is disabled so the "WLAN timeout" source can't muddy the
// result. Read-only w.r.t. persistent state except the wakeup toggle, which it
// restores afterward.
func actionTouchWakeTest(ctx context.Context) string {
	var b strings.Builder

	// Safety net: arm the RTC first so we always wake even if nothing else does.
	b.WriteString("arm rtc safety: " + shell(ctx, "echo 0 > /sys/class/rtc/rtc0/wakealarm; echo +120 > /sys/class/rtc/rtc0/wakealarm; echo rc=$?; cat /sys/class/rtc/rtc0/wakealarm") + "\n")

	// Find touch input device(s) and enable wakeup on them. Names vary by panel.
	b.WriteString("enable touch wakeup: " + shell(ctx,
		"for d in /sys/class/input/event*; do "+
			"n=$(cat $d/device/name 2>/dev/null); "+
			"case \"$n\" in *ouch*|*yttsp*|*orce*|*lan*|*ynaptics*) "+
			"echo enabled > $d/device/power/wakeup 2>/dev/null; "+
			"echo \"  $d [$n] -> wakeup=$(cat $d/device/power/wakeup 2>/dev/null)\";; esac; done; echo done") + "\n")

	b.WriteString("stop framework: " + shell(ctx, "stop lab126_gui 2>&1; echo done") + "\n")
	b.WriteString("unload screensaver: " + shell(ctx, "lipc-set-prop com.lab126.blanket unload screensaver 2>&1; echo done") + "\n")
	b.WriteString("frontlight off: " + shell(ctx, "lipc-set-prop -i com.lab126.powerd flIntensity 0 2>&1; echo done") + "\n")

	b.WriteString("wifi off: " + shell(ctx, "lipc-set-prop com.lab126.cmd wirelessEnable 0 2>&1; echo done") + "\n")
	time.Sleep(1 * time.Second)

	before := time.Now()
	b.WriteString("suspend: " + shell(ctx, "echo mem > /sys/power/state 2>&1; echo rc=$?") + "\n")
	elapsed := time.Since(before)

	// The decisive signal: when the RTC alarm fires it clears itself. If it is
	// still set after resume, the RTC did NOT wake us -> something else did.
	alarmAfter := strings.TrimSpace(shell(ctx, "cat /sys/class/rtc/rtc0/wakealarm 2>/dev/null"))

	b.WriteString("wifi on: " + shell(ctx, "lipc-set-prop com.lab126.cmd wirelessEnable 1 2>&1; echo done") + "\n")

	// Restore the wakeup source we toggled.
	b.WriteString("restore wakeup: " + shell(ctx,
		"for d in /sys/class/input/event*; do "+
			"n=$(cat $d/device/name 2>/dev/null); "+
			"case \"$n\" in *ouch*|*yttsp*|*orce*|*lan*|*ynaptics*) "+
			"echo disabled > $d/device/power/wakeup 2>/dev/null;; esac; done; echo done") + "\n")

	verdict := "INCONCLUSIVE"
	switch {
	case alarmAfter != "" && alarmAfter != "0":
		verdict = "NON-RTC WAKE (RTC alarm still armed) -- touch woke the SoC; tap-to-wake is possible"
	case elapsed < 110*time.Second:
		verdict = "TOUCH WOKE IT (resumed before the 120s RTC safety) -- tap-to-wake is possible"
	default:
		verdict = "RTC WOKE IT (alarm cleared at ~120s) -- touch cannot wake the SoC; only power/RTC can"
	}
	b.WriteString(fmt.Sprintf("RESULT: resumed after %s; rtc alarm now %q. %s", elapsed.Round(time.Second), alarmAfter, verdict))
	return b.String()
}
