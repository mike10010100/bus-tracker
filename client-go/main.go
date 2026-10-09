package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Version is the authoritative application version. It is overridden at build
// time via -ldflags "-X main.Version=$(cat VERSION)" so that the Go client,
// the Python server, and the Citi Bike User-Agent all share one identity.
var Version = "0.0.0"

const (
	BinaryPath       = "/tmp/tracker"
	ImagePath        = "/tmp/dashboard.png"
	PeakPollInterval = 60 * time.Second
	EcoPollInterval  = 10 * time.Minute
	// Polls of a minute or more are aligned to the wall clock and delayed by
	// this offset so the refresh lands just *after* the on-screen clock ticks
	// over, rather than a hair before it.
	PollSettleOffset = 500 * time.Millisecond
	// ManualHoldDuration bounds the view-override hold and the manual-lighting
	// hold (how long a user's explicit choice survives before auto resumes).
	ManualHoldDuration = 45 * time.Minute
	// FastPollHoldDuration bounds how long a *data-affecting* interaction
	// (view switch / refresh) keeps the radio polling at the fast cadence.
	// Screen-only actions (frontlight, exit) deliberately do not arm it.
	FastPollHoldDuration = 10 * time.Minute
)

type TrackerClient struct {
	serverURL           string
	client              *http.Client
	manualLightTime     time.Time
	manualViewTime      time.Time
	lastDataInteraction time.Time
	mu                  sync.Mutex
	refreshCh           chan struct{}
	lastETag            string
	consecutiveErrors   int
	viewMode            string
	lastRenderedView    string
	panelOnce           sync.Once
	panelSize           PanelSize
	logCh               chan string
	logStarted          sync.Once
}

func NewTrackerClient(server string, initialView string) *TrackerClient {
	if initialView == "" {
		initialView = "auto"
	}
	return &TrackerClient{
		serverURL: server,
		viewMode:  initialView,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
		refreshCh: make(chan struct{}, 1),
		logCh:     make(chan string, 64),
	}
}

func (tc *TrackerClient) getViewMode() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.viewMode == "" || tc.viewMode == "auto" {
		return "auto"
	}
	// Manual view override reverts to auto after 45 minutes of inactivity
	if !tc.manualViewTime.IsZero() && time.Since(tc.manualViewTime) > ManualHoldDuration {
		tc.viewMode = "auto"
		return "auto"
	}
	return tc.viewMode
}

func (tc *TrackerClient) cycleViewMode() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	current := tc.viewMode
	if current == "auto" || current == "" {
		if tc.lastRenderedView != "" {
			current = tc.lastRenderedView
		} else {
			current = "evening"
		}
	}

	// Clean 2-way toggle between Morning (Citi Bike) and Evening (Bus) views
	if current == "morning" {
		tc.viewMode = "evening"
	} else {
		tc.viewMode = "morning"
	}
	tc.manualViewTime = time.Now()
	return tc.viewMode
}

func (tc *TrackerClient) setExplicitViewMode(target string) string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.viewMode = target
	tc.manualViewTime = time.Now()
	return tc.viewMode
}

// dataInteraction records a user action that changes what is fetched/rendered
// (view switch or explicit refresh). It arms the fast-poll hold. Screen-only
// actions (frontlight, exit) deliberately do not call this.
func (tc *TrackerClient) dataInteraction() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.lastDataInteraction = time.Now()
}

func (tc *TrackerClient) getNextPollInterval(serverIntervalSec int) time.Duration {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	// If a data-affecting interaction occurred recently, stay in fast mode.
	if !tc.lastDataInteraction.IsZero() && time.Since(tc.lastDataInteraction) < FastPollHoldDuration {
		return PeakPollInterval
	}

	// Use server guidance if provided
	if serverIntervalSec > 0 {
		return time.Duration(serverIntervalSec) * time.Second
	}

	// Fallback calculation based on local time: 60s during rush, 10m off-peak
	now := time.Now()
	hour := float64(now.Hour()) + float64(now.Minute())/60.0
	if (hour >= 7.5 && hour < 9.5) || (hour >= 16.5 && hour < 19.0) {
		return PeakPollInterval
	}
	return EcoPollInterval
}

// alignDelay returns how long to wait (from `now`) so that the next poll lands
// on the next wall-clock multiple of interval, offset by PollSettleOffset so it
// fires just after the target boundary. Intervals below a minute are returned
// unchanged (drifting polls are fine there).
func alignDelay(now time.Time, interval time.Duration) time.Duration {
	if interval < time.Minute {
		return interval
	}
	untilBoundary := interval - time.Duration(now.UnixNano()%int64(interval))
	return untilBoundary + PollSettleOffset
}

// getPanelSize returns the device framebuffer dimensions, detecting them once.
func (tc *TrackerClient) getPanelSize() PanelSize {
	tc.panelOnce.Do(func() {
		tc.panelSize = DetectPanelSize()
	})
	return tc.panelSize
}

func (tc *TrackerClient) getServerURL() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.serverURL
}

func (tc *TrackerClient) setServerURL(url string) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.serverURL = url
}

// logRemote enqueues a diagnostic log for the single background sender.
// It never blocks the caller and never spawns a goroutine per call, so a burst
// of taps cannot open a burst of radio-waking connections. If the queue is
// full the message is dropped (diagnostics are best-effort).
func (tc *TrackerClient) logRemote(msg string) {
	select {
	case tc.logCh <- msg:
	default:
	}
}

// startLogSender launches the one goroutine that serializes log delivery.
func (tc *TrackerClient) startLogSender(ctx context.Context) {
	tc.logStarted.Do(func() {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case msg := <-tc.logCh:
					tc.postLog(msg)
				}
			}
		}()
	})
}

// postLog performs a single synchronous diagnostic POST.
func (tc *TrackerClient) postLog(msg string) {
	tc.postText("/log", msg)
}

// postDiagnostics gathers a device report synchronously (so the probes run on
// the caller's goroutine and don't leak past the caller's lifetime) and uploads
// it to the server's /diag endpoint asynchronously. When active is true, it also
// runs the heavier on-demand capability probe (identity, crontab, RTC devices,
// boot hook) which is safe but shells out a little.
func (tc *TrackerClient) postDiagnostics(active bool) {
	report := GatherDiagnostics().Format()
	if active {
		report += "\n\n" + RunActiveProbe().Format()
	}
	go tc.postText("/diag", report)
}

func (tc *TrackerClient) postText(path, msg string) {
	server := tc.getServerURL()
	req, err := http.NewRequest("POST", server+path, strings.NewReader(msg))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := tc.client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func (tc *TrackerClient) handleNetworkError(ctx context.Context) {
	tc.mu.Lock()
	tc.consecutiveErrors++
	errCount := tc.consecutiveErrors
	tc.mu.Unlock()

	// If server is unreachable, immediately trigger LAN auto-discovery
	if errCount >= 1 {
		tc.logRemote(fmt.Sprintf("Server unreachable (error %d). Triggering LAN auto-discovery...", errCount))
		if discovered, err := autoDiscover(ctx); err == nil && discovered != "" {
			tc.setServerURL(discovered)
			tc.mu.Lock()
			tc.consecutiveErrors = 0
			tc.mu.Unlock()
			tc.logRemote(fmt.Sprintf("LAN Auto-discovery re-routed server to %s", discovered))
		}
	}
}

// lipcSet executes a lipc-set-prop command, discarding output
func lipcSet(prop, key, val string) {
	cmd := execCommand("lipc-set-prop", "-i", prop, key, val)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
}

// lipcGet reads a property using lipc-get-prop
func lipcGet(prop, key string) string {
	cmd := execCommand("lipc-get-prop", prop, key)
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// cleanup performs full cleanup, resets screensaver, clears screen, and restores Kindle UI
func (tc *TrackerClient) cleanup() {
	tc.logRemote("Cleaning up and exiting to Kindle Home...")

	// Re-enable screensaver
	lipcSet("com.lab126.powerd", "preventScreenSaver", "0")

	// Clear e-ink screen so no stale bus tracker image lingers
	cmd := execCommand("eips", "-c")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()

	// Restore Kindle Library / Home booklet
	lipcSet("com.lab126.appmgrd", "start", "app://com.lab126.booklet.home")
}

// releaseScreenSaver lets the device sleep again (used by oneshot/sleep modes
// so we don't hold the SoC awake between renders).
func (tc *TrackerClient) releaseScreenSaver() {
	lipcSet("com.lab126.powerd", "preventScreenSaver", "0")
}

// armRTCWake programs the hardware RTC to wake the device after `in` seconds.
// It prefers powerd's rtcWakeup property (which cooperates with Amazon's power
// state machine) and falls back to rtcwake against /dev/rtc0. Returns the
// mechanism used, or "" on failure.
func (tc *TrackerClient) armRTCWake(in time.Duration) string {
	secs := int(in.Seconds())
	if secs < 1 {
		secs = 1
	}
	want := strconv.Itoa(secs)

	// Preferred: powerd manages the RTC alarm and the suspend/resume cycle.
	// Verify by reading the property back, since rtcWakeup is readable even
	// when unset.
	lipcSet("com.lab126.powerd", "rtcWakeup", want)
	if got := lipcGet("com.lab126.powerd", "rtcWakeup"); got == want {
		return "powerd.rtcWakeup"
	}

	// Fallback: program the RTC directly, then let powerd suspend.
	cmd := execCommand("rtcwake", "-d", "/dev/rtc0", "-m", "no", "-s", want)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err == nil {
		return "rtcwake"
	}
	return ""
}

// cycleFrontlight advances brightness: Off (0) -> Cozy (8) -> Bright (18) -> Off (0)
func (tc *TrackerClient) cycleFrontlight() {
	tc.mu.Lock()
	tc.manualLightTime = time.Now()
	tc.mu.Unlock()

	currStr := lipcGet("com.lab126.powerd", "flIntensity")
	curr, _ := strconv.Atoi(currStr)

	nextIntensity, nextWarmth := NextFrontlightState(curr)

	lipcSet("com.lab126.powerd", "flIntensity", strconv.Itoa(nextIntensity))
	lipcSet("com.lab126.powerd", "schedAmberLevel", strconv.Itoa(nextWarmth))
	tc.logRemote(fmt.Sprintf("Frontlight cycled: %s -> intensity %d (warmth %d)", currStr, nextIntensity, nextWarmth))
}

// configureGestureHandlers wires every gesture callback onto the detector.
// Extracted from startInputListeners so the callback behavior can be tested
// without opening real input devices.
func (tc *TrackerClient) configureGestureHandlers(gd *GestureDetector, cancel context.CancelFunc) {
	refresh := func() {
		select {
		case tc.refreshCh <- struct{}{}:
		default:
		}
	}

	gd.OnLog = func(msg string) { tc.logRemote(msg) }

	gd.OnSingleTap = func(x, y int32) {
		// Screen-only action: cycleFrontlight() arms the lighting hold itself;
		// do not arm the fast-poll hold (nothing on the wire changed).
		tc.logRemote(fmt.Sprintf("Single tap recognized at (%d, %d)", x, y))
		tc.cycleFrontlight()
	}
	gd.OnDoubleTap = func(x, y int32) {
		tc.logRemote(fmt.Sprintf("Double tap recognized at (%d, %d)! Exiting cleanly...", x, y))
		cancel()
	}
	gd.OnTopRightTap = func(x, y int32) {
		tc.logRemote(fmt.Sprintf("Top-Right corner tapped at (%d, %d)! Exiting...", x, y))
		cancel()
	}
	gd.OnTopLeftTap = func(x, y int32) {
		tc.dataInteraction()
		tc.logRemote(fmt.Sprintf("Top-Left corner tapped at (%d, %d)! Refreshing...", x, y))
		refresh()
	}
	gd.OnBottomLeftTap = func(x, y int32) {
		tc.dataInteraction()
		newMode := tc.cycleViewMode()
		tc.logRemote(fmt.Sprintf("Bottom-Left corner tapped at (%d, %d)! View mode cycled to: %s. Refreshing...", x, y, newMode))
		refresh()
	}
	gd.OnBusesTap = func(x, y int32) {
		tc.dataInteraction()
		newMode := tc.setExplicitViewMode("evening")
		tc.logRemote(fmt.Sprintf("BUSES button tapped at (%d, %d)! View set to: %s. Refreshing...", x, y, newMode))
		refresh()
	}
	gd.OnBikesTap = func(x, y int32) {
		tc.dataInteraction()
		newMode := tc.setExplicitViewMode("morning")
		tc.logRemote(fmt.Sprintf("CITI BIKE button tapped at (%d, %d)! View set to: %s. Refreshing...", x, y, newMode))
		refresh()
	}
	gd.OnLightTap = func(x, y int32) {
		// Screen-only action: no fast-poll hold (see OnSingleTap).
		tc.logRemote(fmt.Sprintf("LIGHT button tapped at (%d, %d)! Cycling frontlight...", x, y))
		tc.cycleFrontlight()
	}
	gd.OnRefreshTap = func(x, y int32) {
		tc.dataInteraction()
		tc.logRemote(fmt.Sprintf("REFRESH button tapped at (%d, %d)! Refreshing...", x, y))
		refresh()
	}
	gd.OnExitTap = func(x, y int32) {
		tc.logRemote(fmt.Sprintf("EXIT button tapped at (%d, %d)! Exiting cleanly...", x, y))
		cancel()
	}
}

// runEventLoop dispatches multiplexed input events to the gesture detector
// until the context is cancelled.
func (tc *TrackerClient) runEventLoop(ctx context.Context, cancel context.CancelFunc, eventCh <-chan RawEventMsg) {
	gd := NewGestureDetector(DefaultGestureConfig())
	defer gd.Stop()
	tc.configureGestureHandlers(gd, cancel)

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-eventCh:
			// 1. Hardware Power Button
			if IsPowerKeyEvent(ev) {
				tc.logRemote(fmt.Sprintf("Power button pressed on %s! Exiting...", ev.Device))
				cancel()
				return
			}
			// 2. Feed into Gesture Recognizer
			gd.ProcessEvent(ev)
		}
	}
}

// startInputListeners opens ALL /dev/input/event* devices and multiplexes events into eventCh
func (tc *TrackerClient) startInputListeners(ctx context.Context, cancel context.CancelFunc) {
	matches, err := globInputs("/dev/input/event*")
	if err != nil || len(matches) == 0 {
		matches = []string{"/dev/input/event0", "/dev/input/event1", "/dev/input/event2"}
	}

	tc.logRemote(fmt.Sprintf("Found input devices: %v", matches))

	eventCh := make(chan RawEventMsg, 128)

	for _, devPath := range matches {
		f, err := osOpen(devPath)
		if err != nil {
			continue
		}
		tc.logRemote(fmt.Sprintf("Opened input device listener on %s", devPath))

		go func(path string, file *os.File) {
			defer file.Close()
			buf := make([]byte, 512)

			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				n, err := file.Read(buf)
				if err != nil {
					time.Sleep(100 * time.Millisecond)
					continue
				}

				events := ParseInputEvents(buf, n, path)
				for _, ev := range events {
					select {
					case eventCh <- ev:
					default:
					}
				}
			}
		}(devPath, f)
	}

	// Dispatcher goroutine: processes all events from all devices
	go tc.runEventLoop(ctx, cancel, eventCh)
}

// startPowerListener watches for power button sleep events via lipc
func (tc *TrackerClient) startPowerListener(ctx context.Context, exitCancel context.CancelFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		cmd := execCommandContext(ctx, "lipc-wait-event", "com.lab126.powerd", "goingToScreenSaver")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err == nil {
			tc.logRemote("powerd goingToScreenSaver event received! Exiting...")
			exitCancel()
			return
		}
	}
}

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
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		tc.handleNetworkError(ctx)
		return 0
	}
	req.Header.Set("X-Kindle-Battery", strconv.Itoa(batt.Level))
	req.Header.Set("X-Kindle-Charging", strconv.Itoa(chargeVal))
	req.Header.Set("X-Tracker-View", viewMode)

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

	// The server can ask (one-shot) for a device diagnostics dump via a header.
	// "full" additionally runs the active capability probe.
	if diag := resp.Header.Get("X-Tracker-Diag"); diag != "" {
		tc.postDiagnostics(diag == "full")
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

	// Write image to /tmp/dashboard.png
	tmpFile, err := osCreate(ImagePath)
	if err != nil {
		return serverPollSec
	}
	_, err = io.Copy(tmpFile, resp.Body)
	tmpFile.Close()
	if err != nil {
		return serverPollSec
	}

	// Push directly to Kindle e-ink display
	cmd := execCommand("eips", "-f", "-g", ImagePath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()

	// Apply lighting headers if manual override is inactive
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

	// New version: download and exec it (no-op on the common path).
	if tc.maybeUpdateBinary(ctx, serverVer, serverSHA) {
		return 0
	}
	return serverPollSec
}

func main() {
	// Silence standard error on headless Kindle
	if nullFile, err := osOpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		_ = syscall.Dup2(int(nullFile.Fd()), int(os.Stderr.Fd()))
	}
	run(context.Background())
}

// run dispatches on the requested run mode. The default (resident) mode blocks
// in the poll loop; oneshot renders once and exits; sleep renders, arms an RTC
// wake, and cycles through device suspend.
func run(parent context.Context) {
	mode := ResolveRunMode(os.Args)

	serverURL := GetServerURL()
	initialView := ResolveViewMode(os.Args)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	tc := NewTrackerClient(serverURL, initialView)
	tc.startLogSender(ctx)
	_ = osWriteFile("/tmp/tracker_server.txt", []byte(serverURL), 0644)
	_ = osWriteFile("/mnt/us/documents/tracker_server.txt", []byte(serverURL), 0644)

	// Send initial startup diagnostic
	tc.logRemote(fmt.Sprintf("Transit Tracker v%s starting up (mode: %s, server: %s, view: %s)...", Version, mode, serverURL, initialView))
	if devData, err := osReadFile("/proc/bus/input/devices"); err == nil {
		tc.logRemote(fmt.Sprintf("Input devices:\n%s", string(devData)))
	}

	// Upload a device/jailbreak capability report on every startup (passive
	// only; the active probe runs on request) so the server can track the fleet.
	tc.postDiagnostics(false)

	if mode == ModeOneshot {
		// Render exactly once and exit, leaving the image on screen. Deliberately
		// do NOT set preventScreenSaver or run cleanup (which clears the screen).
		tc.logRemote("Oneshot mode: rendering once and exiting.")
		tc.fetchAndDrawDashboard(ctx, cancel)
		return
	}

	// Ensure Kindle stays awake while the dashboard is running
	lipcSet("com.lab126.powerd", "preventScreenSaver", "1")

	// Intercept OS termination signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigCh
		tc.logRemote("OS signal received. Exiting...")
		cancel()
	}()

	// Start background listeners
	tc.startInputListeners(ctx, cancel)
	go tc.startPowerListener(ctx, cancel)

	if mode == ModeSleep {
		tc.runSleepLoop(ctx, cancel)
		return
	}

	// Resident mode: initial fetch, then the poll loop.
	initialPollSec := tc.fetchAndDrawDashboard(ctx, cancel)
	tc.runPollLoop(ctx, cancel, tc.getNextPollInterval(initialPollSec))
}

// runSleepLoop is the low-power mode: render the dashboard, release the
// screensaver, arm the RTC to wake for the next interval, and block until the
// device resumes (or the wait times out, in which case it simply re-renders).
// This trades the adaptive per-poll cadence for real suspend between updates.
func (tc *TrackerClient) runSleepLoop(ctx context.Context, cancel context.CancelFunc) {
	for {
		select {
		case <-ctx.Done():
			tc.cleanup()
			return
		default:
		}

		serverPollSec := tc.fetchAndDrawDashboard(ctx, cancel)
		wait := alignDelay(time.Now(), tc.getNextPollInterval(serverPollSec))

		tc.releaseScreenSaver()
		mech := tc.armRTCWake(wait)
		if mech == "" {
			tc.logRemote("Sleep mode: could not arm RTC; relying on wall-clock wait.")
		} else {
			tc.logRemote(fmt.Sprintf("Sleep mode: armed wake in %s via %s", wait.Round(time.Second), mech))
		}

		select {
		case <-ctx.Done():
			tc.cleanup()
			return
		case <-tc.waitForResume(ctx, wait):
		}
	}
}

// waitForResume blocks until the device resumes from suspend (via
// lipc-wait-event) or `fallback` elapses, whichever is first. It ensures we
// cannot wedge forever if the RTC wake is never delivered.
func (tc *TrackerClient) waitForResume(ctx context.Context, fallback time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		waitCtx, waitCancel := context.WithTimeout(ctx, fallback+30*time.Second)
		defer waitCancel()
		cmd := execCommandContext(waitCtx, "lipc-wait-event", "-m", "com.lab126.powerd", "resuming")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		_ = cmd.Run()
	}()
	return done
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
			// Forced refresh requested via screen tap.
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
