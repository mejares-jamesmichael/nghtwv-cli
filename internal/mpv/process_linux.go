//go:build linux

package mpv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// startCommand launches a detached mpv: Setsid so the shell's process
// group never receives its signals (parity: start_new_session=True), and
// nil stdio, which exec maps to /dev/null (parity: DEVNULL). The parent
// context's values are inherited, cancellation is not — the child must
// outlive every short-lived controller invocation.
func startCommand(ctx context.Context, args []string) (*os.Process, error) {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "mpv", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mpv: %w", err)
	}
	return cmd.Process, nil
}
