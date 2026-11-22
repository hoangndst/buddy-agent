package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
)

// ResponsesProvider implements ModelProvider for the /v1/responses endpoint.
type ResponsesProvider struct {
	client *openai.Client
}

// NewResponsesProvider initializes a ResponsesProvider.
func NewResponsesProvider(client *openai.Client) *ResponsesProvider {
	return &ResponsesProvider{client: client}
}

// Protocol returns "responses".
func (p *ResponsesProvider) Protocol() string {
	return "responses"
}

// Generate executes a response generation step.
func (p *ResponsesProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	params, opts := responseParams(req)
	resp, err := p.client.Responses.New(ctx, params, opts...)
	if err != nil {
		return nil, err
	}
	return normalizeResponse(resp)
}

// GenerateStream streams text deltas and returns only the completed normalized
// response. Tool argument events are intentionally buffered by the API response
// and are dispatched by the engine only after completion.
func (p *ResponsesProvider) GenerateStream(ctx context.Context, req GenerateRequest, onTextDelta func(string) error) (*GenerateResponse, error) {
	params, opts := responseParams(req)
	stream := p.client.Responses.NewStreaming(ctx, params, opts...)
	defer func() { _ = stream.Close() }()

	var completed *responses.Response
	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "response.output_text.delta", "response.refusal.delta":
			delta := event.Delta.OfString
			if delta != "" && onTextDelta != nil {
				if err := onTextDelta(delta); err != nil {
					return nil, err
				}
			}
		case "response.completed":
			resp := event.Response
			completed = &resp
		case "response.failed", "error":
			if event.Message != "" {
				return nil, fmt.Errorf("responses stream failed: %s", event.Message)
			}
			return nil, fmt.Errorf("responses stream failed (%s)", event.Type)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, fmt.Errorf("responses stream ended without a completed response")
	}
	return normalizeResponse(completed)
}

func responseParams(req GenerateRequest) (responses.ResponseNewParams, []option.RequestOption) {
	inputItems := mapMessagesToResponseInputs(req.Messages)

	params := responses.ResponseNewParams{
		Model: req.Model,
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: inputItems,
		},
	}

	if req.MaxTokens > 0 {
		params.MaxOutputTokens = openai.Int(int64(req.MaxTokens))
	}

	if len(req.Tools) > 0 {
		toolParams := make([]responses.ToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := responses.ToolParamOfFunction(t.Name(), tools.CanonicalizeParameters(t.Parameters()), false)
			tool.OfFunction.Description = openai.String(t.Description())
			toolParams = append(toolParams, tool)
		}
		params.Tools = toolParams
	}

	var opts []option.RequestOption
	if req.ReasoningEffort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort: shared.ReasoningEffort(req.ReasoningEffort),
		}
	}

	return params, opts
}

func normalizeResponse(resp *responses.Response) (*GenerateResponse, error) {
	var reasoningParts []string
	var toolCalls []ToolCall

	for i, item := range resp.Output {
		switch item.Type {
		case "function_call":
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			if id == "" {
				id = fmt.Sprintf("call_%d_%d", 1, i)
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:        id,
				Name:      item.Name,
				Arguments: item.Arguments,
			})
		case "reasoning":
			for _, sum := range item.Summary {
				if sum.Text != "" {
					reasoningParts = append(reasoningParts, sum.Text)
				}
			}
		}
	}

	usage := TokenUsage{
		PromptTokens:     resp.Usage.InputTokens,
		CompletionTokens: resp.Usage.OutputTokens,
		ReasoningTokens:  resp.Usage.OutputTokensDetails.ReasoningTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}

	return &GenerateResponse{
		Content:          resp.OutputText(),
		ReasoningContent: strings.Join(reasoningParts, "\n"),
		ToolCalls:        toolCalls,
		Usage:            usage,
		Protocol:         "responses",
	}, nil
}

// mapMessagesToResponseInputs translates canonical ChatCompletionMessageParamUnion slice
// into native responses.ResponseInputParam items.
func mapMessagesToResponseInputs(msgs []openai.ChatCompletionMessageParamUnion) responses.ResponseInputParam {
	items := make(responses.ResponseInputParam, 0, len(msgs))

	for _, msg := range msgs {
		if msg.OfSystem != nil {
			if msg.OfSystem.Content.OfString.Valid() {
				items = append(items, responses.ResponseInputItemParamOfMessage(
					msg.OfSystem.Content.OfString.Value,
					responses.EasyInputMessageRoleSystem,
				))
			}
			continue
		}
		if msg.OfDeveloper != nil {
			if msg.OfDeveloper.Content.OfString.Valid() {
				items = append(items, responses.ResponseInputItemParamOfMessage(
					msg.OfDeveloper.Content.OfString.Value,
					responses.EasyInputMessageRoleDeveloper,
				))
			}
			continue
		}
		if msg.OfUser != nil {
			if msg.OfUser.Content.OfString.Valid() {
				items = append(items, responses.ResponseInputItemParamOfMessage(
					msg.OfUser.Content.OfString.Value,
					responses.EasyInputMessageRoleUser,
				))
			}
			continue
		}
		if msg.OfAssistant != nil {
			// If assistant called tools, map function calls
			if len(msg.OfAssistant.ToolCalls) > 0 {
				for _, tc := range msg.OfAssistant.ToolCalls {
					items = append(items, responses.ResponseInputItemParamOfFunctionCall(
						tc.Function.Arguments,
						tc.ID,
						tc.Function.Name,
					))
				}
			}
			// If assistant also had text content, include assistant message
			if msg.OfAssistant.Content.OfString.Valid() && msg.OfAssistant.Content.OfString.Value != "" {
				items = append(items, responses.ResponseInputItemParamOfMessage(
					msg.OfAssistant.Content.OfString.Value,
					responses.EasyInputMessageRoleAssistant,
				))
			}
			continue
		}
		if msg.OfTool != nil {
			callID := msg.OfTool.ToolCallID
			content := ""
			if msg.OfTool.Content.OfString.Valid() {
				content = msg.OfTool.Content.OfString.Value
			}
			items = append(items, responses.ResponseInputItemParamOfFunctionCallOutput(
				callID,
				content,
			))
			continue
		}
	}

	return items
}
