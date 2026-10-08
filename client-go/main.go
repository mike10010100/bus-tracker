package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	Version       = "1.1.0"
	ServerURL     = "http://192.168.86.193:8000"
	BinaryPath    = "/tmp/tracker"
	ImagePath     = "/tmp/dashboard.png"
	PollInterval  = 45 * time.Second
	ManualHoldDuration = 45 * time.Minute
)

// Linux input subsystem constants
const (
	EV_SYN    = 0x00
	EV_KEY    = 0x01
	EV_ABS    = 0x03
	BTN_TOUCH = 0x14a // 330

	ABS_X             = 0x00
	ABS_Y             = 0x01
	ABS_MT_POSITION_X = 0x35
	ABS_MT_POSITION_Y = 0x36
)

type inputEvent32 struct {
	Sec   int32
	Usec  int32
	Type  uint16
	Code  uint16
	Value int32
}

type inputEvent64 struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

type TrackerClient struct {
	serverURL       string
	client          *http.Client
	manualLightTime time.Time
	mu              sync.Mutex
	refreshCh       chan struct{}
	lastBinaryMod   string
}

func NewTrackerClient(server string) *TrackerClient {
	return &TrackerClient{
		serverURL: server,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
		refreshCh: make(chan struct{}, 1),
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

	var nextIntensity, nextWarmth int
	switch {
	case curr == 0:
		nextIntensity = 8
		nextWarmth = 12
	case curr <= 12:
		nextIntensity = 18
		nextWarmth = 8
	default:
		nextIntensity = 0
		nextWarmth = 0
	}

	lipcSet("com.lab126.powerd", "flIntensity", strconv.Itoa(nextIntensity))
	lipcSet("com.lab126.powerd", "schedAmberLevel", strconv.Itoa(nextWarmth))
}

// findTouchDevice locates the touchscreen event node in /dev/input
func findTouchDevice() string {
	// Check /proc/bus/input/devices first
	data, err := os.ReadFile("/proc/bus/input/devices")
	if err == nil {
		lines := strings.Split(string(data), "\n")
		isTouch := false
		for _, line := range lines {
			lower := strings.ToLower(line)
			if strings.Contains(lower, "touch") || strings.Contains(lower, "mxt") || strings.Contains(lower, "zforce") || strings.Contains(lower, "cyttsp") {
				isTouch = true
			}
			if isTouch && strings.Contains(line, "Handlers=") {
				fields := strings.Fields(line)
				for _, f := range fields {
					if strings.HasPrefix(f, "event") {
						candidate := "/dev/input/" + f
						if _, err := os.Stat(candidate); err == nil {
							return candidate
						}
					}
				}
			}
			if strings.TrimSpace(line) == "" {
				isTouch = false
			}
		}
	}

	// Fallbacks
	for _, dev := range []string{"/dev/input/event1", "/dev/input/event0", "/dev/input/event2"} {
		if _, err := os.Stat(dev); err == nil {
			return dev
		}
	}
	return "/dev/input/event0"
}

// startTouchListener reads input_event from touchscreen and handles gestures
func (tc *TrackerClient) startTouchListener(ctx context.Context, exitCancel context.CancelFunc) {
	devPath := findTouchDevice()
	f, err := os.Open(devPath)
	if err != nil {
		return
	}
	defer f.Close()

	// Detect 16-byte vs 24-byte input_event structure dynamically
	buf := make([]byte, 24)
	n, err := f.Read(buf)
	if err != nil || (n != 16 && n != 24) {
		// Default to 16 bytes for 32-bit ARM Linux on Kindle
		n = 16
	}
	eventSize := n

	var (
		curX, curY   int32
		lastTapTime  time.Time
		tapTimer     *time.Timer
		tapTimerLock sync.Mutex
	)

	// Process raw input event
	processEvent := func(evType, evCode uint16, evValue int32) {
		if evType == EV_ABS {
			if evCode == ABS_X || evCode == ABS_MT_POSITION_X {
				curX = evValue
			} else if evCode == ABS_Y || evCode == ABS_MT_POSITION_Y {
				curY = evValue
			}
		} else if (evType == EV_KEY && evCode == BTN_TOUCH && evValue == 0) || (evType == EV_ABS && evCode == 0x39 && evValue == -1) {
			// Finger release / Tap completed!
			now := time.Now()
			diff := now.Sub(lastTapTime)

			// 1. Double-Tap Check (within 1.2 seconds) -> EXIT
			if diff < 1200*time.Millisecond && diff > 50*time.Millisecond {
				tapTimerLock.Lock()
				if tapTimer != nil {
					tapTimer.Stop()
				}
				tapTimerLock.Unlock()
				exitCancel()
				return
			}
			lastTapTime = now

			// 2. Corner Touch Gestures (assuming 1236 x 1648 or similar coordinates)
			// Top-Right corner tap -> Immediate Exit
			if (curX > 1050 && curY < 180) || (curX < 180 && curY > 1450) {
				exitCancel()
				return
			}

			// Top-Left corner tap -> Force immediate refresh
			if (curX < 200 && curY < 180) || (curX < 200 && curY < 200) {
				select {
				case tc.refreshCh <- struct{}{}:
				default:
				}
				return
			}

			// 3. Normal Single Tap -> Wait briefly to ensure it's not a double-tap, then cycle light
			tapTimerLock.Lock()
			if tapTimer != nil {
				tapTimer.Stop()
			}
			tapTimer = time.AfterFunc(350*time.Millisecond, func() {
				tc.cycleFrontlight()
			})
			tapTimerLock.Unlock()
		}
	}

	eventBuf := make([]byte, eventSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, err := io.ReadFull(f, eventBuf)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		var evType, evCode uint16
		var evValue int32
		if eventSize == 16 {
			var ev inputEvent32
			_ = binary.Read(bytes.NewReader(eventBuf), binary.LittleEndian, &ev)
			evType, evCode, evValue = ev.Type, ev.Code, ev.Value
		} else {
			var ev inputEvent64
			_ = binary.Read(bytes.NewReader(eventBuf), binary.LittleEndian, &ev)
			evType, evCode, evValue = ev.Type, ev.Code, ev.Value
		}

		processEvent(evType, evCode, evValue)
	}
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
			exitCancel()
			return
		}
	}
}

// checkOTAUpdate checks if Mac server has a newer binary build and hot-swaps in-place
func (tc *TrackerClient) checkOTAUpdate(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, "HEAD", tc.serverURL+"/tracker-arm", nil)
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

	lastMod := resp.Header.Get("Last-Modified")
	if lastMod == "" {
		lastMod = resp.Header.Get("ETag")
	}
	if lastMod == "" {
		lastMod = resp.Header.Get("Content-Length")
	}

	if tc.lastBinaryMod == "" {
		tc.lastBinaryMod = lastMod
		return false
	}

	if lastMod != tc.lastBinaryMod {
		// Newer binary available on Mac! Download to /tmp/tracker.update
		updatePath := "/tmp/tracker.update"
		getReq, _ := http.NewRequestWithContext(ctx, "GET", tc.serverURL+"/tracker-arm", nil)
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

		// Self-exec hot swap!
		tc.cleanup()
		_ = syscall.Exec(BinaryPath, os.Args, os.Environ())
		return true
	}
	return false
}

// fetchAndDrawDashboard fetches dashboard PNG, applies lighting, and pushes to e-ink
func (tc *TrackerClient) fetchAndDrawDashboard(ctx context.Context, exitCancel context.CancelFunc) {
	url := fmt.Sprintf("%s/dashboard.png?kindle=pw5&t=%d", tc.serverURL, time.Now().Unix())
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return
	}

	resp, err := tc.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	// HTTP 205 signals remote stop command
	if resp.StatusCode == 205 {
		exitCancel()
		return
	}

	if resp.StatusCode != http.StatusOK {
		return
	}

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
		if brightStr != "" {
			lipcSet("com.lab126.powerd", "flIntensity", brightStr)
		}
		if warmStr != "" {
			lipcSet("com.lab126.powerd", "schedAmberLevel", warmStr)
		}
	}
}

func main() {
	// Silence standard error
	if nullFile, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		_ = syscall.Dup2(int(nullFile.Fd()), int(os.Stderr.Fd()))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tc := NewTrackerClient(ServerURL)

	// Ensure Kindle stays awake while dashboard is running
	lipcSet("com.lab126.powerd", "preventScreenSaver", "1")

	// Intercept OS termination signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigCh
		cancel()
	}()

	// Start background listeners
	go tc.startTouchListener(ctx, cancel)
	go tc.startPowerListener(ctx, cancel)

	// Clear screen on initial launch
	cmd := exec.Command("eips", "-c")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()

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
