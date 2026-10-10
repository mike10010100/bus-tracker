package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

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
