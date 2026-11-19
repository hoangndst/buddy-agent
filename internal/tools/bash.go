package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// BashTool executes shell commands in the agent's workspace.
type BashTool struct {
	workspace string
	timeout   time.Duration
}

// NewBashTool creates a new BashTool.
func NewBashTool(workspace string, timeout time.Duration) *BashTool {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &BashTool{
		workspace: workspace,
		timeout:   timeout,
	}
}

// Name returns the identifier of the tool.
func (b *BashTool) Name() string {
	return "bash"
}

// Description returns a human-readable explanation of the tool.
func (b *BashTool) Description() string {
	return "Execute a bash shell command in the workspace directory. Returns exit code, stdout, and stderr."
}

// Parameters returns the JSON schema definition for tool arguments.
func (b *BashTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The bash command to run.",
			},
		},
		"required": []string{"command"},
	}
}

type bashInput struct {
	Command string `json:"command"`
}

// Execute runs the specified bash command with timeout in the workspace.
func (b *BashTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var input bashInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", fmt.Errorf("invalid bash tool arguments: %w", err)
	}

	if input.Command == "" {
		return "", errors.New("command argument cannot be empty")
	}

	execCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "bash", "-c", input.Command)
	cmd.Dir = b.workspace

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return "", fmt.Errorf("command execution failed: %w", err)
		}
	}

	return fmt.Sprintf("Exit Code: %d\nStdout:\n%s\nStderr:\n%s",
		exitCode, stdout.String(), stderr.String()), nil
}
