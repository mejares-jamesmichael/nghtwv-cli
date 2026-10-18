package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ServiceVerb is the hidden subcommand that runs the service in the
// foreground; detached spawns point at it. It is intentionally absent from
// the usage text — users reach the service through the player commands.
const ServiceVerb = "service"

// startService launches this binary's service verb as a detached child:
// stdio goes to the void and cancellation does not propagate, because the
// service must outlive the short-lived client invocation that started it.
func startService(ctx context.Context) (Process, error) {
	exe, err := selfCommand()
	if err != nil {
		return nil, err
	}
	proc, err := startDetached(ctx, exe, ServiceVerb)
	if err != nil {
		return nil, err
	}
	return proc, nil
}

// selfCommand resolves the binary to spawn. os.Args[0] wins over
// os.Executable: after an in-place upgrade the running image can be an
// unlinked inode (Linux /proc/self/exe), while the invoked path is the new
// binary — and a new client wants a new service.
func selfCommand() (string, error) {
	name := os.Args[0]
	if name != "" {
		if strings.ContainsAny(name, `/\`) {
			if abs, err := filepath.Abs(name); err == nil {
				return abs, nil
			}
		} else if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return os.Executable()
}
