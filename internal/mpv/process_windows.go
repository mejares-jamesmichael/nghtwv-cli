//go:build windows

package mpv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// startCommand launches a detached mpv: a fresh process group with no
// console (parity plan: DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP) and a
// hidden window; nil stdio maps to /dev/null. Values inherit from the
// parent context; cancellation does not — the child outlives every
// short-lived invocation.
func startCommand(ctx context.Context, args []string) (*os.Process, error) {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "mpv", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mpv: %w", err)
	}
	return cmd.Process, nil
}
