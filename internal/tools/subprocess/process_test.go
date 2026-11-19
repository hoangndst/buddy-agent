package subprocess

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestProcess_NormalExecution(t *testing.T) {
	runner := NewDefaultRunner(1500 * time.Millisecond)
	ctx := context.Background()

	res, err := runner.Run(ctx, ExecutionOptions{
		Command:        "echo 'hello from subprocess'",
		Timeout:        5 * time.Second,
		MaxOutputBytes: 10000,
	})
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}

	if res.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", res.ExitCode)
	}
	if res.Terminated {
		t.Errorf("expected Terminated to be false")
	}
	if !strings.Contains(res.Stdout, "hello from subprocess") {
		t.Errorf("expected stdout to contain message, got: %q", res.Stdout)
	}
	if !strings.Contains(res.Combined, "Exit Code: 0") {
		t.Errorf("expected Combined to contain Exit Code: 0, got:\n%s", res.Combined)
	}
}

func TestProcess_NonZeroExitCode(t *testing.T) {
	runner := NewDefaultRunner(1500 * time.Millisecond)
	ctx := context.Background()

	res, err := runner.Run(ctx, ExecutionOptions{
		Command:        "echo 'err message' >&2; exit 42",
		Timeout:        5 * time.Second,
		MaxOutputBytes: 10000,
	})
	if err != nil {
		t.Fatalf("expected nil error for valid non-zero exit, got: %v", err)
	}

	if res.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", res.ExitCode)
	}
	if res.Terminated {
		t.Errorf("expected Terminated to be false")
	}
	if !strings.Contains(res.Stderr, "err message") {
		t.Errorf("expected stderr to contain error message, got: %q", res.Stderr)
	}
	if !strings.Contains(res.Combined, "Exit Code: 42") {
		t.Errorf("expected Combined to contain Exit Code: 42, got:\n%s", res.Combined)
	}
}

func TestProcess_ClosedStdin(t *testing.T) {
	runner := NewDefaultRunner(1500 * time.Millisecond)
	ctx := context.Background()

	// An interactive read command should fail immediately with EOF rather than hanging
	res, err := runner.Run(ctx, ExecutionOptions{
		Command:        "read line || echo 'EOF received'",
		Timeout:        3 * time.Second,
		MaxOutputBytes: 10000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.Terminated {
		t.Errorf("expected command not to time out on closed stdin")
	}
	if !strings.Contains(res.Stdout, "EOF received") {
		t.Errorf("expected stdout to contain 'EOF received', got: %q", res.Stdout)
	}
}

func TestProcess_StreamingMemoryBound(t *testing.T) {
	runner := NewDefaultRunner(1500 * time.Millisecond)
	ctx := context.Background()

	// Emit rapid lines for 300ms against a 1KB limit
	res, err := runner.Run(ctx, ExecutionOptions{
		Command:        "for i in $(seq 1 1000); do echo \"line $i\"; done",
		Timeout:        5 * time.Second,
		MaxOutputBytes: 1000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(res.Stdout) > 1000 {
		t.Fatalf("stdout length %d exceeds maxOutputBytes 1000", len(res.Stdout))
	}
	if !strings.Contains(res.Stdout, "[... truncated:") {
		t.Errorf("expected omission marker in stdout, got:\n%s", res.Stdout)
	}
}
