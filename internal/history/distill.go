package history

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
)

//go:embed prompts/distill_system.md
var defaultDistillSystemPrompt string

const (
	// MaxDistillTokens bounds the summary generation output.
	MaxDistillTokens = 1500
	// DistillTimeout sets the isolated deadline for summarizer calls.
	DistillTimeout = 45 * time.Second
)

// DistillIntermediateBlocks calls the LLM with strict XML non-execution framing
// to compress intermediate turns into a bounded narrative summary.
func DistillIntermediateBlocks(
	ctx context.Context,
	client CompletionClient,
	model string,
	previousSummary string,
	blocks []TurnBlock,
	tracker *StateTracker,
) (string, error) {
	if client == nil {
		return "", errors.New("completion client is required for distillation")
	}
	if len(blocks) == 0 {
		return previousSummary, nil
	}

	// Check if parent context was already canceled
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Isolated child context to prevent external hangs while honoring parent cancellation
	distillCtx, cancel := context.WithTimeout(ctx, DistillTimeout)
	defer cancel()

	serializedHistory := serializeBlocksForDistillation(previousSummary, blocks, tracker)

	systemPrompt := strings.TrimSpace(defaultDistillSystemPrompt)

	userPrompt := fmt.Sprintf(
		"<conversation_history_to_summarize>\n%s\n</conversation_history_to_summarize>",
		serializedHistory,
	)

	params := openai.ChatCompletionNewParams{
		Model: model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userPrompt),
		},
		MaxTokens:   param.NewOpt(int64(MaxDistillTokens)),
		Temperature: param.NewOpt(0.2),
	}

	resp, err := client.CreateChatCompletion(distillCtx, params)
	if err != nil {
		return "", fmt.Errorf("distillation LLM call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("distillation model returned empty choices")
	}

	summary := strings.TrimSpace(resp.Choices[0].Message.Content)
	if summary == "" {
		return "", errors.New("distillation model returned empty content")
	}

	// Ensure hard token ceiling on summary output
	if EstimateContent(summary) > MaxDistillTokens {
		lines := strings.Split(summary, "\n")
		var clamped strings.Builder
		for _, l := range lines {
			if EstimateContent(clamped.String()+"\n"+l) > MaxDistillTokens {
				break
			}
			clamped.WriteString(l + "\n")
		}
		summary = strings.TrimSpace(clamped.String())
	}

	return summary, nil
}

// serializeBlocksForDistillation converts historical turns into a readable text log.
func serializeBlocksForDistillation(
	previousSummary string,
	blocks []TurnBlock,
	tracker *StateTracker,
) string {
	var sb strings.Builder

	if previousSummary != "" {
		sb.WriteString("## Prior Progress Summary\n")
		sb.WriteString(previousSummary)
		sb.WriteString("\n\n")
	}

	if tracker != nil {
		sb.WriteString("## Current State Registry\n")
		sb.WriteString(tracker.RenderEpisodicState())
		sb.WriteString("\n\n")
	}

	sb.WriteString("## Interaction Turns to Distill\n")
	for i, b := range blocks {
		switch b.Kind {
		case BlockKindUser:
			if b.UserPrompt != "" {
				sb.WriteString(fmt.Sprintf("[%d] User: %s\n", i+1, b.UserPrompt))
			}
		case BlockKindAssistantSimple:
			if b.AssistantSimple != nil {
				sb.WriteString(fmt.Sprintf("[%d] Assistant: %s\n", i+1, b.AssistantSimple.Content))
			}
		case BlockKindAssistantToolCall:
			if b.AssistantToolCall != nil {
				sb.WriteString(fmt.Sprintf("[%d] Assistant Invoked Tools:\n", i+1))
				for _, tc := range b.AssistantToolCall.ToolCalls {
					sb.WriteString(fmt.Sprintf("  - %s(args: %s)\n", tc.Function.Name, tc.Function.Arguments))
				}
			}
			for _, tr := range b.ToolResults {
				str := tr.Content
				if len(str) > 300 {
					str = str[:300] + "... [truncated for summarization]"
				}
				sb.WriteString(fmt.Sprintf("  -> Tool Result [%s]: %s\n", tr.ToolCallID, str))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}
