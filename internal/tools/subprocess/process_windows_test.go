//go:build windows

package subprocess

import (
	"context"
	"testing"
	"time"
)

func TestProcess_WindowsJobObjectCleanup(t *testing.T) {
	runner := NewDefaultRunner(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	res, err := runner.Run(ctx, ExecutionOptions{
		Command:        "powershell -Command Start-Sleep -Seconds 30",
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
