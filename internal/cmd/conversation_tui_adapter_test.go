package cmd

import (
	"testing"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/tui"
)

func TestTUIEventAdapterMapsDisplayEventsAndDropsUnknownData(t *testing.T) {
	tests := []struct {
		name     string
		typeName string
		content  string
		want     tui.EventKind
	}{
		{"finished", string(agent.EventFinished), "", tui.EventTurnFinished},
		{"failure", string(agent.EventError), "failed", tui.EventFailure},
		{"delta", string(agent.EventTextDelta), "token", tui.EventAssistantDelta},
		{"tool start", string(agent.EventToolCall), "", tui.EventActivityStarted},
		{"tool result", string(agent.EventToolResult), "result", tui.EventActivityCompleted},
		{"reasoning", string(agent.EventReasoning), "thought", tui.EventReasoning},
		{"complete", string(agent.EventTextComplete), "answer", tui.EventAssistantComplete},
		{"compaction", string(agent.EventCompactionStart), "", tui.EventCompactionStatus},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := protocol.SessionEventNotification{TurnID: "turn-1", Type: tt.typeName, Content: tt.content}
			if tt.typeName == string(agent.EventToolCall) || tt.typeName == string(agent.EventToolResult) {
				msg.Data = map[string]any{
					"tool_name": "read_file", "path": "internal/main.go", "line": float64(4),
					"output_ref": "output-1", "activity_id": map[string]any{"turn_id": "turn-1", "step": float64(2), "tool_call_id": "call-1"},
					"command": "must not enter UI event",
				}
			}
			got, ok := tuiEventFromProtocol(msg)
			if !ok || got.Kind != tt.want || got.TurnID != "turn-1" {
				t.Fatalf("mapped event = %+v, %v; want kind %v", got, ok, tt.want)
			}
			if (tt.typeName == string(agent.EventToolCall) || tt.typeName == string(agent.EventToolResult)) && got.Activity != nil {
				if got.Activity.Tool != "read_file" || got.Activity.Path != "internal/main.go" || got.Activity.OutputRef != "output-1" {
					t.Fatalf("activity was not reduced to allowlisted display fields: %+v", got.Activity)
				}
			}
			if tt.typeName == string(agent.EventToolResult) && got.Text != "result" {
				t.Fatalf("tool output text = %q", got.Text)
			}
		})
	}
	unknown, ok := tuiEventFromProtocol(protocol.SessionEventNotification{Type: "future_unknown", Content: "ignore", Data: map[string]any{"secret": "drop"}})
	if ok || unknown.Kind != 0 || unknown.TurnID != "" || unknown.Text != "" || unknown.Activity != nil {
		t.Fatalf("unknown protocol event was not ignored: %+v, %v", unknown, ok)
	}
}

func TestTUIEventAdapterUsesBoundedToolResultOverride(t *testing.T) {
	msg := protocol.SessionEventNotification{TurnID: "turn-1", Type: string(agent.EventToolResult), Content: "fallback", Data: map[string]any{"tool_result": "safe output"}}
	got, ok := tuiEventFromProtocol(msg)
	if !ok || got.Text != "safe output" {
		t.Fatalf("result override = %+v, %v", got, ok)
	}
}
