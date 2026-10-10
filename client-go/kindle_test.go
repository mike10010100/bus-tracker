package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLipcSet_Table(t *testing.T) {
	patchRuntime(t)

	tests := []struct {
		name     string
		prop     string
		key      string
		val      string
		wantArgs []string
	}{
		{
			name:     "powerd_screensaver_off",
			prop:     "com.lab126.powerd",
			key:      "preventScreenSaver",
			val:      "0",
			wantArgs: []string{"-i", "com.lab126.powerd", "preventScreenSaver", "0"},
		},
		{
			name:     "frontlight_intensity",
			prop:     "com.lab126.powerd",
			key:      "flIntensity",
			val:      "18",
			wantArgs: []string{"-i", "com.lab126.powerd", "flIntensity", "18"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var recordedCmd string
			var recordedArgs []string

			execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				recordedCmd = name
				recordedArgs = args
				return exec.CommandContext(ctx, "true")
			}

			lipcSet(tt.prop, tt.key, tt.val)

			if recordedCmd != "lipc-set-prop" {
				t.Errorf("got cmd %q, want lipc-set-prop", recordedCmd)
			}
			if len(recordedArgs) != len(tt.wantArgs) {
				t.Fatalf("got args %v, want %v", recordedArgs, tt.wantArgs)
			}
			for i := range tt.wantArgs {
				if recordedArgs[i] != tt.wantArgs[i] {
					t.Errorf("arg[%d] = %q, want %q", i, recordedArgs[i], tt.wantArgs[i])
				}
			}
		})
	}
}

func TestLipcSet_TimeoutTerminates(t *testing.T) {
	patchRuntime(t)

	origTimeout := lipcCallTimeout
	lipcCallTimeout = 20 * time.Millisecond
	defer func() { lipcCallTimeout = origTimeout }()

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "2")
	}

	start := time.Now()
	lipcSet("com.lab126.powerd", "preventScreenSaver", "0")
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Errorf("lipcSet took %v, expected timeout around 20ms", elapsed)
	}
}

func TestLipcGet_Table(t *testing.T) {
	patchRuntime(t)

	tests := []struct {
		name       string
		mockOutput string
		mockErr    bool
		want       string
	}{
		{
			name:       "trimmed_value",
			mockOutput: "  18 \n",
			want:       "18",
		},
		{
			name:       "empty_value",
			mockOutput: "",
			want:       "",
		},
		{
			name:    "command_failure",
			mockErr: true,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if tt.mockErr {
					return exec.CommandContext(ctx, "false")
				}
				return exec.CommandContext(ctx, "echo", tt.mockOutput)
			}

			got := lipcGet("com.lab126.powerd", "flIntensity")
			if got != tt.want {
				t.Errorf("lipcGet() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLipcGet_TimeoutTerminates(t *testing.T) {
	patchRuntime(t)

	origTimeout := lipcCallTimeout
	lipcCallTimeout = 20 * time.Millisecond
	defer func() { lipcCallTimeout = origTimeout }()

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "2")
	}

	start := time.Now()
	got := lipcGet("com.lab126.powerd", "flIntensity")
	elapsed := time.Since(start)

	if got != "" {
		t.Errorf("expected empty string on timeout, got %q", got)
	}
	if elapsed > 1*time.Second {
		t.Errorf("lipcGet took %v, expected timeout around 20ms", elapsed)
	}
}

func TestPrepareDisplayForSleep(t *testing.T) {
	patchRuntime(t)

	var calls []string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return exec.CommandContext(ctx, "true")
	}

	tc := NewTrackerClient("http://127.0.0.1:8000", "auto")
	tc.prepareDisplayForSleep(context.Background())

	if len(calls) != 3 {
		t.Fatalf("expected 3 calls, got %d: %v", len(calls), calls)
	}
	if calls[0] != "stop lab126_gui" {
		t.Errorf("call 0 = %q, want 'stop lab126_gui'", calls[0])
	}
	if calls[1] != "lipc-set-prop com.lab126.blanket unload screensaver" {
		t.Errorf("call 1 = %q", calls[1])
	}
	if calls[2] != "lipc-set-prop com.lab126.blanket unload splash" {
		t.Errorf("call 2 = %q", calls[2])
	}
}
