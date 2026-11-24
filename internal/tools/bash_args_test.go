package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/tools/subprocess"
)

type captureBashRunner struct {
	command string
	calls   int
}

func (r *captureBashRunner) Run(_ context.Context, opts subprocess.ExecutionOptions) (*subprocess.ExecutionResult, error) {
	r.command = opts.Command
	r.calls++
	return &subprocess.ExecutionResult{Combined: "ok"}, nil
}

func TestParseBashCommandRequiresOneUnambiguousField(t *testing.T) {
	for _, raw := range []string{
		`{"command":"first","command":"second"}`,
		`{"command":"git status","Command":"touch /tmp/unapproved"}`,
		`{"command":"git status","extra":true}`,
		`{"command":1}`,
		`{"command":"git status"} {}`,
		`{}`,
		`{"command":""}`,
	} {
		if _, err := ParseBashCommand(json.RawMessage(raw)); err == nil {
			t.Errorf("ParseBashCommand(%s) unexpectedly succeeded", raw)
		}
	}
	if got, err := ParseBashCommand(json.RawMessage(`{"command":"git status"}`)); err != nil || got != "git status" {
		t.Fatalf("ParseBashCommand = %q, %v", got, err)
	}
}

func TestBashToolRejectsAmbiguousArgumentsBeforeExecution(t *testing.T) {
	runner := &captureBashRunner{}
	bash := NewBashTool(t.TempDir(), time.Second, WithRunner(runner))
	for _, raw := range []string{
		`{"command":"git status","Command":"touch /tmp/unapproved"}`,
		`{"command":"git status","command":"touch /tmp/unapproved"}`,
	} {
		if _, err := bash.Execute(context.Background(), json.RawMessage(raw)); err == nil {
			t.Errorf("BashTool.Execute(%s) unexpectedly succeeded", raw)
		}
	}
	if runner.calls != 0 {
		t.Fatalf("ambiguous arguments reached runner %d times", runner.calls)
	}
	if _, err := bash.Execute(context.Background(), json.RawMessage(`{"command":"git status"}`)); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 || runner.command != "git status" {
		t.Fatalf("canonical command execution = calls:%d command:%q", runner.calls, runner.command)
	}
}
