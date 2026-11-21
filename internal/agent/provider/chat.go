package provider

import (
	"context"
	"fmt"

	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

// ChatCompletionsProvider implements ModelProvider for the /v1/chat/completions endpoint.
type ChatCompletionsProvider struct {
	client *openai.Client
}

// NewChatCompletionsProvider initializes a ChatCompletionsProvider.
func NewChatCompletionsProvider(client *openai.Client) *ChatCompletionsProvider {
	return &ChatCompletionsProvider{client: client}
}

// Protocol returns "chat".
func (p *ChatCompletionsProvider) Protocol() string {
	return "chat"
}

// Generate executes a chat completion step.
func (p *ChatCompletionsProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	params := openai.ChatCompletionNewParams{
		Model:    req.Model,
		Messages: req.Messages,
	}

	if req.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(req.MaxTokens))
	}

	if len(req.Tools) > 0 {
		openaiTools := make([]openai.ChatCompletionToolParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			openaiTools = append(openaiTools, openai.ChatCompletionToolParam{
				Function: shared.FunctionDefinitionParam{
					Name:        t.Name(),
					Description: param.NewOpt(t.Description()),
					Parameters:  tools.CanonicalizeParameters(t.Parameters()),
				},
			})
		}
		params.Tools = openaiTools
	}

	var opts []option.RequestOption
	if req.IsLocal && req.LocalNumCtx > 0 {
		opts = append(opts, option.WithJSONSet("options.num_ctx", req.LocalNumCtx))
	}
	if req.ReasoningEffort != "" {
		opts = append(opts, option.WithJSONSet("reasoning_effort", req.ReasoningEffort))
	}

	resp, err := p.client.Chat.Completions.New(ctx, params, opts...)
	if err != nil {
		return nil, err
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("received empty choices array from chat completions API")
	}

	choice := resp.Choices[0]
	toolCalls := make([]ToolCall, 0, len(choice.Message.ToolCalls))
	for i, tc := range choice.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", 1, i)
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}

	var reasoningTokens int64
	if resp.Usage.CompletionTokensDetails.ReasoningTokens > 0 {
		reasoningTokens = resp.Usage.CompletionTokensDetails.ReasoningTokens
	}

	usage := TokenUsage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		ReasoningTokens:  reasoningTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}

	return &GenerateResponse{
		Content:          choice.Message.Content,
		ReasoningContent: "",
		ToolCalls:        toolCalls,
		Usage:            usage,
		Protocol:         "chat",
	}, nil
}
