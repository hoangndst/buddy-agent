package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// WithEnv sets the environment variables for the BashTool subprocess execution.
func WithEnv(env []string) BashToolOption {
	return func(b *BashTool) {
		b.env = env
	}
}

// BashTool executes shell commands in the agent's workspace.
type BashTool struct {
	workspace      string
	timeout        time.Duration
	maxOutputBytes int
	runner         subprocess.Runner
	env            []string
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

// ParseBashCommand accepts exactly one lowercase command field and rejects ambiguous JSON.
func ParseBashCommand(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return "", errors.New("arguments must be an object containing one command field")
	}
	commandSeen := false
	command := ""
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return "", err
		}
		key, ok := token.(string)
		if !ok || key != "command" {
			return "", fmt.Errorf("unexpected bash argument field %q", key)
		}
		if commandSeen {
			return "", errors.New("duplicate bash command field")
		}
		var value any
		if err := dec.Decode(&value); err != nil {
			return "", err
		}
		var okString bool
		command, okString = value.(string)
		if !okString {
			return "", errors.New("bash command field must be a string")
		}
		commandSeen = true
	}
	closing, err := dec.Token()
	if err != nil {
		return "", err
	}
	if closing != json.Delim('}') {
		return "", errors.New("bash arguments object is not closed")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return "", errors.New("unexpected data after bash arguments")
		}
		return "", err
	}
	if !commandSeen {
		return "", errors.New("bash command field is required")
	}
	if command == "" {
		return "", errors.New("command argument cannot be empty")
	}
	return command, nil
}

// Execute runs the specified bash command with timeout in the workspace.
func (b *BashTool) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	command, err := ParseBashCommand(raw)
	if err != nil {
		return "", fmt.Errorf("invalid bash tool arguments: %w", err)
	}

	// Reserve 256 bytes for execution summary headers so res.Combined <= b.maxOutputBytes
	runnerMaxBytes := b.maxOutputBytes - 256
	if runnerMaxBytes < 500 {
		runnerMaxBytes = 500
	}

	res, err := b.runner.Run(ctx, subprocess.ExecutionOptions{
		Command:        command,
		Dir:            b.workspace,
		Timeout:        b.timeout,
		MaxOutputBytes: runnerMaxBytes,
		Env:            b.env,
	})
	if err != nil {
		return "", fmt.Errorf("command execution failed: %w", err)
	}

	return res.Combined, nil
}
