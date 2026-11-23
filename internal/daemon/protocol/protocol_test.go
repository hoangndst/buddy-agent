package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/mcp"
)

func TestConversationRPCShapesRoundTrip(t *testing.T) {
	start := ConversationStartParams{
		StartID: "start-1", SubmissionID: "submission-1", Prompt: "first prompt",
		WorkspaceDir: "/work/project", Mode: "queue",
	}
	encoded, err := json.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ConversationStartParams
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.StartID != start.StartID || decoded.SubmissionID != start.SubmissionID || decoded.Prompt != start.Prompt {
		t.Fatalf("conversation.start round trip mismatch: %#v", decoded)
	}

	syncResult := ConversationSyncResult{
		ConversationID: "conv-1", WorkerEpoch: "epoch-1", EventWatermark: 17,
		HistoryRevision: 9, State: "running",
		Transcript: []ConversationTranscriptEntry{{SubmissionID: "submission-1", TurnID: "turn-1", Role: "user", Content: "first prompt"}},
	}
	encoded, err = json.Marshal(syncResult)
	if err != nil {
		t.Fatal(err)
	}
	var decodedSync ConversationSyncResult
	if err := json.Unmarshal(encoded, &decodedSync); err != nil {
		t.Fatal(err)
	}
	if decodedSync.WorkerEpoch != syncResult.WorkerEpoch || decodedSync.EventWatermark != syncResult.EventWatermark || decodedSync.HistoryRevision != syncResult.HistoryRevision {
		t.Fatalf("conversation.sync cursor/revision round trip mismatch: %#v", decodedSync)
	}

	// Existing one-shot task requests remain available alongside the additive chat RPCs.
	legacy, err := json.Marshal(SessionStartParams{Task: "legacy task"})
	if err != nil {
		t.Fatal(err)
	}
	var decodedLegacy SessionStartParams
	if err := json.Unmarshal(legacy, &decodedLegacy); err != nil {
		t.Fatal(err)
	}
	if decodedLegacy.Task != "legacy task" {
		t.Fatalf("legacy task field changed: %#v", decodedLegacy)
	}
}

func TestPermissionRespondParamsLegacyAndStructuredDecisions(t *testing.T) {
	var legacy PermissionRespondParams
	if err := json.Unmarshal([]byte(`{"request_id":"r1","decision":"allow"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Decision != DecisionAllowOnce {
		t.Fatalf("legacy allow decoded as %q", legacy.Decision)
	}
	params := PermissionRespondParams{
		RequestID: "r2", ConversationID: "c1", TurnID: "t1", Decision: DecisionAllowSession,
		SessionWhitelist: true, CommandPattern: "git status", ModifiedArguments: map[string]any{"path": "main.go"},
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PermissionRespondParams
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Decision != DecisionAllowSession || decoded.CommandPattern != params.CommandPattern || decoded.ModifiedArguments["path"] != "main.go" {
		t.Fatalf("structured permission response round trip mismatch: %+v", decoded)
	}
}

func TestJSONRPCProtocol_EncodeDecode(t *testing.T) {
	// 1. Request roundtrip
	effort := "medium"
	req, err := NewRequest("req-1", MethodSessionStart, SessionStartParams{
		Task:             "run tests",
		MaxContextTokens: 32000,
		MaxOutputChars:   50000,
		WorkingTailSize:  10,
		APIType:          "responses",
		ReasoningEffort:  &effort,
		DBPath:           "/custom/path.db",
		Env:              []string{"FOO=bar", "PATH=/custom/bin"},
		MCPConfig: &mcp.Config{
			MCPServers: map[string]mcp.ServerConfig{
				"test-mcp": {
					Command: "node",
					Args:    []string{"server.js"},
					Headers: map[string]string{
						"Authorization": "Bearer secret-token",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	encoded, err := EncodeMessage(req)
	if err != nil {
		t.Fatalf("failed to encode: %v", err)
	}
	if !strings.HasSuffix(string(encoded), "\n") {
		t.Fatalf("expected newline termination, got: %s", string(encoded))
	}

	decoded, err := DecodeMessage(encoded)
	if err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if decoded.JSONRPC != "2.0" || *decoded.ID != "req-1" || decoded.Method != MethodSessionStart {
		t.Fatalf("decoded request mismatch: %+v", decoded)
	}

	var params SessionStartParams
	if err := json.Unmarshal(decoded.Params, &params); err != nil {
		t.Fatalf("failed to unmarshal params: %v", err)
	}
	if params.Task != "run tests" {
		t.Fatalf("expected task 'run tests', got %q", params.Task)
	}
	if params.MaxContextTokens != 32000 {
		t.Fatalf("expected max context tokens 32000, got %d", params.MaxContextTokens)
	}
	if params.MaxOutputChars != 50000 {
		t.Fatalf("expected max output chars 50000, got %d", params.MaxOutputChars)
	}
	if params.WorkingTailSize != 10 {
		t.Fatalf("expected working tail size 10, got %d", params.WorkingTailSize)
	}
	if params.APIType != "responses" {
		t.Fatalf("expected api type 'responses', got %q", params.APIType)
	}
	if params.ReasoningEffort == nil || *params.ReasoningEffort != "medium" {
		t.Fatalf("expected reasoning effort 'medium', got %+v", params.ReasoningEffort)
	}
	if params.DBPath != "/custom/path.db" {
		t.Fatalf("expected db path '/custom/path.db', got %q", params.DBPath)
	}
	if len(params.Env) != 2 || params.Env[0] != "FOO=bar" || params.Env[1] != "PATH=/custom/bin" {
		t.Fatalf("expected env [FOO=bar PATH=/custom/bin], got %+v", params.Env)
	}
	if params.MCPConfig == nil || len(params.MCPConfig.MCPServers) != 1 {
		t.Fatalf("expected mcp config with 1 server, got %+v", params.MCPConfig)
	}
	if srv, ok := params.MCPConfig.MCPServers["test-mcp"]; !ok || srv.Headers["Authorization"] != "Bearer secret-token" {
		t.Fatalf("expected resolved test-mcp server header, got %+v", srv)
	}

	// 2. Response roundtrip
	res, err := NewResponse("req-1", SessionStartResult{SessionID: "sess-123"})
	if err != nil {
		t.Fatalf("failed to create response: %v", err)
	}
	encodedRes, _ := EncodeMessage(res)
	decodedRes, err := DecodeMessage(encodedRes)
	if err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if *decodedRes.ID != "req-1" || decodedRes.Error != nil {
		t.Fatalf("unexpected decoded response: %+v", decodedRes)
	}

	// 3. Notification roundtrip
	notify, err := NewNotification(NotifySessionEvent, SessionEventNotification{
		SessionID: "sess-123",
		EventID:   1,
		Type:      "reasoning",
		Content:   "thinking...",
	})
	if err != nil {
		t.Fatalf("failed to create notification: %v", err)
	}
	encodedNotify, _ := EncodeMessage(notify)
	decodedNotify, err := DecodeMessage(encodedNotify)
	if err != nil {
		t.Fatalf("failed to decode notification: %v", err)
	}
	if decodedNotify.ID != nil || decodedNotify.Method != NotifySessionEvent {
		t.Fatalf("unexpected notification: %+v", decodedNotify)
	}

	// 4. Error response
	idStr := "req-err"
	errRes := NewErrorResponse(&idStr, ErrCodeInvalidRequest, "invalid input", map[string]string{"field": "task"})
	encodedErr, _ := EncodeMessage(errRes)
	decodedErr, err := DecodeMessage(encodedErr)
	if err != nil {
		t.Fatalf("failed to decode error: %v", err)
	}
	if decodedErr.Error == nil || decodedErr.Error.Code != ErrCodeInvalidRequest {
		t.Fatalf("unexpected error response: %+v", decodedErr)
	}
	if decodedErr.Error.Error() != "invalid input" {
		t.Fatalf("expected Error() to return 'invalid input', got %q", decodedErr.Error.Error())
	}
}

func TestStreamCodec_ReadWrite(t *testing.T) {
	var buf bytes.Buffer
	codec := NewStreamCodec(&buf, &buf)

	req1, _ := NewRequest("1", MethodDaemonPing, nil)
	req2, _ := NewRequest("2", MethodDaemonStatus, nil)

	if err := codec.WriteMessage(req1); err != nil {
		t.Fatalf("write req1 failed: %v", err)
	}
	if err := codec.WriteMessage(req2); err != nil {
		t.Fatalf("write req2 failed: %v", err)
	}

	msg1, err := codec.ReadMessage()
	if err != nil {
		t.Fatalf("read msg1 failed: %v", err)
	}
	if *msg1.ID != "1" || msg1.Method != MethodDaemonPing {
		t.Fatalf("unexpected msg1: %+v", msg1)
	}

	msg2, err := codec.ReadMessage()
	if err != nil {
		t.Fatalf("read msg2 failed: %v", err)
	}
	if *msg2.ID != "2" || msg2.Method != MethodDaemonStatus {
		t.Fatalf("unexpected msg2: %+v", msg2)
	}

	// EOF on empty
	_, err = codec.ReadMessage()
	if err != io.EOF {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func TestDecodeMessage_Validation(t *testing.T) {
	// Empty message
	if _, err := DecodeMessage([]byte("   ")); err == nil {
		t.Errorf("expected error on empty data")
	}

	// Invalid JSON
	if _, err := DecodeMessage([]byte("{invalid")); err == nil {
		t.Errorf("expected error on malformed json")
	}

	// Wrong JSON-RPC version
	if _, err := DecodeMessage([]byte(`{"jsonrpc":"1.0","method":"foo"}`)); err == nil {
		t.Errorf("expected error on wrong jsonrpc version")
	}
}

func TestStreamCodec_OversizedMessageReturnsStreamReadError(t *testing.T) {
	// Generate payload exceeding DefaultMaxMessageSize (16MB)
	oversized := make([]byte, DefaultMaxMessageSize+1024)
	for i := range oversized {
		oversized[i] = 'a'
	}
	oversized[len(oversized)-1] = '\n'

	codec := NewStreamCodec(bytes.NewReader(oversized), &bytes.Buffer{})
	_, err := codec.ReadMessage()
	if err == nil {
		t.Fatal("expected error reading oversized message, got nil")
	}

	var streamErr *StreamReadError
	if !errors.As(err, &streamErr) {
		t.Fatalf("expected *StreamReadError, got %T: %v", err, err)
	}

	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("expected underlying error to be bufio.ErrTooLong, got %v", streamErr.Unwrap())
	}
}
