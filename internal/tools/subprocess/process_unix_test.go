//go:build !windows

package subprocess

import (
	"context"
	"testing"
	"time"
)

func TestProcess_OrphanCleanup(t *testing.T) {
	runner := NewDefaultRunner(200 * time.Millisecond)

	// Command spawns background sleep processes
	cmd := "sleep 30 & sleep 31 & sleep 32 & sleep 1"
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	res, err := runner.Run(ctx, ExecutionOptions{
		Command:        cmd,
		Timeout:        100 * time.Millisecond,
		GracePeriod:    200 * time.Millisecond,
		MaxOutputBytes: 5000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.Terminated {
		t.Errorf("expected command to be terminated by timeout")
	}
}

func TestProcess_GracefulEscalationToKill(t *testing.T) {
	runner := NewDefaultRunner(300 * time.Millisecond)

	// Command explicitly traps SIGTERM and ignores it
	cmd := "trap '' SIGTERM; sleep 5"
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	startTime := time.Now()
	res, err := runner.Run(ctx, ExecutionOptions{
		Command:        cmd,
		Timeout:        100 * time.Millisecond,
		GracePeriod:    300 * time.Millisecond,
		MaxOutputBytes: 5000,
	})
	elapsed := time.Since(startTime)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.Terminated {
		t.Errorf("expected command to be terminated")
	}
	if !res.EscalatedKill {
		t.Errorf("expected command to require escalated SIGKILL")
	}

	if elapsed < 350*time.Millisecond {
		t.Errorf("expected elapsed >= 350ms for escalation, got: %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("expected elapsed < 2s for kill escalation, got: %v", elapsed)
	}
}
