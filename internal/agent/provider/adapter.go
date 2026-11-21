package provider

import (
	"context"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// CompletionClientAdapter bridges an openai.Client to history.CompletionClient
// for Tier 2 context memory compaction summarization.
type CompletionClientAdapter struct {
	client *openai.Client
}

// NewCompletionClientAdapter creates an adapter for history distillation.
func NewCompletionClientAdapter(client *openai.Client) *CompletionClientAdapter {
	return &CompletionClientAdapter{client: client}
}

// CreateChatCompletion fulfills the history.CompletionClient interface.
func (a *CompletionClientAdapter) CreateChatCompletion(ctx context.Context, params openai.ChatCompletionNewParams, opts ...option.RequestOption) (*openai.ChatCompletion, error) {
	return a.client.Chat.Completions.New(ctx, params, opts...)
}
