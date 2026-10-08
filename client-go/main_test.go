package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// patchRuntime restores all process/file seams after the test.
func patchRuntime(t *testing.T) {
	t.Helper()
	origOpenFile := osOpenFile
	origRemove := osRemove
	origRename := osRename
	origChmod := osChmod
	origCreate := osCreate
	origExec := sysExec
	origGlob := globInputs
	origOpen := osOpen
	origReadFile := osReadFile
	origGetBattery := GetBatteryInfo
	origDiscover := autoDiscover

	// Discovery is disabled by default in tests so a real LAN server cannot
	// interfere with assertions; individual tests may override it.
	autoDiscover = func(ctx context.Context) (string, error) { return "", os.ErrNotExist }

	t.Cleanup(func() {
		osOpenFile = origOpenFile
		osRemove = origRemove
		osRename = origRename
		osChmod = origChmod
		osCreate = origCreate
		sysExec = origExec
		globInputs = origGlob
		osOpen = origOpen
		osReadFile = origReadFile
		GetBatteryInfo = origGetBattery
		autoDiscover = origDiscover
	})
}

// tempFileCreate returns an osCreate replacement that materializes a real temp
// file so io.Copy succeeds during OTA download.
func tempFileCreate(t *testing.T) func(string) (*os.File, error) {
	t.Helper()
	return func(name string) (*os.File, error) {
		return os.CreateTemp(t.TempDir(), "tracker-*")
	}
}

// otaServer serves the binary at /tracker-arm with an optional digest header.
func otaServer(t *testing.T, binary []byte, digest string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tracker-arm" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-Tracker-SHA256", digest)
		w.WriteHeader(http.StatusOK)
		w.Write(binary)
	}))
}

func TestMaybeUpdateBinary_ValidChecksumExecs(t *testing.T) {
	patchRuntime(t)
	binary := []byte("NEWBINARY-BYTES")
	sum := sha256.Sum256(binary)
	digest := hex.EncodeToString(sum[:])
	srv := otaServer(t, binary, digest)
	defer srv.Close()

	osOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		return os.CreateTemp(t.TempDir(), "ota-*")
	}
	osRename = func(oldpath, newpath string) error { return nil }
	osChmod = func(name string, mode os.FileMode) error { return nil }

	var execCalled bool
	sysExec = func(argv0 string, argv []string, envv []string) error {
		execCalled = true
		return nil
	}

	tc := NewTrackerClient(srv.URL, "auto")
	if !tc.maybeUpdateBinary(context.Background(), "9.9.9", digest) {
		t.Fatal("expected verified OTA update to be applied")
	}
	if !execCalled {
		t.Error("expected sysExec after verified download")
	}
}

func TestMaybeUpdateBinary_ChecksumMismatchRejects(t *testing.T) {
	patchRuntime(t)
	srv := otaServer(t, []byte("TAMPERED"), "deadbeef")
	defer srv.Close()

	var removed bool
	osOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		return os.CreateTemp(t.TempDir(), "ota-*")
	}
	osRemove = func(name string) error { removed = true; return nil }
	sysExec = func(string, []string, []string) error {
		t.Fatal("sysExec must NOT be called on checksum mismatch")
		return nil
	}

	tc := NewTrackerClient(srv.URL, "auto")
	if tc.maybeUpdateBinary(context.Background(), "9.9.9", "deadbeef") {
		t.Fatal("OTA must be rejected on checksum mismatch")
	}
	if !removed {
		t.Error("expected corrupted update file to be removed")
	}
}

func TestMaybeUpdateBinary_MissingDigestFailsClosed(t *testing.T) {
	patchRuntime(t)
	srv := otaServer(t, []byte("NO DIGEST"), "")
	defer srv.Close()

	osOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		return os.CreateTemp(t.TempDir(), "ota-*")
	}
	osRemove = func(name string) error { return nil }
	sysExec = func(string, []string, []string) error {
		t.Fatal("sysExec must NOT run without a digest")
		return nil
	}

	tc := NewTrackerClient(srv.URL, "auto")
	if tc.maybeUpdateBinary(context.Background(), "9.9.9", "") {
		t.Fatal("OTA without digest must fail closed")
	}
}

func TestMaybeUpdateBinary_NoUpdateWhenVersionMatches(t *testing.T) {
	patchRuntime(t)
	var contacted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tc := NewTrackerClient(srv.URL, "auto")
	if tc.maybeUpdateBinary(context.Background(), Version, "whatever") {
		t.Fatal("no update expected when versions match")
	}
	if contacted {
		t.Error("matching version must not perform any network I/O")
	}
}

func TestFetchAndDrawDashboard_Success(t *testing.T) {
	patchRuntime(t)
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kindle-Poll-Interval", "45")
		w.Header().Set("X-Tracker-View", r.Header.Get("X-Tracker-View"))
		w.Header().Set("X-Resolved-View", "morning")
		w.WriteHeader(http.StatusOK)
		w.Write(png)
	}))
	defer srv.Close()

	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: 88, IsCharging: true} }
	osCreate = tempFileCreate(t)

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if got := tc.fetchAndDrawDashboard(ctx, cancel); got != 45 {
		t.Errorf("expected poll interval 45, got %d", got)
	}
	if tc.lastRenderedView != "morning" {
		t.Errorf("expected resolved view captured, got %q", tc.lastRenderedView)
	}
}

func TestFetchAndDrawDashboard_ReportsPanelDimensions(t *testing.T) {
	patchRuntime(t)
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-Kindle-Poll-Interval", "45")
		w.WriteHeader(http.StatusOK)
		w.Write(png)
	}))
	defer srv.Close()

	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: 88} }
	osCreate = tempFileCreate(t)
	osReadFile = func(string) ([]byte, error) { return []byte("1236,1648\n"), nil }

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc.fetchAndDrawDashboard(ctx, cancel)

	if !strings.Contains(gotQuery, "kindle=pw5") {
		t.Errorf("expected kindle=pw5 in query, got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "w=1648") || !strings.Contains(gotQuery, "h=1236") {
		t.Errorf("expected native panel dims in query, got %q", gotQuery)
	}
}

func TestGetPanelSize_CachesDetection(t *testing.T) {
	patchRuntime(t)
	calls := 0
	osReadFile = func(string) ([]byte, error) {
		calls++
		return []byte("1236,1648\n"), nil
	}
	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	_ = tc.getPanelSize()
	_ = tc.getPanelSize()
	if calls != 1 {
		t.Errorf("expected panel size detected once, got %d reads", calls)
	}
}

func TestFetchAndDrawDashboard_304SkipsRefresh(t *testing.T) {
	patchRuntime(t)
	var sentETag string
	var eipsCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentETag = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("X-Kindle-Poll-Interval", "600")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: 88} }
	orig := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd {
		if name == "eips" {
			eipsCalled = true
		}
		return orig("true")
	}
	defer func() { execCommand = orig }()

	tc := NewTrackerClient(srv.URL, "auto")
	tc.lastETag = `"abc123"`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if got := tc.fetchAndDrawDashboard(ctx, cancel); got != 600 {
		t.Errorf("expected poll interval 600 on 304, got %d", got)
	}
	if sentETag != `"abc123"` {
		t.Errorf("expected If-None-Match to be sent, got %q", sentETag)
	}
	if eipsCalled {
		t.Error("304 must not trigger an eips refresh")
	}
}

func TestFetchAndDrawDashboard_StoresETag(t *testing.T) {
	patchRuntime(t)
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"newtag"`)
		w.Header().Set("X-Kindle-Poll-Interval", "45")
		w.WriteHeader(http.StatusOK)
		w.Write(png)
	}))
	defer srv.Close()
	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: 88} }
	osCreate = tempFileCreate(t)

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc.fetchAndDrawDashboard(ctx, cancel)

	tc.mu.Lock()
	tag := tc.lastETag
	tc.mu.Unlock()
	if tag != `"newtag"` {
		t.Errorf("expected ETag stored, got %q", tag)
	}
}

func TestFetchAndDrawDashboard_Stop205Cancels(t *testing.T) {
	patchRuntime(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(205)
	}))
	defer srv.Close()
	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: -1} }

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	if got := tc.fetchAndDrawDashboard(ctx, cancel); got != 0 {
		t.Errorf("expected 0 on 205, got %d", got)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("expected context cancel on HTTP 205")
	}
}

func TestFetchAndDrawDashboard_AdoptsPrivateServerHeader(t *testing.T) {
	patchRuntime(t)
	origDiscover := autoDiscover
	autoDiscover = func(context.Context) (string, error) { return "", os.ErrNotExist }
	defer func() { autoDiscover = origDiscover }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tracker-Server", "http://10.0.0.55:8000")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: -1} }

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc.fetchAndDrawDashboard(ctx, cancel)
	if tc.getServerURL() != "http://10.0.0.55:8000" {
		t.Errorf("expected adoption of private header, got %s", tc.getServerURL())
	}
}

func TestFetchAndDrawDashboard_IgnoresPublicServerHeader(t *testing.T) {
	patchRuntime(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tracker-Server", "http://8.8.8.8:8000")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: -1} }

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc.fetchAndDrawDashboard(ctx, cancel)
	if tc.getServerURL() == "http://8.8.8.8:8000" {
		t.Error("must not adopt a public server header")
	}
}

func TestCleanupInvokesCommands(t *testing.T) {
	patchRuntime(t)
	var called []string
	orig := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd {
		called = append(called, name)
		return orig("true")
	}
	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	tc.cleanup()
	if len(called) == 0 {
		t.Fatal("expected cleanup to invoke external commands")
	}
}

func TestStartInputListenersFallsBackWhenGlobEmpty(t *testing.T) {
	patchRuntime(t)
	globInputs = func(string) ([]string, error) { return nil, nil }
	osOpen = func(string) (*os.File, error) { return nil, os.ErrNotExist }

	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	ctx, cancel := context.WithCancel(context.Background())
	tc.startInputListeners(ctx, cancel)
	cancel()
}

func TestCycleFrontlightReadsAndWrites(t *testing.T) {
	patchRuntime(t)
	var setProps int
	orig := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd {
		if name == "lipc-set-prop" {
			setProps++
		}
		// Route through a harmless command that always succeeds.
		return orig("true")
	}
	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	tc.cycleFrontlight()
	if setProps == 0 {
		t.Error("expected frontlight properties to be written")
	}
}

func TestConfigureGestureHandlers_WiresViewAndRefresh(t *testing.T) {
	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	gd := NewGestureDetector(DefaultGestureConfig())
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	tc.configureGestureHandlers(gd, cancel)

	if gd.OnBusesTap == nil || gd.OnBikesTap == nil || gd.OnExitTap == nil ||
		gd.OnLightTap == nil || gd.OnRefreshTap == nil || gd.OnSingleTap == nil ||
		gd.OnDoubleTap == nil || gd.OnTopLeftTap == nil || gd.OnTopRightTap == nil ||
		gd.OnBottomLeftTap == nil {
		t.Fatal("expected all gesture handlers to be wired")
	}

	// Bikes button sets morning view and queues a refresh.
	gd.OnBikesTap(0, 0)
	if tc.getViewMode() != "morning" {
		t.Errorf("bikes tap should set morning view, got %s", tc.getViewMode())
	}
	select {
	case <-tc.refreshCh:
	default:
		t.Error("bikes tap should enqueue a refresh")
	}

	// Buses button sets evening view.
	gd.OnBusesTap(0, 0)
	if tc.getViewMode() != "evening" {
		t.Errorf("buses tap should set evening view, got %s", tc.getViewMode())
	}
}

func TestConfigureGestureHandlers_AllButtonsAndCorners(t *testing.T) {
	patchRuntime(t)
	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	gd := NewGestureDetector(DefaultGestureConfig())
	ctx, cancel := context.WithCancel(context.Background())
	// Track frontlight invocations without a Kindle.
	origCmd := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd { return origCmd("true") }
	defer func() { execCommand = origCmd }()

	tc.configureGestureHandlers(gd, cancel)

	// Light button cycles the frontlight.
	gd.OnLightTap(0, 0)
	// Top-left and refresh enqueue refresh signals.
	gd.OnTopLeftTap(0, 0)
	gd.OnRefreshTap(0, 0)
	// Bottom-left cycles the view.
	tc.lastRenderedView = "evening"
	gd.OnBottomLeftTap(0, 0)
	if tc.getViewMode() != "morning" && tc.getViewMode() != "evening" {
		t.Errorf("unexpected view after bottom-left tap: %s", tc.getViewMode())
	}
	// Single tap should not panic.
	gd.OnSingleTap(0, 0)
	// Top-right exits.
	gd.OnTopRightTap(0, 0)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("top-right tap should cancel")
	}
}

func TestScreenOnlyActionsDoNotArmFastPoll(t *testing.T) {
	patchRuntime(t)
	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	gd := NewGestureDetector(DefaultGestureConfig())
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	origCmd := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd { return origCmd("true") }
	defer func() { execCommand = origCmd }()

	tc.configureGestureHandlers(gd, cancel)

	// Frontlight actions are screen-only: they must NOT pin fast polling.
	gd.OnSingleTap(0, 0)
	gd.OnLightTap(0, 0)
	tc.mu.Lock()
	armed := !tc.lastDataInteraction.IsZero()
	tc.mu.Unlock()
	if armed {
		t.Error("screen-only actions must not arm the fast-poll hold")
	}

	// Data-affecting actions DO arm it.
	gd.OnRefreshTap(0, 0)
	tc.mu.Lock()
	armed = !tc.lastDataInteraction.IsZero()
	tc.mu.Unlock()
	if !armed {
		t.Error("refresh must arm the fast-poll hold")
	}
}

func TestLogRemoteQueueDoesNotBlockAndDropsWhenFull(t *testing.T) {
	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	// Never started the sender, so nothing drains the queue.
	for i := 0; i < cap(tc.logCh)+50; i++ {
		tc.logRemote("msg") // must not block even when full
	}
	if len(tc.logCh) != cap(tc.logCh) {
		t.Errorf("expected queue to fill to capacity %d, got %d", cap(tc.logCh), len(tc.logCh))
	}
}

func TestPostDiagnosticsUploadsReport(t *testing.T) {
	patchRuntime(t)
	got := make(chan [2]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- [2]string{r.URL.Path, string(b)}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tc := NewTrackerClient(srv.URL, "auto")
	tc.postDiagnostics(false)

	select {
	case pair := <-got:
		if pair[0] != "/diag" {
			t.Errorf("expected POST /diag, got %q", pair[0])
		}
		if !strings.Contains(pair[1], "=== DIAGNOSTICS") {
			t.Errorf("expected diagnostics report in body, got %q", pair[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a diagnostics POST")
	}
}

func TestFetchAndDrawDashboard_SendsDiagnosticsWhenRequested(t *testing.T) {
	patchRuntime(t)
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	diagCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/diag" {
			b, _ := io.ReadAll(r.Body)
			diagCh <- string(b)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("X-Tracker-Diag", "1")
		w.WriteHeader(http.StatusOK)
		w.Write(png)
	}))
	defer srv.Close()

	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: 88} }
	osCreate = tempFileCreate(t)

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc.fetchAndDrawDashboard(ctx, cancel)

	select {
	case body := <-diagCh:
		if !strings.Contains(body, "=== DIAGNOSTICS") {
			t.Errorf("expected diagnostics body, got %q", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected a diagnostics upload when X-Tracker-Diag is set")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestLogSenderDrainsQueue(t *testing.T) {
	patchRuntime(t)
	var received int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/log" {
			atomic.AddInt32(&received, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc.startLogSender(ctx)

	for i := 0; i < 3; i++ {
		tc.logRemote("hello")
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&received) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&received); got != 3 {
		t.Errorf("expected 3 logs delivered, got %d", got)
	}
}

func TestRunEventLoop_FeedsTouchAndExitsOnCancel(t *testing.T) {
	patchRuntime(t)
	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	ctx, cancel := context.WithCancel(context.Background())

	eventCh := make(chan RawEventMsg, 8)
	done := make(chan struct{})
	go func() {
		tc.runEventLoop(ctx, cancel, eventCh)
		close(done)
	}()

	eventCh <- RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_POSITION_X, EvValue: 500}
	eventCh <- RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_POSITION_Y, EvValue: 500}
	eventCh <- RawEventMsg{EvType: EV_KEY, EvCode: BTN_TOUCH, EvValue: 0}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runEventLoop should exit on cancel")
	}
}

func TestStartInputListeners_RealOpenPath(t *testing.T) {
	patchRuntime(t)
	// Create a real file and have glob return it, then write an event and cancel.
	dir := t.TempDir()
	devPath := dir + "/event0"
	if err := os.WriteFile(devPath, buildEventBytes(EV_ABS, ABS_MT_POSITION_X, 400), 0644); err != nil {
		t.Fatal(err)
	}
	globInputs = func(string) ([]string, error) { return []string{devPath}, nil }

	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	ctx, cancel := context.WithCancel(context.Background())
	tc.startInputListeners(ctx, cancel)
	time.Sleep(30 * time.Millisecond)
	cancel()
}

func buildEventBytes(evType, evCode uint16, evValue int32) []byte {
	// 16-byte little-endian input_event.
	b := make([]byte, 16)
	binary.LittleEndian.PutUint16(b[8:10], evType)
	binary.LittleEndian.PutUint16(b[10:12], evCode)
	binary.LittleEndian.PutUint32(b[12:16], uint32(evValue))
	return b
}

func TestConfigureGestureHandlers_ExitCancels(t *testing.T) {
	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	gd := NewGestureDetector(DefaultGestureConfig())
	ctx, cancel := context.WithCancel(context.Background())
	tc.configureGestureHandlers(gd, cancel)

	gd.OnExitTap(0, 0)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("EXIT tap should cancel the context")
	}
}

func TestRunEventLoop_PowerKeyCancels(t *testing.T) {
	patchRuntime(t)
	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	ctx, cancel := context.WithCancel(context.Background())

	eventCh := make(chan RawEventMsg, 1)
	done := make(chan struct{})
	go func() {
		tc.runEventLoop(ctx, cancel, eventCh)
		close(done)
	}()

	eventCh <- RawEventMsg{Device: "/dev/input/event0", EvType: EV_KEY, EvCode: KEY_POWER, EvValue: 1}

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("power key should cancel the context")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runEventLoop should return after power key")
	}
}

func TestRunPollLoop_CleansUpOnCancel(t *testing.T) {
	patchRuntime(t)
	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: -1} }
	osCreate = tempFileCreate(t)
	var cleaned bool
	orig := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd {
		if name == "eips" && len(arg) > 0 && arg[0] == "-c" {
			cleaned = true
		}
		return orig("true")
	}

	// Server returns 500 so no OTA/download occurs; loop should idle.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tc.runPollLoop(ctx, cancel, 10*time.Millisecond)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runPollLoop should exit on cancel")
	}
	if !cleaned {
		t.Error("expected cleanup to clear the screen on loop exit")
	}
}

func TestHandleNetworkErrorIncrementsCounter(t *testing.T) {
	patchRuntime(t)
	tc := NewTrackerClient("http://127.0.0.1:1", "auto")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	tc.handleNetworkError(ctx)

	tc.mu.Lock()
	count := tc.consecutiveErrors
	tc.mu.Unlock()
	if count < 1 {
		t.Errorf("expected consecutiveErrors >= 1, got %d", count)
	}
}

func TestHandleNetworkError_RediscoveryResetsCounter(t *testing.T) {
	patchRuntime(t)
	origDiscover := autoDiscover
	autoDiscover = func(context.Context) (string, error) { return "http://10.1.2.3:8000", nil }
	defer func() { autoDiscover = origDiscover }()

	tc := NewTrackerClient("http://127.0.0.1:1", "auto")
	tc.handleNetworkError(context.Background())

	if tc.getServerURL() != "http://10.1.2.3:8000" {
		t.Errorf("expected rediscovered server URL, got %s", tc.getServerURL())
	}
	tc.mu.Lock()
	count := tc.consecutiveErrors
	tc.mu.Unlock()
	if count != 0 {
		t.Errorf("expected error counter reset after rediscovery, got %d", count)
	}
}

func TestStartPowerListener_CancelsOnEvent(t *testing.T) {
	patchRuntime(t)
	orig := execCommandContext
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		return orig(ctx, "true")
	}
	defer func() { execCommandContext = orig }()

	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		tc.startPowerListener(ctx, cancel)
		close(done)
	}()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("power listener should cancel context on sleep event")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("power listener should return")
	}
}

func TestStartPowerListener_ReturnsOnContextCancel(t *testing.T) {
	patchRuntime(t)
	orig := execCommandContext
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		// Long-running command that is killed when ctx is cancelled.
		return orig(ctx, "sleep", "30")
	}
	defer func() { execCommandContext = orig }()

	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		tc.startPowerListener(ctx, cancel)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("power listener should return after context cancel")
	}
}

func TestGetServerURL_UsesDiscoverySeam(t *testing.T) {
	origDiscover := autoDiscover
	autoDiscover = func(context.Context) (string, error) { return "http://10.9.9.9:8000", nil }
	defer func() { autoDiscover = origDiscover }()
	// With no CLI/env/file server, GetServerURL should consult discovery.
	got := GetServerURL()
	if got == "" {
		t.Fatal("expected a non-empty server URL")
	}
}

func TestStartInputListeners_OpensProvidedDevices(t *testing.T) {
	patchRuntime(t)
	// Simulate two device files; both fail to open, exercising the continue path.
	globInputs = func(string) ([]string, error) {
		return []string{"/dev/input/event0", "/dev/input/event1"}, nil
	}
	osOpen = func(string) (*os.File, error) { return nil, os.ErrNotExist }

	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	ctx, cancel := context.WithCancel(context.Background())
	tc.startInputListeners(ctx, cancel)
	cancel()
}

func TestRun_StartsAndStops(t *testing.T) {
	patchRuntime(t)
	// Avoid touching the real network/filesystem beyond what seams cover.
	origCreate := osCreate
	osCreate = tempFileCreate(t)
	defer func() { osCreate = origCreate }()

	origCmd := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd { return origCmd("true") }
	defer func() { execCommand = origCmd }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: -1} }
	globInputs = func(string) ([]string, error) { return nil, nil }
	osOpen = func(string) (*os.File, error) { return nil, os.ErrNotExist }
	origDiscover := autoDiscover
	autoDiscover = func(context.Context) (string, error) { return srv.URL, nil }
	defer func() { autoDiscover = origDiscover }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run should return after context cancellation")
	}
}
