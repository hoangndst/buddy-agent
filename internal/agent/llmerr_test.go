package agent

import (
	"errors"
	"testing"

	"github.com/openai/openai-go"
)

func TestParseContextLengthError(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantTokens    int
		wantMatched   bool
	}{
		{
			name:        "nil error",
			err:         nil,
			wantTokens:  0,
			wantMatched: false,
		},
		{
			name: "OpenAI structured context_length_exceeded with limit",
			err: &openai.Error{
				Code:    "context_length_exceeded",
				Message: "This model's maximum context length is 16384 tokens. However, your messages resulted in 18230 tokens.",
			},
			wantTokens:  16384,
			wantMatched: true,
		},
		{
			name: "OpenAI structured context_length_exceeded with 32k limit",
			err: &openai.Error{
				Code:    "context_length_exceeded",
				Message: "maximum context length is 32768 tokens. Please reduce your message length.",
			},
			wantTokens:  32768,
			wantMatched: true,
		},
		{
			name: "Standard error string with limit of X tokens",
			err:  errors.New("API error: limit of 8192 tokens reached for this model"),
			wantTokens:  8192,
			wantMatched: true,
		},
		{
			name: "Standard error string with X tokens maximum",
			err:  errors.New("Error: 64000 tokens maximum exceeded by prompt"),
			wantTokens:  64000,
			wantMatched: true,
		},
		{
			name: "OpenAI structured error without explicit limit",
			err: &openai.Error{
				Code:    "context_length_exceeded",
				Message: "Your prompt tokens exceed the context window size.",
			},
			wantTokens:  0,
			wantMatched: true,
		},
		{
			name:        "Raw string context_length_exceeded without number",
			err:         errors.New("HTTP 400: context_length_exceeded"),
			wantTokens:  0,
			wantMatched: true,
		},
		{
			name:        "Unrelated rate limit error",
			err:         errors.New("429 Too Many Requests: rate limit reached"),
			wantTokens:  0,
			wantMatched: false,
		},
		{
			name: "Unrelated authentication error",
			err: &openai.Error{
				Code:    "invalid_api_key",
				Message: "Incorrect API key provided.",
			},
			wantTokens:  0,
			wantMatched: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tokens, matched := ParseContextLengthError(tc.err)
			if matched != tc.wantMatched {
				t.Errorf("ParseContextLengthError() matched = %v, want %v", matched, tc.wantMatched)
			}
			if tokens != tc.wantTokens {
				t.Errorf("ParseContextLengthError() tokens = %d, want %d", tokens, tc.wantTokens)
			}
		})
	}
}
