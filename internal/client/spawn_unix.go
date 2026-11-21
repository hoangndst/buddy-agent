//go:build !windows

package client

import (
	"os/exec"
	"syscall"
)

// SetDetachedProcess configures OS-specific process attributes to detach from current session.
func SetDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
