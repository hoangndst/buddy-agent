//go:build windows

package client

import (
	"os/exec"
	"syscall"
)

// SetDetachedProcess configures OS-specific process attributes to detach from current session.
func SetDetachedProcess(cmd *exec.Cmd) {
	// 0x00000008 corresponds to DETACHED_PROCESS on Windows
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000008,
	}
}
