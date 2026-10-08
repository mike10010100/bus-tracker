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
	globInputs         = filepath.Glob
	osOpen             = os.Open
	osReadFile         = os.ReadFile
	osWriteFile        = os.WriteFile
	osRename           = os.Rename
	osRemove           = os.Remove
	osChmod            = os.Chmod
	osCreate           = os.Create
	osOpenFile         = os.OpenFile
	autoDiscover       = func(ctx context.Context) (string, error) { return AutoDiscoverServer(ctx) }

	netInterfaces    = net.Interfaces
	verifyServerFn   = verifyServer
	saveServerURL    = SaveServerURL
	discoverViaUDP   = DiscoverViaUDP
	discoverViaSweep = DiscoverViaSubnetSweep
)
