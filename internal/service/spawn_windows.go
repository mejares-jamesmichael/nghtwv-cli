//go:build windows

package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// startDetached runs the service in its own process group with no console
// and a hidden window; nil stdio maps to devnull. Cancellation does not
// propagate — the service outlives the invocation that started it (the
// same flags as mpv's detached spawn).
func startDetached(ctx context.Context, exe string, args ...string) (*os.Process, error) {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start nghtwv service: %w", err)
	}
	return cmd.Process, nil
}
