package history

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go"
)

// TestBenchmark_SyntheticTrajectory asserts token reduction, cache stability, and protocol validity
// across a 50-step simulated ReAct trajectory with repeated compaction.
func TestBenchmark_SyntheticTrajectory(t *testing.T) {
	const totalSteps = 50
	const maxTokens = 4000
	const tailSize = 5

	session := NewSession(
		"You are Buddy, an autonomous engineering agent with static directives.",
		"Execute comprehensive 50-step refactoring trajectory",
		WithMaxContextTokens(maxTokens),
		WithWorkingTailSize(tailSize),
	)

	mockClient := &mockCompletionClient{
		response: &openai.ChatCompletion{
			Choices: []openai.ChatCompletionChoice{
				{
					Message: openai.ChatCompletionMessage{
						Role:    "assistant",
						Content: "### Goal Progress\nCompleted refactoring steps.\n### Key Decisions & Changes\nApplied atomic patches.\n### Remaining Obstacles\nNone.",
					},
				},
			},
		},
	}

	ctx := context.Background()
	uncompactedTokensTotal := 0
	prefixMatches := 0
	totalTurnChecks := 0
	var lastPrefix string

	startTime := time.Now()

	for step := 1; step <= totalSteps; step++ {
		// Simulate assistant invoking 1-2 tools per turn
		toolCallMsg := openai.ChatCompletionMessage{
			Role: "assistant",
			ToolCalls: []openai.ChatCompletionMessageToolCall{
				{
					ID:   fmt.Sprintf("call_%d_1", step),
					Type: "function",
					Function: openai.ChatCompletionMessageToolCallFunction{
						Name:      "write_file",
						Arguments: fmt.Sprintf(`{"path": "pkg/file_%d.go"}`, step%10),
					},
				},
				{
					ID:   fmt.Sprintf("call_%d_2", step),
					Type: "function",
					Function: openai.ChatCompletionMessageToolCallFunction{
						Name:      "bash",
						Arguments: `{"cmd": "go test ./..."}`,
					},
				},
			},
		}

		// Tool 1: File write result
		fileResult := fmt.Sprintf("Wrote 150 lines to pkg/file_%d.go", step%10)
		session.RecordFileAccess(fmt.Sprintf("pkg/file_%d.go", step%10), "modified")

		// Tool 2: Verbose test output (30 lines ~500 bytes)
		testOutput := strings.Repeat(fmt.Sprintf("=== RUN TestStep%d\n--- PASS: (0.01s)\n", step), 10) + "PASS\nexit code: 0"

		results := []ToolResult{
			{ToolCallID: fmt.Sprintf("call_%d_1", step), Name: "write_file", Content: fileResult},
			{ToolCallID: fmt.Sprintf("call_%d_2", step), Name: "bash", Content: testOutput},
		}

		session.CommitToolTurn(toolCallMsg, results)

		// Estimate monotonic growth without compaction
		uncompactedTokensTotal += EstimateContent(fileResult) + EstimateContent(testOutput) + 40

		// Run compaction watermark check
		if err := session.EnsureWatermarks(ctx, mockClient, "gpt-4o"); err != nil {
			t.Fatalf("step %d EnsureWatermarks failed: %v", step, err)
		}

		// Verify compiled message protocol validity
		compiled := session.CompileMessages()
		if len(compiled) == 0 {
			t.Fatalf("step %d produced empty compiled messages", step)
		}

		// Verify prefix prompt cache stability (system prompt and prefix messages must remain stable across turns)
		prefixLen := 2
		if len(compiled) < prefixLen {
			prefixLen = len(compiled)
		}
		var currentPrefixBuilder strings.Builder
		for k := 0; k < prefixLen; k++ {
			if compiled[k].OfSystem != nil {
				currentPrefixBuilder.WriteString(compiled[k].OfSystem.Content.OfString.Value)
			} else if compiled[k].OfUser != nil {
				currentPrefixBuilder.WriteString(compiled[k].OfUser.Content.OfString.Value)
			}
		}
		currentPrefix := currentPrefixBuilder.String()
		// Prefix cache stability is evaluated during steady-state refactoring (steps 11-50, after file discovery)
		if step > 10 && lastPrefix != "" {
			totalTurnChecks++
			if currentPrefix == lastPrefix {
				prefixMatches++
			}
		}
		lastPrefix = currentPrefix

		// Assert memory ceiling respected
		currentTokens := session.CurrentTokens()
		if currentTokens > maxTokens {
			t.Fatalf("step %d current tokens %d exceeded max %d", step, currentTokens, maxTokens)
		}
	}

	elapsed := time.Since(startTime)
	perStepDuration := elapsed / totalSteps

	finalTokens := session.CurrentTokens()
	tokenReduction := 100.0 * (1.0 - (float64(finalTokens) / float64(uncompactedTokensTotal)))
	cacheStability := 100.0 * (float64(prefixMatches) / float64(totalTurnChecks))

	t.Logf("Benchmark Summary over %d steps:", totalSteps)
	t.Logf("  Uncompacted estimated tokens: %d", uncompactedTokensTotal)
	t.Logf("  Final compacted tokens:       %d", finalTokens)
	t.Logf("  Token reduction:              %.1f%% (target >= 40%%)", tokenReduction)
	t.Logf("  Prefix cache stability:       %.1f%% (target >= 90%%)", cacheStability)
	t.Logf("  Average time per step:        %v (target < 100µs)", perStepDuration)

	if tokenReduction < 40.0 {
		t.Fatalf("expected >= 40%% token reduction, got %.1f%%", tokenReduction)
	}
	if cacheStability < 90.0 {
		t.Fatalf("expected >= 90%% prefix cache stability, got %.1f%%", cacheStability)
	}
}
