// Package subprocess provides process group lifecycle supervision, dual-phase termination,
// and memory-bounded streaming output truncation for agent tools.
package subprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// ExecutionResult encapsulates the outcome of a supervised command execution.
type ExecutionResult struct {
	ExitCode      int           // Process exit code (0 for success, non-zero for command failures, -1 for terminated)
	Stdout        string        // Line-aware truncated standard output
	Stderr        string        // Line-aware truncated standard error
	Combined      string        // Formatted summary string containing exit status, duration, and output
	Terminated    bool          // True if process was terminated by timeout or context cancellation
	EscalatedKill bool          // True if process group required SIGKILL escalation
	Duration      time.Duration // Wall-clock execution duration
	OmittedLines  int           // Total lines omitted across stdout and stderr
	OmittedBytes  int64         // Total bytes omitted across stdout and stderr
}

// ExecutionOptions specifies execution parameters for the subprocess runner.
type ExecutionOptions struct {
	Command        string        // Command string to execute via shell
	Dir            string        // Working directory for execution
	Timeout        time.Duration // Maximum execution duration before triggering termination
	GracePeriod    time.Duration // Grace period between SIGTERM and SIGKILL (defaults to 1500ms)
	MaxOutputBytes int           // Maximum byte capacity for output (defaults to 10000)
	Env            []string      // Optional environment variables (nil = inherit os.Environ())
}

// Runner defines the contract for supervised subprocess execution.
type Runner interface {
	Run(ctx context.Context, opts ExecutionOptions) (*ExecutionResult, error)
}

// Supervisor defines the internal cross-platform OS process management contract.
type Supervisor interface {
	PrepareCmd(cmd *exec.Cmd) error
	AttachProcess(cmd *exec.Cmd) error
	Terminate(graceful bool) error
	IsAlive() bool
	Close() error
}

// NewSupervisor instantiates a platform-specific OS process lifecycle supervisor.
func NewSupervisor() Supervisor {
	return newPlatformSupervisor()
}

// DefaultRunner implements Runner using OS process groups and streaming truncation.
type DefaultRunner struct {
	defaultGracePeriod time.Duration
}

// NewDefaultRunner creates a DefaultRunner with a default grace period.
func NewDefaultRunner(gracePeriod time.Duration) *DefaultRunner {
	if gracePeriod <= 0 {
		gracePeriod = 1500 * time.Millisecond
	}
	return &DefaultRunner{
		defaultGracePeriod: gracePeriod,
	}
}

// Run executes the command in an isolated process group with streaming output truncation.
func (r *DefaultRunner) Run(ctx context.Context, opts ExecutionOptions) (*ExecutionResult, error) {
	if opts.Command == "" {
		return nil, errors.New("command argument cannot be empty")
	}

	maxBytes := opts.MaxOutputBytes
	if maxBytes < 500 {
		maxBytes = 10000
	}

	stdoutBudget := (maxBytes * 7) / 10
	stderrBudget := maxBytes - stdoutBudget

	stdoutTruncator := NewStreamingLineTruncator(stdoutBudget)
	stderrTruncator := NewStreamingLineTruncator(stderrBudget)

	var execCtx context.Context
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	} else {
		execCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	shellBin, shellArgs := buildShellCommand(opts.Command)
	cmd := exec.Command(shellBin, shellArgs...)
	cmd.Dir = opts.Dir
	cmd.Stdin = nil
	cmd.Stdout = stdoutTruncator
	cmd.Stderr = stderrTruncator
	cmd.WaitDelay = 500 * time.Millisecond

	if opts.Env != nil {
		cmd.Env = opts.Env
	} else {
		cmd.Env = os.Environ()
	}

	sup := NewSupervisor()
	defer func() {
		_ = sup.Close()
	}()

	if err := sup.PrepareCmd(cmd); err != nil {
		return nil, fmt.Errorf("failed to prepare command: %w", err)
	}

	startTime := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("command execution failed to start: %w", err)
	}

	if err := sup.AttachProcess(cmd); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("failed to attach process group: %w", err)
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var exitCode int
	var terminated bool
	var escalatedKill bool
	var waitErr error

	select {
	case waitErr = <-waitDone:
		// Normal completion before timeout/cancellation
	case <-execCtx.Done():
		terminated = true
		_ = sup.Terminate(true) // Phase 1: Graceful SIGTERM

		gracePeriod := opts.GracePeriod
		if gracePeriod <= 0 {
			gracePeriod = r.defaultGracePeriod
		}
		graceTimer := time.NewTimer(gracePeriod)
		defer graceTimer.Stop()

		select {
		case waitErr = <-waitDone:
			// Leader exited. Check if background child descendants are still alive
			if sup.IsAlive() {
				ticker := time.NewTicker(25 * time.Millisecond)
				defer ticker.Stop()
				escalated := false
			descendantLoop:
				for {
					select {
					case <-graceTimer.C:
						_ = sup.Terminate(false) // Phase 2: Forced SIGKILL
						escalatedKill = true
						escalated = true
						break descendantLoop
					case <-ticker.C:
						if !sup.IsAlive() {
							break descendantLoop
						}
					}
				}
				if escalated {
					time.Sleep(10 * time.Millisecond)
				}
			}
		case <-graceTimer.C:
			// Grace period expired without leader exit; escalate to forced SIGKILL
			_ = sup.Terminate(false)
			escalatedKill = true
			waitErr = <-waitDone
		}
	}

	if waitErr != nil {
		if errors.Is(waitErr, exec.ErrWaitDelay) {
			_ = sup.Terminate(false)
			exitCode = -1
			terminated = true
		} else if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if terminated {
			exitCode = -1
		} else {
			return nil, fmt.Errorf("command execution failed: %w", waitErr)
		}
	} else {
		exitCode = 0
	}

	duration := time.Since(startTime)
	stdoutStr := stdoutTruncator.String()
	stderrStr := stderrTruncator.String()
	omittedLines := stdoutTruncator.OmittedLines() + stderrTruncator.OmittedLines()
	omittedBytes := stdoutTruncator.OmittedBytes() + stderrTruncator.OmittedBytes()

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Exit Code: %d\n", exitCode))
	if terminated {
		if escalatedKill {
			b.WriteString("Status: Terminated (Timeout / Cancelled, SIGKILL Escalated)\n")
		} else {
			b.WriteString("Status: Terminated (Timeout / Cancelled, Graceful Exit)\n")
		}
	}
	b.WriteString(fmt.Sprintf("Duration: %s\n", duration.Round(time.Millisecond)))
	b.WriteString("Stdout:\n")
	if stdoutStr == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(stdoutStr)
		if !strings.HasSuffix(stdoutStr, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString("Stderr:\n")
	if stderrStr == "" {
		b.WriteString("(empty)\n")
	} else {
		b.WriteString(stderrStr)
		if !strings.HasSuffix(stderrStr, "\n") {
			b.WriteString("\n")
		}
	}

	return &ExecutionResult{
		ExitCode:      exitCode,
		Stdout:        stdoutStr,
		Stderr:        stderrStr,
		Combined:      b.String(),
		Terminated:    terminated,
		EscalatedKill: escalatedKill,
		Duration:      duration,
		OmittedLines:  omittedLines,
		OmittedBytes:  omittedBytes,
	}, nil
}

func buildShellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("bash.exe"); err == nil && p != "" {
			return p, []string{"-c", command}
		}
		if p, err := exec.LookPath("powershell.exe"); err == nil && p != "" {
			return p, []string{"-Command", command}
		}
		return "cmd.exe", []string{"/c", command}
	}
	if p, err := exec.LookPath("bash"); err == nil && p != "" {
		return p, []string{"-c", command}
	}
	return "sh", []string{"-c", command}
}
