//go:build !windows

package subprocess

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

type posixSupervisor struct {
	pgid int
}

func newPlatformSupervisor() Supervisor {
	return &posixSupervisor{}
}

func (s *posixSupervisor) PrepareCmd(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func (s *posixSupervisor) AttachProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil || cmd.Process.Pid <= 1 {
		return fmt.Errorf("invalid process pid: %d", cmd.Process.Pid)
	}
	s.pgid = cmd.Process.Pid
	return nil
}

func (s *posixSupervisor) Terminate(graceful bool) error {
	if s.pgid <= 1 {
		return nil
	}
	sig := syscall.SIGKILL
	if graceful {
		sig = syscall.SIGTERM
	}
	err := syscall.Kill(-s.pgid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (s *posixSupervisor) IsAlive() bool {
	if s.pgid <= 1 {
		return false
	}
	err := syscall.Kill(-s.pgid, 0)
	return err == nil
}

func (s *posixSupervisor) Close() error {
	return nil
}
