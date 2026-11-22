package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

type mockTool struct {
	name string
	desc string
}

func TestChatCompletionsProvider_StreamAssemblesTextAndToolArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeChunk := func(delta map[string]any, finish any) {
			chunk := map[string]any{"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": "test-model", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
			data, _ := json.Marshal(chunk)
			_, _ = w.Write(append(append([]byte("data: "), data...), []byte("\n\n")...))
		}
		writeChunk(map[string]any{"role": "assistant", "content": "Hello "}, nil)
		writeChunk(map[string]any{"content": "world", "tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"`}}}}, nil)
		writeChunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `x"}`}}}}, "tool_calls")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	provider := NewChatCompletionsProvider(&client)
	var text strings.Builder
	resp, err := provider.GenerateStream(context.Background(), GenerateRequest{Model: "test-model"}, func(delta string) error {
		text.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("GenerateStream failed: %v", err)
	}
	if text.String() != "Hello world" || resp.Content != "Hello world" {
		t.Fatalf("text deltas/final response = %q/%q", text.String(), resp.Content)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Arguments != `{"q":"x"}` {
		t.Fatalf("tool call arguments were not assembled: %+v", resp.ToolCalls)
	}
}

func TestResponsesProvider_StreamReturnsCompletedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\",\"sequence_number\":1,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"logprobs\":[]}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_1\",\"created_at\":1234567890,\"object\":\"response\",\"status\":\"completed\",\"model\":\"test-model\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hi\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"))
	}))
	defer server.Close()
	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	provider := NewResponsesProvider(&client)
	var text strings.Builder
	resp, err := provider.GenerateStream(context.Background(), GenerateRequest{Model: "test-model"}, func(delta string) error {
		text.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("GenerateStream failed: %v", err)
	}
	if text.String() != "Hi" || resp.Content != "Hi" || resp.Usage.TotalTokens != 3 {
		t.Fatalf("stream/final response mismatch: deltas=%q response=%+v", text.String(), resp)
	}
}

func TestAutoProvider_StreamFallsBackBeforeFirstDelta(t *testing.T) {
	chatRequests, responseRequests := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat/completions":
			chatRequests++
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "function tools with reasoning_effort are not supported; use /v1/responses"}})
		case "/responses":
			responseRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp_1", "created_at": 1234567890, "object": "response", "status": "completed", "model": "test-model",
				"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "fallback"}}}},
				"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	auto := NewAutoProvider(NewChatCompletionsProvider(&client), NewResponsesProvider(&client), false)
	var visible strings.Builder
	resp, err := auto.GenerateStream(context.Background(), GenerateRequest{Model: "test-model", Tools: []tools.Tool{&mockTool{name: "lookup"}}, ReasoningEffort: "high"}, func(delta string) error {
		visible.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("expected pre-output capability fallback: %v", err)
	}
	if visible.Len() != 0 || resp.Content != "fallback" || resp.Protocol != "responses" || chatRequests != 2 || responseRequests != 1 {
		t.Fatalf("unexpected fallback result: visible=%q response=%+v chat=%d responses=%d", visible.String(), resp, chatRequests, responseRequests)
	}
}

func TestAutoProvider_StreamDoesNotRetryAfterVisibleDelta(t *testing.T) {
	chatRequests, responseRequests := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			responseRequests++
			http.Error(w, "unexpected fallback", http.StatusInternalServerError)
			return
		}
		chatRequests++
		w.Header().Set("Content-Type", "text/event-stream")
		writeChunk := map[string]any{"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": "test-model", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}, "finish_reason": nil}}}
		data, _ := json.Marshal(writeChunk)
		_, _ = w.Write(append(append([]byte("data: "), data...), []byte("\n\n")...))
		_, _ = w.Write([]byte("data: {broken-json\n\n"))
	}))
	defer server.Close()
	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	auto := NewAutoProvider(NewChatCompletionsProvider(&client), NewResponsesProvider(&client), false)
	var visible strings.Builder
	_, err := auto.GenerateStream(context.Background(), GenerateRequest{Model: "test-model"}, func(delta string) error {
		visible.WriteString(delta)
		return nil
	})
	if err == nil || visible.String() != "partial" || chatRequests != 1 || responseRequests != 0 {
		t.Fatalf("stream failure was replayed: err=%v visible=%q chat=%d responses=%d", err, visible.String(), chatRequests, responseRequests)
	}
}

func TestChatCompletionsProvider_StreamHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := map[string]any{"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": "test-model", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}, "finish_reason": nil}}}
		data, _ := json.Marshal(chunk)
		_, _ = w.Write(append(append([]byte("data: "), data...), []byte("\n\n")...))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	provider := NewChatCompletionsProvider(&client)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var visible strings.Builder
	_, err := provider.GenerateStream(ctx, GenerateRequest{Model: "test-model"}, func(delta string) error {
		visible.WriteString(delta)
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || visible.String() != "partial" {
		t.Fatalf("cancellation result: err=%v visible=%q", err, visible.String())
	}
}

func (m *mockTool) Name() string        { return m.name }
func (m *mockTool) Description() string { return m.desc }
func (m *mockTool) Parameters() any     { return map[string]any{"type": "object"} }
func (m *mockTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "ok", nil
}

func TestToChatMessage(t *testing.T) {
	resp := &GenerateResponse{
		Content: "Hello world",
		ToolCalls: []ToolCall{
			{
				ID:        "call_1",
				Name:      "test_tool",
				Arguments: `{"query":"test"}`,
			},
		},
		Usage: TokenUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
		Protocol: "chat",
	}

	msg := resp.ToChatMessage()
	if msg.Content != "Hello world" {
		t.Errorf("expected content 'Hello world', got %s", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].ID != "call_1" {
		t.Errorf("expected tool ID call_1, got %s", msg.ToolCalls[0].ID)
	}
	if msg.ToolCalls[0].Function.Name != "test_tool" {
		t.Errorf("expected function name test_tool, got %s", msg.ToolCalls[0].Function.Name)
	}
}

func TestParseCapabilityError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected CapabilityErrorKind
	}{
		{
			name:     "Nil error",
			err:      nil,
			expected: CapabilityErrorNone,
		},
		{
			name:     "404 endpoint not found",
			err:      errors.New("404 Not Found: POST /v1/responses"),
			expected: CapabilityErrorEndpointUnsupported,
		},
		{
			name:     "Azure reasoning conflict error",
			err:      errors.New("function tools with reasoning_effort are not supported for gpt-6-luna in /v1/chat/completions; to use function tools, use /v1/responses or set reasoning_effort to 'none'"),
			expected: CapabilityErrorReasoningConflict,
		},
		{
			name:     "Unrecognized parameter error",
			err:      errors.New("400 Bad Request: unrecognized parameter: reasoning_effort"),
			expected: CapabilityErrorParamRejected,
		},
		{
			name:     "Unsupported parameter error without tool conflict",
			err:      errors.New("400 Bad Request: unsupported parameter: reasoning_effort is not supported with this model"),
			expected: CapabilityErrorParamRejected,
		},
		{
			name:     "Generic operational error",
			err:      errors.New("500 Internal Server Error"),
			expected: CapabilityErrorNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCapabilityError(tt.err)
			if got != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestAutoProvider_LocalTopologyPinsChat(t *testing.T) {
	client := openai.NewClient(option.WithBaseURL("http://localhost:11434/v1"), option.WithAPIKey("test"))
	chat := NewChatCompletionsProvider(&client)
	resp := NewResponsesProvider(&client)

	auto := NewAutoProvider(chat, resp, true)
	if auto.Protocol() != "chat" {
		t.Errorf("expected local AutoProvider to pin protocol 'chat', got %s", auto.Protocol())
	}
}

func TestAutoProvider_DiagnosticEscalation(t *testing.T) {
	chatCalls := 0
	responsesCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat/completions":
			chatCalls++
			// Simulate Azure reasoning conflict error
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "Function tools with reasoning_effort are not supported for model in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.",
					"type":    "invalid_request_error",
					"param":   "reasoning_effort",
				},
			})
		case "/responses":
			responsesCalls++
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         "resp_123",
				"created_at": 1234567890,
				"object":     "response",
				"status":     "completed",
				"output": []map[string]any{
					{
						"type":      "function_call",
						"call_id":   "call_abc",
						"name":      "test_tool",
						"arguments": `{"arg":"val"}`,
					},
				},
				"usage": map[string]any{
					"input_tokens":  50,
					"output_tokens": 20,
					"total_tokens":  70,
					"output_tokens_details": map[string]any{
						"reasoning_tokens": 15,
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
		}
	}))
	defer server.Close()

	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	chat := NewChatCompletionsProvider(&client)
	responses := NewResponsesProvider(&client)

	auto := NewAutoProvider(chat, responses, false)
	if auto.Protocol() != "auto" {
		t.Errorf("expected unlatched remote AutoProvider protocol 'auto', got %s", auto.Protocol())
	}

	req := GenerateRequest{
		Model:           "gpt-6-luna",
		Messages:        []openai.ChatCompletionMessageParamUnion{openai.UserMessage("run tool")},
		Tools:           []tools.Tool{&mockTool{name: "test_tool", desc: "mock tool"}},
		ReasoningEffort: "medium",
	}

	genResp, err := auto.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("expected successful escalation to responses API, got error: %v", err)
	}

	if genResp.Protocol != "responses" {
		t.Errorf("expected response protocol 'responses', got %s", genResp.Protocol)
	}
	if auto.Protocol() != "responses" {
		t.Errorf("expected AutoProvider to latch 'responses', got %s", auto.Protocol())
	}
	if len(genResp.ToolCalls) != 1 || genResp.ToolCalls[0].Name != "test_tool" {
		t.Errorf("expected 1 test_tool call, got: %+v", genResp.ToolCalls)
	}
	if genResp.Usage.ReasoningTokens != 15 {
		t.Errorf("expected 15 reasoning tokens, got %d", genResp.Usage.ReasoningTokens)
	}

	// Verify second turn executes directly against latched responses provider without hitting chat completions
	_, err = auto.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error on second turn: %v", err)
	}
	if chatCalls != 1 {
		t.Errorf("expected chatCalls to remain 1, got %d", chatCalls)
	}
	if responsesCalls != 2 {
		t.Errorf("expected responsesCalls to be 2, got %d", responsesCalls)
	}
}

func TestAutoProvider_Responses404FallbackToSanitizedChat(t *testing.T) {
	chatCalls := 0
	responsesCalls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat/completions":
			chatCalls++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			effort, _ := body["reasoning_effort"].(string)

			if effort != "none" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"message": "Function tools with reasoning_effort are not supported... use /v1/responses or set reasoning_effort to 'none'.",
					},
				})
				return
			}

			// When reasoning_effort is none, succeed
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "chat_123",
				"choices": []map[string]any{
					{
						"message": map[string]any{
							"role":    "assistant",
							"content": "chat success with reasoning_effort none",
						},
					},
				},
				"usage": map[string]any{
					"prompt_tokens":     10,
					"completion_tokens": 5,
					"total_tokens":      15,
				},
			})

		case "/responses":
			responsesCalls++
			// Endpoint unsupported (e.g. proxy gateway does not implement responses)
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "404 Not Found",
				},
			})
		}
	}))
	defer server.Close()

	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	chat := NewChatCompletionsProvider(&client)
	responses := NewResponsesProvider(&client)

	auto := NewAutoProvider(chat, responses, false)

	req := GenerateRequest{
		Model:           "test-model",
		Messages:        []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
		Tools:           []tools.Tool{&mockTool{name: "test_tool", desc: "mock tool"}},
		ReasoningEffort: "medium",
	}

	genResp, err := auto.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("expected fallback to sanitized chat to succeed, got: %v", err)
	}

	if genResp.Content != "chat success with reasoning_effort none" {
		t.Errorf("expected success message, got: %s", genResp.Content)
	}
	if auto.Protocol() != "chat" {
		t.Errorf("expected auto provider to latch 'chat', got %s", auto.Protocol())
	}

	// Turn 2: User sends subsequent turn with empty ReasoningEffort (default unset)
	// Invariant: The downgrade to reasoning_effort: "none" must persist so subsequent tool turns don't fail.
	req2 := GenerateRequest{
		Model:    "test-model",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("step 2")},
		Tools:    []tools.Tool{&mockTool{name: "test_tool", desc: "mock tool"}},
	}
	genResp2, err := auto.Generate(context.Background(), req2)
	if err != nil {
		t.Fatalf("expected Turn 2 to succeed with persisted reasoning_effort none, got: %v", err)
	}
	if genResp2.Content != "chat success with reasoning_effort none" {
		t.Errorf("expected success message on Turn 2, got: %s", genResp2.Content)
	}
}

func TestAutoProvider_OperationalErrorFailFast(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "Invalid API Key",
			},
		})
	}))
	defer server.Close()

	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("bad-key"))
	chat := NewChatCompletionsProvider(&client)
	respProv := NewResponsesProvider(&client)

	auto := NewAutoProvider(chat, respProv, false)

	req := GenerateRequest{
		Model:    "test-model",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")},
	}

	_, err := auto.Generate(context.Background(), req)
	if err == nil {
		t.Fatalf("expected 401 error, got nil")
	}

	// Latch must remain unlatched on operational errors
	if auto.Protocol() != "auto" {
		t.Errorf("expected protocol to remain 'auto' on operational error, got %s", auto.Protocol())
	}
}

func TestResponsesProvider_MessageTranslation(t *testing.T) {
	var inputCount int
	var capturedReasoning map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var rawBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&rawBody)
		if inputList, ok := rawBody["input"].([]any); ok {
			inputCount = len(inputList)
		}
		if reasoning, ok := rawBody["reasoning"].(map[string]any); ok {
			capturedReasoning = reasoning
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp_test",
			"created_at": 1234567890,
			"object":     "response",
			"status":     "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{
							"type": "output_text",
							"text": "test output",
						},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
				"total_tokens":  15,
			},
		})
	}))
	defer server.Close()

	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	prov := NewResponsesProvider(&client)

	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage("sys instructions"),
		openai.UserMessage("user question"),
		openai.ToolMessage("tool result output", "call_abc"),
	}

	req := GenerateRequest{
		Model:           "test-model",
		Messages:        msgs,
		ReasoningEffort: "none",
	}

	resp, err := prov.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Content != "test output" {
		t.Errorf("expected output text 'test output', got %s", resp.Content)
	}

	if inputCount != 3 {
		t.Fatalf("expected 3 captured input items, got %d", inputCount)
	}

	// Verify that explicit "none" reasoning effort is forwarded rather than dropped
	if capturedReasoning == nil || capturedReasoning["effort"] != "none" {
		t.Errorf("expected reasoning.effort 'none' to be forwarded, got: %+v", capturedReasoning)
	}
}

func TestAutoProvider_ParamRejectedOmitsParameterAndPersistsAcrossTurns(t *testing.T) {
	var capturedEffort []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var rawBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&rawBody)

		effortStr, hasEffort := rawBody["reasoning_effort"].(string)
		if hasEffort {
			capturedEffort = append(capturedEffort, effortStr)
			// Endpoint rejects reasoning_effort as unrecognized parameter
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "400 Bad Request: unrecognized parameter: reasoning_effort",
				},
			})
			return
		}
		capturedEffort = append(capturedEffort, "")

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chat_test",
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": "chat success without reasoning_effort",
					},
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 5,
				"total_tokens":      15,
			},
		})
	}))
	defer server.Close()

	client := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	chat := NewChatCompletionsProvider(&client)
	respProv := NewResponsesProvider(&client)

	auto := NewAutoProvider(chat, respProv, false)

	// Turn 1: Initial turn with configured BUDDY_REASONING_EFFORT="medium"
	req1 := GenerateRequest{
		Model:           "test-model",
		Messages:        []openai.ChatCompletionMessageParamUnion{openai.UserMessage("turn 1")},
		ReasoningEffort: "medium",
	}

	resp1, err := auto.Generate(context.Background(), req1)
	if err != nil {
		t.Fatalf("expected Turn 1 to recover by omitting reasoning_effort, got: %v", err)
	}
	if resp1.Content != "chat success without reasoning_effort" {
		t.Errorf("expected success message, got: %s", resp1.Content)
	}
	if auto.Protocol() != "chat" {
		t.Errorf("expected auto to latch 'chat', got %s", auto.Protocol())
	}

	// Turn 2: Subsequent turn with configured BUDDY_REASONING_EFFORT="medium"
	// Invariant: The parameter rejection omission must persist so it doesn't fail with 400 again
	req2 := GenerateRequest{
		Model:           "test-model",
		Messages:        []openai.ChatCompletionMessageParamUnion{openai.UserMessage("turn 2")},
		ReasoningEffort: "medium",
	}

	resp2, err := auto.Generate(context.Background(), req2)
	if err != nil {
		t.Fatalf("expected Turn 2 to succeed by continuing to omit reasoning_effort, got: %v", err)
	}
	if resp2.Content != "chat success without reasoning_effort" {
		t.Errorf("expected success message on Turn 2, got: %s", resp2.Content)
	}

	// Verify request history:
	// 1st request had "medium" -> rejected with 400
	// 2nd request (retry) had "" (omitted) -> succeeded
	// 3rd request (Turn 2) had "" (omitted) directly without failure
	if len(capturedEffort) != 3 {
		t.Fatalf("expected 3 total requests, got %d (%v)", len(capturedEffort), capturedEffort)
	}
	if capturedEffort[0] != "medium" || capturedEffort[1] != "" || capturedEffort[2] != "" {
		t.Errorf("expected efforts ['medium', '', ''], got: %v", capturedEffort)
	}
}
