package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

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

func TestFetchAndDrawDashboard_InteractiveOverride(t *testing.T) {
	patchRuntime(t)
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("X-Kindle-Poll-Interval", "600")
		w.WriteHeader(http.StatusOK)
		w.Write(png)
	}))
	defer srv.Close()

	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: 88} }
	osCreate = tempFileCreate(t)

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Default (not interacting): no override, server decides the face.
	tc.fetchAndDrawDashboard(ctx, cancel)
	if strings.Contains(gotQuery, "present=interactive") {
		t.Errorf("non-interacting fetch must not force interactive, got %q", gotQuery)
	}

	// In a session: request the full tappable dashboard.
	tc.setInteracting(true)
	if !tc.isInteracting() {
		t.Fatal("setInteracting(true) should stick")
	}
	tc.fetchAndDrawDashboard(ctx, cancel)
	if !strings.Contains(gotQuery, "present=interactive") {
		t.Errorf("interacting fetch must request present=interactive, got %q", gotQuery)
	}
	tc.setInteracting(false)
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

func TestFetchAndDrawDashboard_AppliesLightingHeaders(t *testing.T) {
	patchRuntime(t)
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kindle-Poll-Interval", "600")
		w.Header().Set("X-Kindle-Brightness", "8")
		w.Header().Set("X-Kindle-Warmth", "12")
		w.WriteHeader(http.StatusOK)
		w.Write(png)
	}))
	defer srv.Close()
	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: 88} }
	osCreate = tempFileCreate(t)

	var setProps []string
	orig := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd {
		if name == "lipc-get-prop" {
			// Report a different current value so the set branches run.
			return orig("echo", "0")
		}
		if name == "lipc-set-prop" && len(arg) >= 4 {
			setProps = append(setProps, arg[2])
		}
		return orig("true")
	}
	defer func() { execCommand = orig }()

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc.fetchAndDrawDashboard(ctx, cancel)

	joined := strings.Join(setProps, ",")
	if !strings.Contains(joined, "flIntensity") || !strings.Contains(joined, "schedAmberLevel") {
		t.Errorf("expected lighting props to be set, got %v", setProps)
	}
}

func TestFetchAndDrawDashboard_RunsRequestedAction(t *testing.T) {
	patchRuntime(t)
	png := []byte{0x89, 0x50, 0x4E, 0x47}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tracker-Action", "framework-state")
		w.Header().Set("X-Kindle-Poll-Interval", "600")
		w.WriteHeader(http.StatusOK)
		w.Write(png)
	}))
	defer srv.Close()
	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: 88} }
	osCreate = tempFileCreate(t)

	var ranAction bool
	origCtxCmd := execCommandContext
	execCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		ranAction = true
		return origCtxCmd(ctx, "echo", "state")
	}
	defer func() { execCommandContext = origCtxCmd }()

	tc := NewTrackerClient(srv.URL, "auto")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tc.fetchAndDrawDashboard(ctx, cancel)
	if !ranAction {
		t.Error("expected the requested device action to run")
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

func TestRunPollLoop_CleansUpOnCancel(t *testing.T) {
	patchRuntime(t)
	GetBatteryInfo = func() BatteryInfo { return BatteryInfo{Level: -1} }
	osCreate = tempFileCreate(t)
	var restartedFramework, blanked bool
	orig := execCommand
	execCommand = func(name string, arg ...string) *exec.Cmd {
		if name == "start" && len(arg) > 0 && arg[0] == "lab126_gui" {
			restartedFramework = true
		}
		if name == "eips" && len(arg) > 0 && arg[0] == "-c" {
			blanked = true
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
	if !restartedFramework {
		t.Error("expected cleanup to restart lab126_gui on loop exit")
	}
	if blanked {
		t.Error("cleanup must not blank the panel (breaks framework-stopped devices)")
	}
}

func TestMaybeUpdateBinary_ErrorBranches(t *testing.T) {
	patchRuntime(t)
	tc := NewTrackerClient("http://127.0.0.1:1", "auto")

	if tc.maybeUpdateBinary(context.Background(), "", "sha") {
		t.Error("expected false for empty version")
	}

	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv404.Close()
	tc404 := NewTrackerClient(srv404.URL, "auto")
	if tc404.maybeUpdateBinary(context.Background(), "99.0.0", "sha") {
		t.Error("expected false when download returns 404")
	}

	srv200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("binary-data"))
	}))
	defer srv200.Close()
	tc200 := NewTrackerClient(srv200.URL, "auto")

	osOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		return nil, os.ErrPermission
	}
	if tc200.maybeUpdateBinary(context.Background(), "99.0.0", "sha") {
		t.Error("expected false when osOpenFile fails")
	}

	osOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		return os.CreateTemp(t.TempDir(), "bin-*")
	}
	if tc200.maybeUpdateBinary(context.Background(), "99.0.0", "wrong-sha") {
		t.Error("expected false on SHA mismatch")
	}

	if tc200.maybeUpdateBinary(context.Background(), "99.0.0", "") {
		t.Error("expected false when no SHA provided")
	}
}

func TestFetchAndDrawDashboard_ErrorBranches(t *testing.T) {
	patchRuntime(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tracker-Action", "totally-unknown-action")
		w.Header().Set("X-Tracker-Mode", "sleep")
		w.Header().Set("X-Kindle-Poll-Interval", "60")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not-an-image"))
	}))
	defer srv.Close()

	tc := NewTrackerClient(srv.URL, "auto")
	origExec := sysExec
	sysExec = func(argv0 string, argv []string, envv []string) error {
		return os.ErrPermission
	}
	defer func() { sysExec = origExec }()

	osCreate = func(name string) (*os.File, error) {
		return nil, os.ErrPermission
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poll := tc.fetchAndDrawDashboard(ctx, cancel)
	if poll != 60 {
		t.Fatalf("expected poll interval 60, got %d", poll)
	}

	srv304 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tracker-Version", "99.0.0")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv304.Close()
	tc304 := NewTrackerClient(srv304.URL, "auto")
	osOpenFile = func(string, int, os.FileMode) (*os.File, error) { return nil, os.ErrPermission }
	res304 := tc304.fetchAndDrawDashboard(ctx, cancel)
	if res304 != 0 {
		t.Logf("304 response result: %d", res304)
	}
}
