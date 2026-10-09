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
type DeviceAction func(ctx context.Context) string

// deviceActions maps action names to their implementations.
var deviceActions = map[string]DeviceAction{
	"disable-ads":     actionDisableAds,
	"stop-framework":  actionStopFramework,
	"start-framework": actionStartFramework,
	"framework-state": actionFrameworkState,
	"sleep-test":      actionSleepTest,
}

// actionTimeout bounds a whole action.
var actionTimeout = 60 * time.Second

// runAction executes a named action and returns a human-readable result.
func runAction(ctx context.Context, name string) (string, bool) {
	fn, ok := deviceActions[name]
	if !ok {
		return fmt.Sprintf("unknown action %q", name), false
	}
	actx, cancel := context.WithTimeout(ctx, actionTimeout)
	defer cancel()
	return fn(actx), true
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
	// Flip the ad visibility flag in appreg.db. Prefer sqlite3; fall back to a
	// byte-level replace if sqlite3 is absent.
	sql := `sqlite3 /var/local/appreg.db "UPDATE properties SET value='false' WHERE name='adunit.viewable';" 2>&1`
	b.WriteString("sqlite3: " + shell(ctx, sql) + "\n")
	// Also blank the ad unit dir and the .assets store marker.
	b.WriteString("rm adunits: " + shell(ctx, "rm -rf /var/local/adunits /mnt/us/.assets 2>&1; echo done") + "\n")
	b.WriteString("verify: " + shell(ctx, "ls -la /var/local/adunits 2>&1; ls -la /mnt/us/.assets 2>&1") + "\n")
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
