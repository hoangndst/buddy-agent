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

func newPlatformSupervisor() platformSupervisor {
	return &posixSupervisor{}
}

func (s *posixSupervisor) prepareCmd(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}

func (s *posixSupervisor) attachProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil || cmd.Process.Pid <= 1 {
		return fmt.Errorf("invalid process pid: %d", cmd.Process.Pid)
	}
	s.pgid = cmd.Process.Pid
	return nil
}

func (s *posixSupervisor) terminate(graceful bool) error {
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

func (s *posixSupervisor) isAlive() bool {
	if s.pgid <= 1 {
		return false
	}
	err := syscall.Kill(-s.pgid, 0)
	return err == nil
}

func (s *posixSupervisor) close() error {
	return nil
}
