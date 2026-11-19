package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hoangndst/buddy-agent/internal/tools/subprocess"
)

// BashToolOption configures a BashTool.
type BashToolOption func(*BashTool)

// WithRunner sets the subprocess.Runner for the BashTool.
func WithRunner(r subprocess.Runner) BashToolOption {
	return func(b *BashTool) {
		if r != nil {
			b.runner = r
		}
	}
}

// WithMaxOutputBytes sets the maximum byte output limit for the BashTool.
func WithMaxOutputBytes(n int) BashToolOption {
	return func(b *BashTool) {
		if n > 0 {
			b.maxOutputBytes = n
		}
	}
}

// BashTool executes shell commands in the agent's workspace.
type BashTool struct {
	workspace      string
	timeout        time.Duration
	maxOutputBytes int
	runner         subprocess.Runner
}

// NewBashTool creates a new BashTool with optional configuration options.
func NewBashTool(workspace string, timeout time.Duration, opts ...BashToolOption) *BashTool {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	b := &BashTool{
		workspace:      workspace,
		timeout:        timeout,
		maxOutputBytes: 10000,
		runner:         subprocess.NewDefaultRunner(1500 * time.Millisecond),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Name returns the identifier of the tool.
func (b *BashTool) Name() string {
	return "bash"
}

// Description returns a human-readable explanation of the tool.
func (b *BashTool) Description() string {
	return "Execute a bash shell command in the workspace directory. Returns exit code, duration, stdout, and stderr."
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

	// Reserve 256 bytes for execution summary headers so res.Combined <= b.maxOutputBytes
	runnerMaxBytes := b.maxOutputBytes - 256
	if runnerMaxBytes < 500 {
		runnerMaxBytes = 500
	}

	res, err := b.runner.Run(ctx, subprocess.ExecutionOptions{
		Command:        input.Command,
		Dir:            b.workspace,
		Timeout:        b.timeout,
		MaxOutputBytes: runnerMaxBytes,
	})
	if err != nil {
		return "", fmt.Errorf("command execution failed: %w", err)
	}

	return res.Combined, nil
}
