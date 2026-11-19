//go:build windows

package subprocess

import (
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobObjectBasicAccountingInformation mirrors Win32 JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

const jobObjectBasicAccountingInformationClass = 1

type windowsSupervisor struct {
	jobHandle windows.Handle
	closeOnce sync.Once
}

func newPlatformSupervisor() platformSupervisor {
	return &windowsSupervisor{}
}

func (s *windowsSupervisor) prepareCmd(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
	return nil
}

func (s *windowsSupervisor) attachProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return fmt.Errorf("invalid process pid: %d", cmd.Process.Pid)
	}

	hJob, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject failed: %w", err)
	}
	s.jobHandle = hJob

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		hJob,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		_ = windows.CloseHandle(hJob)
		s.jobHandle = 0
		return fmt.Errorf("SetInformationJobObject failed: %w", err)
	}

	hProcess, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(hJob)
		s.jobHandle = 0
		return fmt.Errorf("OpenProcess failed: %w", err)
	}
	defer windows.CloseHandle(hProcess)

	if err := windows.AssignProcessToJobObject(hJob, hProcess); err != nil {
		_ = windows.CloseHandle(hJob)
		s.jobHandle = 0
		return fmt.Errorf("AssignProcessToJobObject failed: %w", err)
	}

	return nil
}

func (s *windowsSupervisor) terminate(graceful bool) error {
	if s.jobHandle != 0 {
		_ = windows.TerminateJobObject(s.jobHandle, 1)
	}
	return nil
}

func (s *windowsSupervisor) isAlive() bool {
	if s.jobHandle == 0 {
		return false
	}
	var basicInfo jobObjectBasicAccountingInformation
	err := windows.QueryInformationJobObject(
		s.jobHandle,
		jobObjectBasicAccountingInformationClass,
		uintptr(unsafe.Pointer(&basicInfo)),
		uint32(unsafe.Sizeof(basicInfo)),
		nil,
	)
	if err != nil {
		return false
	}
	return basicInfo.ActiveProcesses > 0
}

func (s *windowsSupervisor) close() error {
	s.closeOnce.Do(func() {
		if s.jobHandle != 0 {
			_ = windows.CloseHandle(s.jobHandle)
			s.jobHandle = 0
		}
	})
	return nil
}
