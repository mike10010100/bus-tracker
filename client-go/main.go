package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	Version            = "1.5.4"
	BinaryPath         = "/tmp/tracker"
	ImagePath          = "/tmp/dashboard.png"
	PollInterval       = 45 * time.Second
	ManualHoldDuration = 45 * time.Minute
)

type TrackerClient struct {
	serverURL         string
	client            *http.Client
	manualLightTime   time.Time
	manualViewTime    time.Time
	mu                sync.Mutex
	refreshCh         chan struct{}
	lastBinaryMod     string
	consecutiveErrors int
	viewMode          string
	lastRenderedView  string
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

// logRemote sends non-blocking diagnostic logs to the active server
func (tc *TrackerClient) logRemote(msg string) {
	server := tc.getServerURL()
	go func() {
		req, err := http.NewRequest("POST", server+"/log", strings.NewReader(msg))
		if err == nil {
			req.Header.Set("Content-Type", "text/plain")
			resp, err := tc.client.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}
	}()
}

func (tc *TrackerClient) handleNetworkError(ctx context.Context) {
	tc.mu.Lock()
	tc.consecutiveErrors++
	errCount := tc.consecutiveErrors
	tc.mu.Unlock()

	// If server is unreachable, immediately trigger LAN auto-discovery
	if errCount >= 1 {
		tc.logRemote(fmt.Sprintf("Server unreachable (error %d). Triggering LAN auto-discovery...", errCount))
		if discovered, err := AutoDiscoverServer(ctx); err == nil && discovered != "" {
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
	cmd := exec.Command("lipc-set-prop", "-i", prop, key, val)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
}

// lipcGet reads a property using lipc-get-prop
func lipcGet(prop, key string) string {
	cmd := exec.Command("lipc-get-prop", prop, key)
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
	cmd := exec.Command("eips", "-c")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()

	// Restore Kindle Library / Home booklet
	lipcSet("com.lab126.appmgrd", "start", "app://com.lab126.booklet.home")
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

// startInputListeners opens ALL /dev/input/event* devices and multiplexes events into eventCh
func (tc *TrackerClient) startInputListeners(ctx context.Context, cancel context.CancelFunc) {
	matches, err := filepath.Glob("/dev/input/event*")
	if err != nil || len(matches) == 0 {
		matches = []string{"/dev/input/event0", "/dev/input/event1", "/dev/input/event2"}
	}

	tc.logRemote(fmt.Sprintf("Found input devices: %v", matches))

	eventCh := make(chan RawEventMsg, 128)

	for _, devPath := range matches {
		f, err := os.Open(devPath)
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
	go func() {
		gd := NewGestureDetector(DefaultGestureConfig())
		defer gd.Stop()

		gd.OnLog = func(msg string) {
			tc.logRemote(msg)
		}

		gd.OnSingleTap = func(x, y int32) {
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
			tc.logRemote(fmt.Sprintf("Top-Left corner tapped at (%d, %d)! Refreshing...", x, y))
			select {
			case tc.refreshCh <- struct{}{}:
			default:
			}
		}

		gd.OnBottomLeftTap = func(x, y int32) {
			newMode := tc.cycleViewMode()
			tc.logRemote(fmt.Sprintf("Bottom-Left corner tapped at (%d, %d)! View mode cycled to: %s. Refreshing...", x, y, newMode))
			select {
			case tc.refreshCh <- struct{}{}:
			default:
			}
		}

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
	}()
}

// startPowerListener watches for power button sleep events via lipc
func (tc *TrackerClient) startPowerListener(ctx context.Context, exitCancel context.CancelFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		cmd := exec.CommandContext(ctx, "lipc-wait-event", "com.lab126.powerd", "goingToScreenSaver")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err == nil {
			tc.logRemote("powerd goingToScreenSaver event received! Exiting...")
			exitCancel()
			return
		}
	}
}

// checkOTAUpdate checks if server has a newer binary build and hot-swaps in-place
func (tc *TrackerClient) checkOTAUpdate(ctx context.Context) bool {
	server := tc.getServerURL()
	req, err := http.NewRequestWithContext(ctx, "HEAD", server+"/tracker-arm", nil)
	if err != nil {
		return false
	}
	resp, err := tc.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	if canonical := resp.Header.Get("X-Tracker-Server"); canonical != "" && canonical != server {
		tc.setServerURL(canonical)
		_ = SaveServerURL(canonical)
		server = canonical
	}

	serverVer := resp.Header.Get("X-Tracker-Version")
	lastMod := resp.Header.Get("Last-Modified")
	if lastMod == "" {
		lastMod = resp.Header.Get("ETag")
	}
	if lastMod == "" {
		lastMod = resp.Header.Get("Content-Length")
	}

	should, reason := ShouldUpdate(serverVer, Version, lastMod, tc.lastBinaryMod)

	if tc.lastBinaryMod == "" {
		tc.lastBinaryMod = lastMod
	}

	if should {
		tc.logRemote(fmt.Sprintf("OTA update triggered: %s. Hot-reloading...", reason))

		updatePath := "/tmp/tracker.update"
		getReq, _ := http.NewRequestWithContext(ctx, "GET", server+"/tracker-arm", nil)
		getResp, err := tc.client.Do(getReq)
		if err != nil || getResp.StatusCode != http.StatusOK {
			return false
		}
		defer getResp.Body.Close()

		out, err := os.OpenFile(updatePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return false
		}
		_, err = io.Copy(out, getResp.Body)
		out.Close()
		if err != nil {
			os.Remove(updatePath)
			return false
		}

		_ = os.Rename(updatePath, BinaryPath)
		_ = os.Chmod(BinaryPath, 0755)

		_ = SaveServerURL(server)

		newArgs := []string{BinaryPath, "-server", server, "-view", tc.getViewMode()}
		tc.logRemote("Executing updated binary via syscall.Exec...")
		_ = syscall.Exec(BinaryPath, newArgs, os.Environ())
		return true
	}
	return false
}

// fetchAndDrawDashboard fetches dashboard PNG, applies lighting, and pushes to e-ink
func (tc *TrackerClient) fetchAndDrawDashboard(ctx context.Context, exitCancel context.CancelFunc) {
	batt := GetBatteryInfo()
	chargeVal := 0
	if batt.IsCharging {
		chargeVal = 1
	}

	viewMode := tc.getViewMode()
	server := tc.getServerURL()
	url := fmt.Sprintf("%s/dashboard.png?kindle=pw5&batt=%d&charging=%d&view=%s&t=%d", server, batt.Level, chargeVal, viewMode, time.Now().Unix())
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		tc.handleNetworkError(ctx)
		return
	}
	req.Header.Set("X-Kindle-Battery", strconv.Itoa(batt.Level))
	req.Header.Set("X-Kindle-Charging", strconv.Itoa(chargeVal))
	req.Header.Set("X-Tracker-View", viewMode)

	resp, err := tc.client.Do(req)
	if err != nil {
		tc.handleNetworkError(ctx)
		return
	}
	defer resp.Body.Close()

	if canonical := resp.Header.Get("X-Tracker-Server"); canonical != "" && canonical != server {
		tc.setServerURL(canonical)
		_ = SaveServerURL(canonical)
	}

	if resView := resp.Header.Get("X-Resolved-View"); resView != "" {
		tc.mu.Lock()
		tc.lastRenderedView = resView
		tc.mu.Unlock()
	}

	// HTTP 205 signals remote stop command
	if resp.StatusCode == 205 {
		tc.logRemote("Server sent HTTP 205 Stop signal. Exiting cleanly...")
		exitCancel()
		return
	}

	if resp.StatusCode != http.StatusOK {
		tc.handleNetworkError(ctx)
		return
	}

	tc.mu.Lock()
	tc.consecutiveErrors = 0
	tc.mu.Unlock()

	// Write image to /tmp/dashboard.png
	tmpFile, err := os.Create(ImagePath)
	if err != nil {
		return
	}
	_, err = io.Copy(tmpFile, resp.Body)
	tmpFile.Close()
	if err != nil {
		return
	}

	// Push directly to Kindle e-ink display
	cmd := exec.Command("eips", "-f", "-g", ImagePath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()

	// Apply astronomical lighting headers if manual override is inactive
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
			tc.logRemote(fmt.Sprintf("Astronomical auto-dimming applied: brightness %s -> %s", currB, brightStr))
		}
		if warmStr != "" && warmStr != currW {
			lipcSet("com.lab126.powerd", "schedAmberLevel", warmStr)
			tc.logRemote(fmt.Sprintf("Astronomical auto-dimming applied: warmth %s -> %s", currW, warmStr))
		}
	}
}

func main() {
	// Silence standard error on headless Kindle
	if nullFile, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		_ = syscall.Dup2(int(nullFile.Fd()), int(os.Stderr.Fd()))
	}

	serverURL := GetServerURL()
	initialView := ResolveViewMode(os.Args)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tc := NewTrackerClient(serverURL, initialView)
	_ = os.WriteFile("/tmp/tracker_server.txt", []byte(serverURL), 0644)
	_ = os.WriteFile("/mnt/us/documents/tracker_server.txt", []byte(serverURL), 0644)

	// Send initial startup diagnostic
	tc.logRemote(fmt.Sprintf("Bus Tracker v%s starting up (server: %s, view: %s)...", Version, serverURL, initialView))
	if devData, err := os.ReadFile("/proc/bus/input/devices"); err == nil {
		tc.logRemote(fmt.Sprintf("Input devices:\n%s", string(devData)))
	}

	// Ensure Kindle stays awake while dashboard is running
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

	// Initial fetch
	tc.fetchAndDrawDashboard(ctx, cancel)

	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			tc.cleanup()
			return

		case <-tc.refreshCh:
			// Forced refresh requested via screen tap
			tc.fetchAndDrawDashboard(ctx, cancel)

		case <-ticker.C:
			// 1. Check for OTA binary update on Mac
			if tc.checkOTAUpdate(ctx) {
				return // Replaced by new binary via syscall.Exec
			}
			// 2. Fetch and render latest dashboard
			tc.fetchAndDrawDashboard(ctx, cancel)
		}
	}
}
