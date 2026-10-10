package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLauncherPath(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "default when no args",
			args: []string{"tracker"},
			want: DefaultLauncherPath,
		},
		{
			name: "space separated -launcher",
			args: []string{"tracker", "-launcher", "/custom/launcher.sh"},
			want: "/custom/launcher.sh",
		},
		{
			name: "space separated --launcher",
			args: []string{"tracker", "--launcher", "/custom/launcher2.sh"},
			want: "/custom/launcher2.sh",
		},
		{
			name: "equals separated -launcher=",
			args: []string{"tracker", "-launcher=/custom/launcher3.sh"},
			want: "/custom/launcher3.sh",
		},
		{
			name: "equals separated --launcher=",
			args: []string{"tracker", "--launcher=/custom/launcher4.sh"},
			want: "/custom/launcher4.sh",
		},
		{
			name: "empty value falls through to default",
			args: []string{"tracker", "-launcher", "   "},
			want: DefaultLauncherPath,
		},
		{
			name: "empty equals value falls through to default",
			args: []string{"tracker", "-launcher="},
			want: DefaultLauncherPath,
		},
		{
			name: "missing next arg falls through to default",
			args: []string{"tracker", "-launcher"},
			want: DefaultLauncherPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveLauncherPath(tt.args)
			if got != tt.want {
				t.Errorf("resolveLauncherPath(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestSelfUpdateLauncher(t *testing.T) {
	patchRuntime(t)
	td := t.TempDir()
	target := filepath.Join(td, "TransitTracker.sh")

	// 1. File does not exist -> no-op, returns nil
	if err := selfUpdateLauncher(target); err != nil {
		t.Fatalf("expected nil when file does not exist, got %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("target should not be created if it did not exist")
	}

	// 2. File exists without marker -> no-op, not overwritten
	customScript := "#!/bin/sh\necho custom\n"
	if err := os.WriteFile(target, []byte(customScript), 0755); err != nil {
		t.Fatal(err)
	}
	if err := selfUpdateLauncher(target); err != nil {
		t.Fatalf("expected nil on missing marker, got %v", err)
	}
	content, _ := os.ReadFile(target)
	if string(content) != customScript {
		t.Fatal("script without marker should not be modified")
	}

	// 3. File exists with marker but different bytes -> successfully updated
	outdatedScript := LauncherMarkerLine + "\n# Outdated version\n"
	if err := os.WriteFile(target, []byte(outdatedScript), 0755); err != nil {
		t.Fatal(err)
	}
	if err := selfUpdateLauncher(target); err != nil {
		t.Fatalf("expected update to succeed, got %v", err)
	}
	updated, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), LauncherMarkerLine) {
		t.Fatal("updated script must contain marker line")
	}
	if len(embeddedLauncher) > 0 && string(updated) != string(embeddedLauncher) {
		t.Fatal("updated script must match embedded launcher")
	}

	// 4. File already identical -> no-op
	if err := selfUpdateLauncher(target); err != nil {
		t.Fatalf("expected nil on identical bytes, got %v", err)
	}

	// 5. Write error
	origWrite := osWriteFile
	osWriteFile = func(path string, data []byte, perm os.FileMode) error {
		return os.ErrPermission
	}
	_ = os.WriteFile(target, []byte(outdatedScript), 0755)
	if err := selfUpdateLauncher(target); err == nil {
		t.Fatal("expected error when osWriteFile fails")
	}
	osWriteFile = origWrite

	// 6. Rename error
	origRename := osRename
	osRename = func(oldpath, newpath string) error {
		return os.ErrPermission
	}
	_ = os.WriteFile(target, []byte(outdatedScript), 0755)
	if err := selfUpdateLauncher(target); err == nil {
		t.Fatal("expected error when osRename fails")
	}
	osRename = origRename
}
