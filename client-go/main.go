package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
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
	BinaryPath = "/tmp/tracker"
	ImagePath  = "/tmp/dashboard.png"
	// ManualHoldDuration bounds the view-override hold and the manual-lighting
	// hold (how long a user's explicit choice survives before auto resumes).
	ManualHoldDuration = 45 * time.Minute
)

// TrackerClient coordinates e-ink display updates, power states, inputs, and server communication.
type TrackerClient struct {
	client    *http.Client
	refreshCh chan struct{}
	// touchCh fires on ANY recognised touch, so an interaction session stays
	// alive while the user is poking at the screen (a frontlight tap must reset
	// the idle timer too, not just data taps).
	touchCh    chan struct{}
	logCh      chan string
	logStarted sync.Once
	panelOnce  sync.Once
	panelSize  PanelSize
	// exitOnPowerKey, when true (resident mode), treats a hardware power-key
	// press as a request to exit. In low-power dashboard mode it is false: the
	// power key is a wake source, not an exit, so a press must not kill us.
	exitOnPowerKey bool
	wg             sync.WaitGroup

	mu sync.Mutex
	// Fields protected by mu:
	// interacting is true while the client is in an awake power-button session,
	// during which it requests the full tappable dashboard from the server.
	interacting         bool
	serverURL           string
	manualLightTime     time.Time
	manualViewTime      time.Time
	lastDataInteraction time.Time
	lastETag            string
	consecutiveErrors   int
	viewMode            string
	lastRenderedView    string
	// presentation is the server-advised visual/interaction state: "interactive"
	// (tappable dashboard, awake), "idle" (suspended; press power to interact)
	// or "dormant" (overnight). Logged for observability.
	presentation string
}

// NewTrackerClient constructs an initialized TrackerClient with default channels and HTTP timeouts.
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

// Wait blocks until all background goroutines tracked by tc have finished.
func (tc *TrackerClient) Wait() {
	tc.wg.Wait()
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
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	tc := NewTrackerClient(serverURL, initialView)
	defer func() {
		cancel()
		tc.Wait()
	}()

	tc.wg.Add(1)
	go func() {
		defer tc.wg.Done()
		select {
		case <-parent.Done():
			return
		case <-ctx.Done():
			if parent.Err() == nil {
				tc.logRemote("OS signal received. Exiting...")
			}
		}
	}()

	tc.startLogSender(ctx)
	_ = osWriteFile("/tmp/tracker_server.txt", []byte(serverURL), 0644)
	_ = osWriteFile("/mnt/us/documents/tracker_server.txt", []byte(serverURL), 0644)

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

	lipcSet("com.lab126.powerd", "preventScreenSaver", "1")

	tc.startInputListeners(ctx, cancel)
	tc.wg.Add(1)
	go func() {
		defer tc.wg.Done()
		tc.startPowerListener(ctx, cancel)
	}()

	initialPollSec := tc.fetchAndDrawDashboard(ctx, cancel)
	tc.runPollLoop(ctx, cancel, tc.getNextPollInterval(initialPollSec))
}
