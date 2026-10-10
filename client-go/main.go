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
	// touchCh fires on ANY recognised touch, so an interaction session stays
	// alive while the user is poking at the screen (a frontlight tap must reset
	// the idle timer too, not just data taps).
	touchCh           chan struct{}
	lastETag          string
	consecutiveErrors int
	viewMode          string
	lastRenderedView  string
	panelOnce         sync.Once
	panelSize         PanelSize
	logCh             chan string
	logStarted        sync.Once
	// exitOnPowerKey, when true (resident mode), treats a hardware power-key
	// press as a request to exit. In low-power dashboard mode it is false: the
	// power key is a wake source, not an exit, so a press must not kill us.
	exitOnPowerKey bool
	// presentation is the server-advised visual/interaction state: "interactive"
	// (tappable dashboard, awake), "idle" (suspended; press power to interact)
	// or "dormant" (overnight). Logged for observability.
	presentation string
	// interacting is true while the client is in an awake power-button session,
	// during which it requests the full tappable dashboard from the server.
	interacting bool
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
		refreshCh:      make(chan struct{}, 1),
		touchCh:        make(chan struct{}, 1),
		logCh:          make(chan string, 64),
		exitOnPowerKey: true,
		presentation:   "interactive",
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

// setInteractionLighting lights the panel for an interaction session and, while
// on, suppresses the schedule auto-lighting. On session end (on=false) the
// manual hold is cleared so the schedule lighting resumes (e.g. off overnight).
func (tc *TrackerClient) setInteractionLighting(on bool) {
	tc.mu.Lock()
	if on {
		tc.manualLightTime = time.Now()
	} else {
		tc.manualLightTime = time.Time{}
	}
	tc.mu.Unlock()
	if !on {
		return
	}
	curr := lipcGet("com.lab126.powerd", "flIntensity")
	if curr == "" || curr == "0" {
		lipcSet("com.lab126.powerd", "flIntensity", "8")
		lipcSet("com.lab126.powerd", "schedAmberLevel", "12")
		tc.logRemote(fmt.Sprintf("Interaction lighting on (was %q).", curr))
	}
}

// noteTouch signals that the user touched the screen. Used to keep an awake
// interaction session alive while the user is interacting, even for a tap that
// only changes (say) the frontlight. Non-blocking.
func (tc *TrackerClient) noteTouch() {
	select {
	case tc.touchCh <- struct{}{}:
	default:
	}
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

// rawTouchToDesign maps a raw touch coordinate (in the panel's portrait
// framebuffer space) into the renderer's design space (800px wide). The server
// draws the landscape design at the panel's native landscape resolution and
// then rotates it 90 degrees counter-clockwise into the portrait framebuffer,
// so we undo that rotation here:
//
//	dx = (Wl - 1 - py) * DesignWidth / Wl
//	dy = px * DesignWidth / Wl
//
// where (px,py) is the raw portrait coordinate and Wl is the landscape width
// (equal to the portrait height). Verified against the rendered button bar,
// which lands at portrait x ~= 1145..1215 (design y 556..590).
func (tc *TrackerClient) rawTouchToDesign(px, py int32) (int32, int32) {
	p := tc.getPanelSize()
	wl := float64(p.LandscapeW)
	if wl <= 0 {
		return px, py
	}
	scale := wl / float64(DesignWidth)
	dx := (wl - 1 - float64(py)) / scale
	dy := float64(px) / scale
	return int32(dx + 0.5), int32(dy + 0.5)
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

// lipcCallTimeout bounds how long a caller waits on a lipc property call. The
// gesture handlers run on the input dispatcher goroutine, so a slow or
// unresponsive daemon (notably powerd right after a power-button wake) must
// never be able to block input forever. If a call exceeds the timeout the
// caller moves on (the subprocess is abandoned).
var lipcCallTimeout = 2 * time.Second

// lipcSet executes a lipc-set-prop command, discarding output.
func lipcSet(prop, key, val string) {
	fn := execCommand // capture now; the goroutine must not touch the seam var
	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd := fn("lipc-set-prop", "-i", prop, key, val)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		_ = cmd.Run()
	}()
	select {
	case <-done:
	case <-time.After(lipcCallTimeout):
	}
}

// lipcGet reads a property using lipc-get-prop, bounded by lipcCallTimeout.
func lipcGet(prop, key string) string {
	fn := execCommand // capture now; the goroutine must not touch the seam var
	ch := make(chan string, 1)
	go func() {
		cmd := fn("lipc-get-prop", prop, key)
		cmd.Stderr = io.Discard
		out, err := cmd.Output()
		if err != nil {
			ch <- ""
			return
		}
		ch <- strings.TrimSpace(string(out))
	}()
	select {
	case res := <-ch:
		return res
	case <-time.After(lipcCallTimeout):
		return ""
	}
}

// cleanup performs full cleanup, resets screensaver, clears screen, and restores Kindle UI
func (tc *TrackerClient) cleanup() {
	tc.logRemote("Cleaning up and exiting to Kindle Home...")

	// Re-enable screensaver
	lipcSet("com.lab126.powerd", "preventScreenSaver", "0")

	// Low-power mode stops the Amazon UI framework (lab126_gui) to allow
	// suspend. If we exited without restarting it, the panel would be stuck on
	// our image with no UI behind it -- a blank, unresponsive screen. So always
	// best-effort restart the framework before restoring Home.
	startCmd := execCommand("start", "lab126_gui")
	startCmd.Stdout = io.Discard
	startCmd.Stderr = io.Discard
	_ = startCmd.Run()

	// Deliberately do NOT `eips -c` here: clearing the panel while the framework
	// is still starting would leave it blank. "start Home" repaints it anyway.
	lipcSet("com.lab126.appmgrd", "start", "app://com.lab126.booklet.home")
}

// releaseScreenSaver lets the device sleep again (used by oneshot/sleep modes
// so we don't hold the SoC awake between renders).
func (tc *TrackerClient) releaseScreenSaver() {
	lipcSet("com.lab126.powerd", "preventScreenSaver", "0")
}

// suspendSettleDelay is how long to let powerd settle after releasing the
// screensaver, before attempting to suspend. Variable for tests.
var suspendSettleDelay = 2 * time.Second

// suspendDeadlineMargin bounds how long past the poll interval we wait for a
// powerd suspend/resume event before falling back to a wall-clock refresh.
// Variable for tests.
var suspendDeadlineMargin = 30 * time.Second

// Sysfs wakealarm and power-state nodes. On the PW5 (BD71828 RTC) the kernel
// wakealarm is the reliable wake source; powerd's rtcWakeup is only writable
// inside its readyToSuspend window and adds a ~60s delay, so we drive the
// kernel directly.
var (
	sysfsWakeAlarmPath = "/sys/class/rtc/rtc0/wakealarm"
	powerStatePath     = "/sys/power/state"
)

// setWireless toggles the Wi-Fi radio. Disabling it is REQUIRED before a direct
// suspend: an associated interface holds the "WLAN timeout" wakeup source, which
// makes /sys/power/state return EBUSY.
func (tc *TrackerClient) setWireless(on bool) {
	val := "0"
	if on {
		val = "1"
	}
	lipcSet("com.lab126.cmd", "wirelessEnable", val)
}

// armSysfsWake programs the hardware RTC to fire in `in` seconds. The alarm must
// be cleared (0) before setting +N on this kernel.
func (tc *TrackerClient) armSysfsWake(in time.Duration) bool {
	secs := int(in.Seconds())
	if secs < 1 {
		secs = 1
	}
	_ = osWriteFile(sysfsWakeAlarmPath, []byte("0"), 0644)
	if err := osWriteFile(sysfsWakeAlarmPath, []byte("+"+strconv.Itoa(secs)), 0644); err != nil {
		return false
	}
	return true
}

// enterSuspend writes "mem" to /sys/power/state. It blocks until the device
// resumes (RTC alarm or user input), returning the elapsed time. On failure it
// returns the error.
func (tc *TrackerClient) enterSuspend() (time.Duration, error) {
	start := time.Now()
	err := osWriteFile(powerStatePath, []byte("mem"), 0644)
	return time.Since(start), err
}

// rtcAlarmStillArmed reports whether the RTC wake alarm is still programmed after
// a resume. A fired alarm clears itself, so "still armed" means the RTC did NOT
// wake us -- a person did (touch/power). This is the decisive wake-reason signal.
func (tc *TrackerClient) rtcAlarmStillArmed() bool {
	b, err := osReadFile(sysfsWakeAlarmPath)
	if err != nil {
		return false
	}
	v := strings.TrimSpace(string(b))
	return v != "" && v != "0"
}

// disarmRTC clears the wake alarm (used after a non-RTC wake so a stale alarm
// can't fire a spurious resume mid-interaction).
func (tc *TrackerClient) disarmRTC() {
	_ = osWriteFile(sysfsWakeAlarmPath, []byte("0"), 0644)
}

// setPresentation records the server-advised visual/interaction state.
func (tc *TrackerClient) setPresentation(p string) {
	if p == "" {
		return
	}
	tc.mu.Lock()
	tc.presentation = p
	tc.mu.Unlock()
}

// getPresentation returns the current presentation ("interactive" by default).
func (tc *TrackerClient) getPresentation() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.presentation == "" {
		return "interactive"
	}
	return tc.presentation
}

// setInteracting marks whether we are in an awake power-button session, during
// which the dashboard is requested in its full tappable form.
func (tc *TrackerClient) setInteracting(v bool) {
	tc.mu.Lock()
	tc.interacting = v
	tc.mu.Unlock()
}

func (tc *TrackerClient) isInteracting() bool {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.interacting
}

// interactionHoldDuration is how long a touch wake keeps the device awake with
// no further taps before it re-suspends. Each tap resets it. Variable for tests.
var interactionHoldDuration = 90 * time.Second

// interactionAwake keeps the client awake for `d` after a touch wake, servicing
// forced-refresh taps so the user can browse. Each refresh resets the timer, so
// the device stays awake as long as the user keeps interacting and sleeps `d`
// after the last tap. Returns false if ctx ended.
func (tc *TrackerClient) interactionAwake(ctx context.Context, cancel context.CancelFunc, d time.Duration) bool {
	// Hold the screensaver open so powerd doesn't auto-sleep mid-browse.
	lipcSet("com.lab126.powerd", "preventScreenSaver", "1")
	// Light the panel for the session (off-peak the auto-lighting leaves it
	// dark, so a night-time interaction would be unreadable).
	tc.setInteractionLighting(true)
	defer tc.setInteractionLighting(false)

	// Render the full tappable dashboard: the user just pressed power to engage
	// and the suspended face was the inert strip. Wi-Fi is still re-associating
	// right after the wake, so wait for the link and retry a few times, or the
	// interactive render would silently fail and the panel would stay idle.
	rendered := false
	for i := 0; i < 5 && !rendered; i++ {
		tc.waitForNetwork(ctx)
		if tc.fetchAndDrawDashboard(ctx, cancel) > 0 {
			rendered = true
			break
		}
		if !tc.sleepWallClock(ctx, 2*time.Second) {
			return false
		}
	}
	tc.logRemote(fmt.Sprintf("Interactive session: dashboard rendered=%v.", rendered))

	timer := time.NewTimer(d)
	defer timer.Stop()
	reset := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-tc.touchCh:
			// Any touch keeps the session alive (even a frontlight-only tap).
			reset()
		case <-tc.refreshCh:
			tc.fetchAndDrawDashboard(ctx, cancel)
			reset()
		}
	}
}

// prepareDisplayForSleep stops the Amazon UI framework and unloads the blanket
// screensaver so nothing repaints over our dashboard and the device can suspend.
// Idempotent. Only appropriate for a dedicated dashboard.
func (tc *TrackerClient) prepareDisplayForSleep(ctx context.Context) {
	runQuiet(ctx, "stop", "lab126_gui")
	runQuiet(ctx, "lipc-set-prop", "com.lab126.blanket", "unload", "screensaver")
	runQuiet(ctx, "lipc-set-prop", "com.lab126.blanket", "unload", "splash")
}

// runQuiet runs a command, discarding output.
func runQuiet(ctx context.Context, name string, args ...string) {
	cmd := execCommandContext(ctx, name, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
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
		tc.noteTouch()
		select {
		case tc.refreshCh <- struct{}{}:
		default:
		}
	}

	gd.OnLog = func(msg string) { tc.logRemote(msg) }

	gd.OnSingleTap = func(x, y int32) {
		// Taps in the main content area are deliberately inert: the frontlight
		// has its own LIGHT button, and cycling it on every tap was confusing
		// ("only brightness changed"). Still acknowledge the touch so an awake
		// interaction session doesn't expire while the user is poking around.
		tc.noteTouch()
	}
	gd.OnDoubleTap = func(x, y int32) {
		// A double tap is trivially easy to trigger accidentally; do NOT exit.
		// It forces a refresh, the least-surprising safe action.
		tc.dataInteraction()
		tc.logRemote(fmt.Sprintf("Double tap at (%d, %d)! Refreshing...", x, y))
		refresh()
	}
	gd.OnTopRightTap = func(x, y int32) {
		// Like the double tap, this corner must not exit accidentally.
		tc.dataInteraction()
		tc.logRemote(fmt.Sprintf("Top-Right corner tapped at (%d, %d)! Refreshing...", x, y))
		refresh()
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
		// Screen-only action: no fast-poll hold (see OnSingleTap). The lipc calls
		// inside are individually bounded by lipcCallTimeout, so a slow powerd
		// can stall the dispatcher at most briefly.
		tc.noteTouch()
		tc.logRemote(fmt.Sprintf("LIGHT button tapped at (%d, %d)! Cycling frontlight...", x, y))
		tc.cycleFrontlight()
	}
	gd.OnRefreshTap = func(x, y int32) {
		tc.dataInteraction()
		tc.logRemote(fmt.Sprintf("REFRESH button tapped at (%d, %d)! Refreshing...", x, y))
		refresh()
	}
}

// runEventLoop dispatches multiplexed input events to the gesture detector
// until the context is cancelled.
func (tc *TrackerClient) runEventLoop(ctx context.Context, cancel context.CancelFunc, eventCh <-chan RawEventMsg) {
	cfg := DefaultGestureConfig()
	cfg.Transform = tc.rawTouchToDesign
	gd := NewGestureDetector(cfg)
	defer gd.Stop()
	tc.configureGestureHandlers(gd, cancel)

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-eventCh:
			// 1. Hardware Power Button
			if IsPowerKeyEvent(ev) {
				if tc.exitOnPowerKey {
					tc.logRemote(fmt.Sprintf("Power button pressed on %s! Exiting...", ev.Device))
					cancel()
					return
				}
				// Low-power dashboard mode: the power key is a wake source, not
				// an exit. Ignore it so pressing power to wake doesn't kill us.
				tc.logRemote("Power key ignored (dedicated dashboard mode).")
				continue
			}
			// 2. Feed into Gesture Recognizer
			gd.ProcessEvent(ev)
		}
	}
}

// startInputListeners opens ALL /dev/input/event* devices and multiplexes events into eventCh
func (tc *TrackerClient) startInputListeners(ctx context.Context, cancel context.CancelFunc) {
	// Prime the panel size once here, on the caller's goroutine, so the event
	// goroutine's coordinate transform reads a cached value and never touches
	// the filesystem (also avoids a data race with test seam restoration).
	_ = tc.getPanelSize()

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
	currentRunMode = mode

	serverURL := GetServerURL()
	initialView := ResolveViewMode(os.Args)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	tc := NewTrackerClient(serverURL, initialView)
	tc.startLogSender(ctx)
	_ = osWriteFile("/tmp/tracker_server.txt", []byte(serverURL), 0644)
	_ = osWriteFile("/mnt/us/documents/tracker_server.txt", []byte(serverURL), 0644)

	// Send initial startup diagnostic
	tc.logRemote(fmt.Sprintf("Transit Tracker v%s starting up (mode: %s, server: %s, view: %s)...", Version, currentModeName(), serverURL, initialView))
	// Log the raw framebuffer geometry so panel/orientation issues are visible.
	if modes, err := osReadFile("/sys/class/graphics/fb0/modes"); err == nil {
		tc.logRemote("fb0/modes: " + strings.TrimSpace(string(modes)))
	}
	if vsize, err := osReadFile("/sys/class/graphics/fb0/virtual_size"); err == nil {
		tc.logRemote("fb0/virtual_size: " + strings.TrimSpace(string(vsize)))
	}
	p := tc.getPanelSize()
	tc.logRemote(fmt.Sprintf("Detected panel (landscape): %dx%d", p.LandscapeW, p.LandscapeH))
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

	if mode == ModeSleep {
		// Low-power mode: do NOT hold the screensaver open (suspending is the
		// point) and do NOT start the power listener -- it exits on
		// goingToScreenSaver, which is exactly the suspend we want to allow.
		// The power key is a wake source here, not an exit.
		tc.exitOnPowerKey = false
		tc.startInputListeners(ctx, cancel)
		tc.runSleepLoop(ctx, cancel, wantsSuspend(os.Args))
		return
	}

	// Resident mode: hold the device awake, listen for sleep/power, and poll.
	lipcSet("com.lab126.powerd", "preventScreenSaver", "1")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigCh
		tc.logRemote("OS signal received. Exiting...")
		cancel()
	}()

	tc.startInputListeners(ctx, cancel)
	go tc.startPowerListener(ctx, cancel)

	initialPollSec := tc.fetchAndDrawDashboard(ctx, cancel)
	tc.runPollLoop(ctx, cancel, tc.getNextPollInterval(initialPollSec))
}

// minSuspendInterval is the shortest poll interval at which we bother to
// suspend. Below this, the ~15-25s suspend/resume round-trip (RTC relock, EPDC
// refresh, Wi-Fi re-association) costs more than it saves, so we stay awake.
var minSuspendInterval = 120 * time.Second

// runSleepLoop is the low-power mode for a DEDICATED e-ink dashboard. It cycles
// true suspend-to-RAM with an RTC wake, which the device validates as:
//
//	render -> (once) stop lab126_gui + unload the screensaver
//	  -> arm /sys/class/rtc/rtc0/wakealarm +interval
//	  -> disable Wi-Fi (releases the WLAN wakeup source; else EBUSY)
//	  -> echo mem > /sys/power/state   (blocks until the RTC fires)
//	  -> re-enable Wi-Fi, render again
//
// The rendered e-ink image is bistable and stays visible through the suspend.
// `allowSuspend` gates the whole path for safe testing; without it we wait on
// the wall clock. Intervals below minSuspendInterval also stay awake.
func (tc *TrackerClient) runSleepLoop(ctx context.Context, cancel context.CancelFunc, allowSuspend bool) {
	prepared := false
	for {
		select {
		case <-ctx.Done():
			tc.cleanup()
			return
		default:
		}

		// Stay awake and ensure Wi-Fi is up to fetch/render.
		lipcSet("com.lab126.powerd", "preventScreenSaver", "1")
		tc.setWireless(true)
		if !tc.waitForNetwork(ctx) {
			tc.cleanup()
			return
		}
		serverPollSec := tc.fetchAndDrawDashboard(ctx, cancel)
		// Sleep mode follows the *schedule's* cadence, deliberately ignoring the
		// resident-mode "fast poll" hold. Otherwise an off-peak data tap held the
		// device awake for 10 minutes showing the idle face -- while the button
		// handlers were still live (deceptive). Off-peak we suspend; peak (60s)
		// keeps us awake and interactive.
		interval := time.Duration(serverPollSec) * time.Second
		if serverPollSec <= 0 {
			interval = tc.getNextPollInterval(serverPollSec)
		}

		if !allowSuspend || interval < minSuspendInterval {
			tc.logRemote(fmt.Sprintf("Sleep mode: staying awake for %s.", interval.Round(time.Second)))
			if !tc.sleepWallClock(ctx, interval) {
				tc.cleanup()
				return
			}
			continue
		}

		if !prepared {
			tc.prepareDisplayForSleep(ctx)
			prepared = true
			tc.logRemote("Sleep mode: framework stopped, screensaver unloaded.")
		}

		// Touch cannot wake this SoC (the Parade driver never arms its IRQ for
		// wake, and the touch rails are cut in suspend), so we do not arm it.
		// The power button is the wake source; pressing it starts an interaction
		// session (detected below via a still-armed RTC alarm).
		tc.releaseScreenSaver()
		if !tc.armSysfsWake(interval) {
			tc.logRemote("Sleep mode: could not arm RTC; wall-clock wait.")
			tc.setWireless(true)
			if !tc.sleepWallClock(ctx, interval) {
				tc.cleanup()
				return
			}
			continue
		}

		tc.logRemote(fmt.Sprintf("Sleep mode: suspending for %s (rtc armed, face=%s).", interval.Round(time.Second), tc.getPresentation()))
		tc.setWireless(false)
		time.Sleep(suspendSettleDelay)
		elapsed, err := tc.enterSuspend()
		tc.setWireless(true)
		if err != nil {
			tc.logRemote(fmt.Sprintf("Sleep mode: suspend failed (%v) after %s; wall-clock wait.", err, elapsed.Round(time.Second)))
			if !tc.sleepWallClock(ctx, interval) {
				tc.cleanup()
				return
			}
			continue
		}

		// Wake reason: a fired RTC alarm clears itself, so a still-armed alarm
		// means a person woke us by pressing the power button, not the schedule.
		if tc.rtcAlarmStillArmed() {
			tc.disarmRTC()
			tc.logRemote(fmt.Sprintf("Power-button wake after %s: starting interactive session.", elapsed.Round(time.Second)))
			tc.setInteracting(true)
			if !tc.interactionAwake(ctx, cancel, interactionHoldDuration) {
				tc.cleanup()
				return
			}
			tc.setInteracting(false)
			continue
		}
		tc.logRemote(fmt.Sprintf("Sleep mode: woke after %s.", elapsed.Round(time.Second)))
	}
}

// waitForNetwork waits until the default route is reachable, so a fetch right
// after resume doesn't fail before Wi-Fi has re-associated. Bounded so it can't
// hang; the caller still proceeds and lets the fetch retry if it times out.
func (tc *TrackerClient) waitForNetwork(ctx context.Context) bool {
	for i := 0; i < 20; i++ {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		if checkNetworkFn(ctx) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return true
}

// checkNetworkFn reports whether the network looks usable. Seam for tests.
var checkNetworkFn = func(ctx context.Context) bool {
	cmd := execCommandContext(ctx, "ip", "route")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "default")
}

// sleepWallClock waits `d`, interruptible by ctx. Returns false if ctx ended.
func (tc *TrackerClient) sleepWallClock(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
