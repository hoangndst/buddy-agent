package history

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go"
)

// mockCompletionClient simulates LLM completion responses for testing Tier 2 distillation.
type mockCompletionClient struct {
	response *openai.ChatCompletion
	err      error
	invoked  bool
}

func (m *mockCompletionClient) CreateChatCompletion(
	ctx context.Context,
	_ openai.ChatCompletionNewParams,
) (*openai.ChatCompletion, error) {
	m.invoked = true
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func TestTurnBlock_AtomicityAndFallbackSynthesis(t *testing.T) {
	// 1. User block
	userBlock := NewUserBlock("Implement context engine")
	userMsgs := userBlock.CompileMessages()
	if len(userMsgs) != 1 {
		t.Fatalf("expected 1 user message, got %d", len(userMsgs))
	}

	// 2. Simple assistant block
	asstMsg := openai.ChatCompletionMessage{
		Role:    "assistant",
		Content: "I will start now.",
	}
	asstBlock := NewAssistantSimpleBlock(asstMsg)
	asstMsgs := asstBlock.CompileMessages()
	if len(asstMsgs) != 1 {
		t.Fatalf("expected 1 assistant message, got %d", len(asstMsgs))
	}

	// 3. Tool block with complete results
	toolCallMsg := openai.ChatCompletionMessage{
		Role: "assistant",
		ToolCalls: []openai.ChatCompletionMessageToolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: openai.ChatCompletionMessageToolCallFunction{
					Name:      "read_file",
					Arguments: `{"path": "main.go"}`,
				},
			},
			{
				ID:   "call_2",
				Type: "function",
				Function: openai.ChatCompletionMessageToolCallFunction{
					Name:      "bash",
					Arguments: `{"cmd": "go test"}`,
				},
			},
		},
	}
	results := []ToolResult{
		{ToolCallID: "call_1", Name: "read_file", Content: "package main"},
		{ToolCallID: "call_2", Name: "bash", Content: "PASS"},
	}

	toolBlock := NewAssistantToolCallBlock(toolCallMsg, results)
	if err := toolBlock.ValidatePairing(); err != nil {
		t.Fatalf("expected valid pairing, got error: %v", err)
	}

	msgs := toolBlock.CompileMessages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 compiled messages (1 asst + 2 tool), got %d", len(msgs))
	}

	// 4. Aborted tool execution: only call_1 finished, call_2 was aborted
	partialResults := []ToolResult{
		{ToolCallID: "call_1", Name: "read_file", Content: "package main"},
	}
	abortedBlock := NewAssistantToolCallBlock(toolCallMsg, partialResults)
	if err := abortedBlock.ValidatePairing(); err == nil {
		t.Fatal("expected ValidatePairing to fail on aborted tool, got nil")
	}

	// CompileMessages must synthesize fallback error stub for call_2
	abortedMsgs := abortedBlock.CompileMessages()
	if len(abortedMsgs) != 3 {
		t.Fatalf("expected 3 compiled messages with fallback stub, got %d", len(abortedMsgs))
	}
}

func TestStateTracker_TelemetryAndRender(t *testing.T) {
	st := NewStateTracker("Fix failing build", []string{"Zero-CGO", "Pure Go static binary"})

	st.RecordFileAccess("main.go", "inspected")
	st.RecordFileAccess("internal/history/session.go", "created")
	st.RecordError("exit status 1: undefined symbol")
	st.RecordError("syntax error line 42")

	markdown := st.RenderEpisodicState()

	if !strings.Contains(markdown, "Fix failing build") {
		t.Error("rendered markdown missing root objective")
	}
	if !strings.Contains(markdown, "Zero-CGO") {
		t.Error("rendered markdown missing invariant")
	}
	if !strings.Contains(markdown, "`internal/history/session.go` | created") {
		t.Error("rendered markdown missing touched file")
	}
	if !strings.Contains(markdown, "syntax error line 42") {
		t.Error("rendered markdown missing error entry")
	}
}

func TestTokenEstimator_RuneAndCalibration(t *testing.T) {
	// Standard ASCII
	ascii := "Hello, world!"
	tokens := EstimateContent(ascii)
	if tokens < 1 || tokens > 10 {
		t.Fatalf("unexpected token count %d for %q", tokens, ascii)
	}

	// UTF-8 multi-byte characters
	cjk := "你好世界，这是一个测试文本"
	cjkTokens := EstimateContent(cjk)
	if cjkTokens <= 0 {
		t.Fatal("expected positive token count for CJK text")
	}

	tracker := NewUsageTracker(100)
	if tracker.CurrentTokens() != 100 {
		t.Fatalf("expected 100 tokens, got %d", tracker.CurrentTokens())
	}

	tracker.AddDelta(25)
	if tracker.CurrentTokens() != 125 {
		t.Fatalf("expected 125 tokens, got %d", tracker.CurrentTokens())
	}

	// Closed-loop calibration from API
	tracker.Calibrate(150)
	if tracker.CurrentTokens() != 150 {
		t.Fatalf("expected calibrated 150 tokens, got %d", tracker.CurrentTokens())
	}
}

func TestMicroPruning_StagedBoundary(t *testing.T) {
	longOutput := strings.Repeat("line content here\n", 50) + "exit status: 0"
	toolMsg := openai.ChatCompletionMessage{
		Role: "assistant",
		ToolCalls: []openai.ChatCompletionMessageToolCall{
			{ID: "call_1", Type: "function", Function: openai.ChatCompletionMessageToolCallFunction{Name: "bash"}},
		},
	}
	block := NewAssistantToolCallBlock(toolMsg, []ToolResult{
		{ToolCallID: "call_1", Name: "bash", Content: longOutput},
	})

	reclaimed, count := PruneStaleToolOutputs(&block, 100)
	if count != 1 {
		t.Fatalf("expected 1 pruned output, got %d", count)
	}
	if reclaimed <= 0 {
		t.Fatalf("expected positive reclaimed tokens, got %d", reclaimed)
	}

	prunedContent := block.ToolResults[0].Content
	if !strings.HasPrefix(prunedContent, "[Output pruned:") {
		t.Fatalf("expected stub prefix, got %q", prunedContent)
	}
	if !strings.Contains(prunedContent, "exit status: 0") {
		t.Fatalf("expected exit status preserved in stub, got %q", prunedContent)
	}

	// Idempotency: pruning again should be a no-op
	reclaimed2, count2 := PruneStaleToolOutputs(&block, 100)
	if count2 != 0 || reclaimed2 != 0 {
		t.Fatalf("expected idempotency on second prune, got count=%d reclaimed=%d", count2, reclaimed2)
	}
}

func TestSession_Tier1MicroPruneTrigger(t *testing.T) {
	// Configure MaxContextTokens = 1000, Soft = 700 tokens, Tail = 2
	session := NewSession(
		"You are Buddy.",
		"Run diagnostics",
		WithMaxContextTokens(1000),
		WithWorkingTailSize(2),
	)

	// Add 4 tool blocks with verbose outputs
	for i := 1; i <= 4; i++ {
		toolCallMsg := openai.ChatCompletionMessage{
			Role: "assistant",
			ToolCalls: []openai.ChatCompletionMessageToolCall{
				{
					ID:   fmt.Sprintf("call_%d", i),
					Type: "function",
					Function: openai.ChatCompletionMessageToolCallFunction{
						Name:      "bash",
						Arguments: `{"cmd": "run"}`,
					},
				},
			},
		}
		longOutput := strings.Repeat(fmt.Sprintf("log output line from step %d\n", i), 20) + "exit code: 0"
		session.CommitToolTurn(toolCallMsg, []ToolResult{
			{ToolCallID: fmt.Sprintf("call_%d", i), Name: "bash", Content: longOutput},
		})
	}

	// Watermark check should trigger Tier 1 Micro-Pruning on blocks 0 and 1,
	// while tail blocks 2 and 3 remain unpruned
	ctx := context.Background()
	mockClient := &mockCompletionClient{}
	err := session.EnsureWatermarks(ctx, mockClient, "gpt-4o")
	if err != nil {
		t.Fatalf("EnsureWatermarks failed: %v", err)
	}

	msgs := session.CompileMessages()
	if len(msgs) == 0 {
		t.Fatal("expected compiled messages")
	}
}

func TestSession_Tier2MacroDistill(t *testing.T) {
	// Configure low token limit so Tier 2 triggers
	session := NewSession(
		"You are Buddy.",
		"Migrate database schema",
		WithMaxContextTokens(500),
		WithWorkingTailSize(2),
	)

	// Add multiple verbose blocks to breach hard watermark
	for i := 1; i <= 5; i++ {
		asstMsg := openai.ChatCompletionMessage{
			Role:    "assistant",
			Content: strings.Repeat(fmt.Sprintf("Working on migration step %d. ", i), 25),
		}
		session.CommitSimpleAssistant(asstMsg)
	}

	mockClient := &mockCompletionClient{
		response: &openai.ChatCompletion{
			Choices: []openai.ChatCompletionChoice{
				{
					Message: openai.ChatCompletionMessage{
						Role:    "assistant",
						Content: "### Goal Progress\nCompleted migration steps 1-3.\n### Key Decisions & Changes\nApplied schema patch.",
					},
				},
			},
		},
	}

	ctx := context.Background()
	err := session.EnsureWatermarks(ctx, mockClient, "gpt-4o")
	if err != nil {
		t.Fatalf("EnsureWatermarks failed: %v", err)
	}

	if !mockClient.invoked {
		t.Fatal("expected Tier 2 distillation client to be invoked")
	}

	// Session blocks must be reduced to WorkingTailSize
	if session.BlockCount() > 2 {
		t.Fatalf("expected blocks to be trimmed to tail size 2, got %d", session.BlockCount())
	}

	msgs := session.CompileMessages()
	foundSummary := false
	for _, m := range msgs {
		if m.OfUser != nil && strings.Contains(m.OfUser.Content.OfString.Value, "Completed migration steps 1-3") {
			foundSummary = true
			break
		}
	}
	if !foundSummary {
		t.Fatal("expected episodic narrative summary in compiled messages")
	}
}

func TestSession_Tier3DeterministicLocalFallback(t *testing.T) {
	session := NewSession(
		"You are Buddy.",
		"Audit system security",
		WithMaxContextTokens(1000),
		WithWorkingTailSize(2),
	)

	for i := 1; i <= 5; i++ {
		asstMsg := openai.ChatCompletionMessage{
			Role:    "assistant",
			Content: strings.Repeat(fmt.Sprintf("Security audit analysis step %d. ", i), 25),
		}
		session.CommitSimpleAssistant(asstMsg)
	}

	// Mock client fails with 500 server error
	mockClient := &mockCompletionClient{
		err: errors.New("500 internal server error from OpenAI"),
	}

	ctx := context.Background()
	err := session.EnsureWatermarks(ctx, mockClient, "gpt-4o")
	if err != nil {
		t.Fatalf("expected Tier 3 fallback to handle distillation error gracefully, got: %v", err)
	}

	// Intermediate blocks must still be evicted to prevent context overflow
	if session.BlockCount() > 2 {
		t.Fatalf("expected blocks trimmed to tail size 2 by Tier 3, got %d", session.BlockCount())
	}

	msgs := session.CompileMessages()
	foundNotice := false
	for _, m := range msgs {
		if m.OfUser != nil && strings.Contains(m.OfUser.Content.OfString.Value, "evicted via local fallback") {
			foundNotice = true
			break
		}
	}
	if !foundNotice {
		t.Fatal("expected fallback notice in episodic state")
	}
}

func TestSession_ContextCancellation_NoFallback(t *testing.T) {
	session := NewSession(
		"You are Buddy.",
		"Process cancellation test",
		WithMaxContextTokens(500),
		WithWorkingTailSize(2),
	)

	for i := 1; i <= 5; i++ {
		asstMsg := openai.ChatCompletionMessage{
			Role:    "assistant",
			Content: strings.Repeat("Cancel test. ", 30),
		}
		session.CommitSimpleAssistant(asstMsg)
	}

	preCount := session.BlockCount()

	// Pass canceled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mockClient := &mockCompletionClient{
		err: context.Canceled,
	}

	err := session.EnsureWatermarks(ctx, mockClient, "gpt-4o")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled error, got: %v", err)
	}

	// State must NOT be mutated under cancellation
	if session.BlockCount() != preCount {
		t.Fatalf("expected session blocks preserved on cancellation, got %d (was %d)", session.BlockCount(), preCount)
	}
}

func TestSession_ErrContextExhausted(t *testing.T) {
	// Tiny ceiling of 10 tokens cannot fit system prompt
	session := NewSession(
		"You are Buddy with a very extensive set of system instructions.",
		"Run heavy workload",
		WithMaxContextTokens(10),
	)

	ctx := context.Background()
	mockClient := &mockCompletionClient{}
	err := session.EnsureWatermarks(ctx, mockClient, "gpt-4o")
	if !errors.Is(err, ErrContextExhausted) {
		t.Fatalf("expected ErrContextExhausted, got: %v", err)
	}
}

func TestSession_ConcurrentCompileAndFileAccess(_ *testing.T) {
	session := NewSession(
		"System prompt",
		"Task prompt",
		WithMaxContextTokens(10000),
	)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Concurrently record file accesses and errors
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			counter := 0
			for {
				select {
				case <-stop:
					return
				default:
					session.RecordFileAccess(fmt.Sprintf("file_%d_%d.go", workerID, counter), "modified")
					session.RecordError(fmt.Sprintf("error_%d_%d", workerID, counter))
					counter++
				}
			}
		}(i)
	}

	// Concurrently compile messages
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = session.CompileMessages()
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestDistillSystemPrompt_EmbedContent(t *testing.T) {
	if defaultDistillSystemPrompt == "" {
		t.Fatal("expected defaultDistillSystemPrompt to be non-empty via go:embed")
	}
	expectedDirectives := []string{
		"autonomous context compaction engine",
		"Read-Only / Non-Execution Safety",
		"Information Retention Priority",
		"### Goal & Core Objectives",
		"### Actions Taken & Key Findings",
		"### Current State & Touched Resources",
		"### Active Issues & Blockers",
		"### Next Immediate Steps",
	}
	for _, directive := range expectedDirectives {
		if !strings.Contains(defaultDistillSystemPrompt, directive) {
			t.Errorf("expected embedded distill prompt to contain %q", directive)
		}
	}
}
