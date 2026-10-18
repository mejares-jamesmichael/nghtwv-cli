//go:build linux

package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// startDetached runs the service in its own session with nil stdio (exec
// maps nil files to /dev/null). Setsid keeps the shell's process group
// from signalling the service, and context.WithoutCancel keeps the child
// alive when the spawning invocation exits — the same contract as mpv's
// detached spawn.
func startDetached(ctx context.Context, exe string, args ...string) (*os.Process, error) {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start nghtwv service: %w", err)
	}
	return cmd.Process, nil
}
