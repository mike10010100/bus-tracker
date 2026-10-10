package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// maybeUpdateBinary downloads and installs a new binary if the version
// advertised by the server (learned from the dashboard response headers)
// differs from the one currently running. Returns true only when it hands off
// control to the freshly-exec'd binary. This is normally a no-op and performs
// no network I/O.
func (tc *TrackerClient) maybeUpdateBinary(ctx context.Context, serverVer, serverSHA string) bool {
	if serverVer == "" {
		return false
	}
	should, reason := ShouldUpdate(serverVer, Version, "", "")
	if !should {
		return false
	}
	tc.logRemote(fmt.Sprintf("OTA update triggered: %s. Hot-reloading...", reason))

	server := tc.getServerURL()
	updatePath := "/tmp/tracker.update"
	getReq, err := http.NewRequestWithContext(ctx, "GET", server+"/tracker-arm", nil)
	if err != nil {
		return false
	}
	getResp, err := tc.client.Do(getReq)
	if err != nil || getResp.StatusCode != http.StatusOK {
		return false
	}
	defer getResp.Body.Close()

	out, err := osOpenFile(updatePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return false
	}
	hasher := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, hasher), getResp.Body)
	out.Close()
	if err != nil {
		_ = osRemove(updatePath)
		return false
	}

	// Prefer the digest advertised on the dashboard response; fall back to the
	// one carried by the download response itself.
	expected := serverSHA
	if expected == "" {
		expected = getResp.Header.Get("X-Tracker-SHA256")
	}
	actual := hex.EncodeToString(hasher.Sum(nil))
	if !VerifySHA256(actual, expected) {
		if expected == "" {
			tc.logRemote("OTA response carried no X-Tracker-SHA256; skipping update for safety.")
		} else {
			tc.logRemote(fmt.Sprintf("OTA checksum mismatch (expected %s, got %s). Discarding update.", expected, actual))
		}
		_ = osRemove(updatePath)
		return false
	}

	_ = osRename(updatePath, BinaryPath)
	_ = osChmod(BinaryPath, 0755)
	_ = SaveServerURL(server)

	newArgs := []string{BinaryPath, "-server", server, "-view", tc.getViewMode()}
	tc.logRemote("Executing updated binary via syscall.Exec...")
	_ = sysExec(BinaryPath, newArgs, os.Environ())
	return true
}

// fetchAndDrawDashboard fetches dashboard PNG, applies lighting, and pushes to e-ink.
// Returns the target poll interval in seconds reported by the server header (or 0 if unavailable).
func (tc *TrackerClient) fetchAndDrawDashboard(ctx context.Context, exitCancel context.CancelFunc) int {
	batt := GetBatteryInfo()
	chargeVal := 0
	if batt.IsCharging {
		chargeVal = 1
	}

	viewMode := tc.getViewMode()
	server := tc.getServerURL()
	panel := tc.getPanelSize()
	url := fmt.Sprintf("%s/dashboard.png?kindle=pw5&w=%d&h=%d&batt=%d&charging=%d&view=%s&t=%d",
		server, panel.LandscapeW, panel.LandscapeH, batt.Level, chargeVal, viewMode, time.Now().Unix())
	// While in an awake interaction session, ask for the full tappable dashboard
	// regardless of the time-based presentation.
	if tc.isInteracting() {
		url += "&present=interactive"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		tc.handleNetworkError(ctx)
		return 0
	}
	req.Header.Set("X-Kindle-Battery", strconv.Itoa(batt.Level))
	req.Header.Set("X-Kindle-Charging", strconv.Itoa(chargeVal))
	req.Header.Set("X-Tracker-View", viewMode)
	// Report the mode we're actually running, so the server can confirm a mode
	// switch landed and keep requesting it until it does.
	req.Header.Set("X-Tracker-Mode", currentModeName())

	tc.mu.Lock()
	etag := tc.lastETag
	tc.mu.Unlock()
	if etag != "" {
		// Ask the server to answer 304 if the dashboard is unchanged, so we can
		// skip both the image transfer and the eips refresh.
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := tc.client.Do(req)
	if err != nil {
		tc.handleNetworkError(ctx)
		return 0
	}
	defer resp.Body.Close()

	if canonical := resp.Header.Get("X-Tracker-Server"); canonical != "" && canonical != server && AdoptableServerURL(canonical) {
		tc.setServerURL(canonical)
		_ = SaveServerURL(canonical)
	}

	if resView := resp.Header.Get("X-Resolved-View"); resView != "" {
		tc.mu.Lock()
		tc.lastRenderedView = resView
		tc.mu.Unlock()
	}

	// The dashboard response carries the server's binary version/digest, so the
	// OTA decision needs no separate request. A mismatch hands off control.
	serverVer := resp.Header.Get("X-Tracker-Version")
	serverSHA := resp.Header.Get("X-Tracker-SHA256")

	// The server tells us whether to present a live (tappable) dashboard or an
	// inert dormant face (overnight). This gates whether touch wakes the SoC.
	tc.setPresentation(resp.Header.Get("X-Tracker-Presentation"))

	// The server can ask (one-shot) for a device diagnostics dump via a header.
	// "full" additionally runs the active capability probe.
	if diag := resp.Header.Get("X-Tracker-Diag"); diag != "" {
		tc.postDiagnostics(diag == "full")
	}

	// The server can request a named, allowlisted device action (maintenance).
	if action := resp.Header.Get("X-Tracker-Action"); action != "" {
		result, known := runAction(ctx, action)
		// Some actions suspend the SoC and toggle Wi-Fi, so the link may still
		// be re-associating when we try to report the result. Wait for the
		// network first, or the (async) result POST races and is dropped.
		tc.waitForNetwork(ctx)
		tc.logRemote(fmt.Sprintf("Device action %q ->\n%s", action, result))
		if !known {
			tc.logRemote(fmt.Sprintf("Unknown device action %q ignored.", action))
		}
	}

	// The server can ask the client to relaunch in a different run mode via a
	// header (resident/oneshot/sleep/sleep-suspend). We only re-exec when the
	// requested mode differs from the one we're running. sysExec only returns on
	// failure; if it does, log loudly and fall through (the server keeps
	// re-requesting until our next poll reports the new mode, so a transient
	// failure self-heals).
	if want := resp.Header.Get("X-Tracker-Mode"); want != "" && want != currentModeName() {
		if flags := modeFlags(want); flags != nil {
			tc.logRemote(fmt.Sprintf("Server requested run mode %q; relaunching.", want))
			newArgs := append([]string{BinaryPath, "-server", server, "-view", tc.getViewMode()}, flags...)
			// sysExec replaces this process on success and never returns; if it
			// returns, the exec failed, so log and fall through (the server keeps
			// re-requesting until a later poll reports the new mode).
			if err := sysExec(BinaryPath, newArgs, os.Environ()); err != nil {
				tc.logRemote(fmt.Sprintf("Mode relaunch exec FAILED (%v); staying in %q.", err, currentModeName()))
			}
		}
	}

	// HTTP 205 signals remote stop command
	if resp.StatusCode == 205 {
		tc.logRemote("Server sent HTTP 205 Stop signal. Exiting cleanly...")
		exitCancel()
		return 0
	}

	// HTTP 304: dashboard unchanged. Skip the transfer and the screen refresh.
	if resp.StatusCode == http.StatusNotModified {
		tc.mu.Lock()
		tc.consecutiveErrors = 0
		tc.mu.Unlock()
		var pollSec int
		if pStr := resp.Header.Get("X-Kindle-Poll-Interval"); pStr != "" {
			pollSec, _ = strconv.Atoi(pStr)
		}
		if tc.maybeUpdateBinary(ctx, serverVer, serverSHA) {
			return 0 // exec'd into the new binary
		}
		return pollSec
	}

	if resp.StatusCode != http.StatusOK {
		tc.handleNetworkError(ctx)
		return 0
	}

	var serverPollSec int
	if pStr := resp.Header.Get("X-Kindle-Poll-Interval"); pStr != "" {
		serverPollSec, _ = strconv.Atoi(pStr)
	}

	tc.mu.Lock()
	tc.consecutiveErrors = 0
	if newTag := resp.Header.Get("ETag"); newTag != "" {
		tc.lastETag = newTag
	}
	tc.mu.Unlock()

	tmpFile, err := osCreate(ImagePath)
	if err != nil {
		return serverPollSec
	}
	_, err = io.Copy(tmpFile, resp.Body)
	tmpFile.Close()
	if err != nil {
		return serverPollSec
	}

	cmd := execCommand("eips", "-f", "-g", ImagePath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()

	tc.mu.Lock()
	manualActive := time.Since(tc.manualLightTime) < ManualHoldDuration
	tc.mu.Unlock()

	if !manualActive {
		brightStr := resp.Header.Get("X-Kindle-Brightness")
		warmStr := resp.Header.Get("X-Kindle-Warmth")
		currB := lipcGet("com.lab126.powerd", "flIntensity")
		currW := lipcGet("com.lab126.powerd", "schedAmberLevel")

		if brightStr != "" && brightStr != currB {
			lipcSet("com.lab126.powerd", "flIntensity", brightStr)
			tc.logRemote(fmt.Sprintf("Commute auto-lighting applied: brightness %s -> %s", currB, brightStr))
		}
		if warmStr != "" && warmStr != currW {
			lipcSet("com.lab126.powerd", "schedAmberLevel", warmStr)
			tc.logRemote(fmt.Sprintf("Commute auto-lighting applied: warmth %s -> %s", currW, warmStr))
		}
	}

	if tc.maybeUpdateBinary(ctx, serverVer, serverSHA) {
		return 0
	}
	return serverPollSec
}

// runPollLoop drives the fetch/OTA cycle until ctx is cancelled. `interval` is
// the initial wait; subsequent waits are re-derived from the server's reported
// poll interval and aligned to the wall clock. The initial interval is injected
// so tests can run the loop at millisecond cadence.
func (tc *TrackerClient) runPollLoop(ctx context.Context, cancel context.CancelFunc, interval time.Duration) {
	timer := time.NewTimer(alignDelay(time.Now(), interval))
	defer timer.Stop()

	reschedule := func(serverPollSec int) {
		timer.Reset(alignDelay(time.Now(), tc.getNextPollInterval(serverPollSec)))
	}

	for {
		select {
		case <-ctx.Done():
			tc.cleanup()
			return

		case <-tc.refreshCh:
			serverPollSec := tc.fetchAndDrawDashboard(ctx, cancel)
			reschedule(serverPollSec)

		case <-timer.C:
			// Fetch (and, if the server advertises a new build, OTA-update).
			// There is no separate per-cycle OTA check: the dashboard response
			// carries the server version, so a normal tick is one request.
			serverPollSec := tc.fetchAndDrawDashboard(ctx, cancel)
			reschedule(serverPollSec)
		}
	}
}
