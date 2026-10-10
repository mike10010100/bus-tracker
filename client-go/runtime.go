package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Process, filesystem, and network seams. Production code uses the real
// implementations; tests override these package-level hooks to exercise
// OS-coupled logic (OTA download/exec, input device discovery, LAN
// rediscovery) without a Kindle or a live network.
var (
	execCommand        = exec.Command
	execCommandContext = exec.CommandContext
	sysExec            = syscall.Exec
	syscallDup2        = syscall.Dup2
	globInputs         = filepath.Glob
	osOpen             = os.Open
	osReadFile         = os.ReadFile
	osWriteFile        = os.WriteFile
	osRename           = os.Rename
	osRemove           = os.Remove
	osRemoveAll        = os.RemoveAll
	osChmod            = os.Chmod
	osCreate           = os.Create
	osCreateTemp       = os.CreateTemp
	osOpenFile         = os.OpenFile
	osMkdirAll         = os.MkdirAll
	osLstat            = os.Lstat
	osExecutable       = os.Executable
	osGetuid           = os.Getuid
	autoDiscover       = func(ctx context.Context) (string, error) { return AutoDiscoverServer(ctx) }

	netInterfaces    = net.Interfaces
	verifyServerFn   = verifyServer
	saveServerURL    = SaveServerURL
	discoverViaUDP   = DiscoverViaUDP
	discoverViaSweep = DiscoverViaSubnetSweep
)
