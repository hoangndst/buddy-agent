package provider

import (
	"context"
	"sync/atomic"
)

const (
	latchUnset     int32 = 0
	latchChat      int32 = 1
	latchResponses int32 = 2
)

// AutoProvider manages automatic capability negotiation and diagnostic escalation between
// ChatCompletionsProvider and ResponsesProvider.
type AutoProvider struct {
	chat      *ChatCompletionsProvider
	responses *ResponsesProvider
	isLocal   bool
	latch     int32
	forceNone int32
}

// NewAutoProvider creates an AutoProvider instance.
func NewAutoProvider(chat *ChatCompletionsProvider, responses *ResponsesProvider, isLocal bool) *AutoProvider {
	p := &AutoProvider{
		chat:      chat,
		responses: responses,
		isLocal:   isLocal,
	}
	if isLocal {
		atomic.StoreInt32(&p.latch, latchChat)
	}
	return p
}

// Protocol returns the active latched protocol ("chat", "responses", or "auto" if unlatched).
func (p *AutoProvider) Protocol() string {
	l := atomic.LoadInt32(&p.latch)
	switch l {
	case latchResponses:
		return "responses"
	case latchChat:
		return "chat"
	default:
		return "auto"
	}
}

// Generate executes an LLM generation turn with diagnostic capability escalation.
func (p *AutoProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	currentLatch := atomic.LoadInt32(&p.latch)

	// If already latched to responses, delegate directly
	if currentLatch == latchResponses {
		return p.responses.Generate(ctx, req)
	}

	// If reasoning downgrade was previously latched, enforce reasoning_effort: "none"
	if atomic.LoadInt32(&p.forceNone) == 1 {
		req.ReasoningEffort = "none"
	}

	// If already latched to chat, delegate directly
	if currentLatch == latchChat {
		resp, err := p.chat.Generate(ctx, req)
		if err != nil {
			kind := ParseCapabilityError(err)
			if kind == CapabilityErrorReasoningConflict || kind == CapabilityErrorParamRejected {
				// Retry with sanitized reasoning_effort and latch the downgrade
				atomic.StoreInt32(&p.forceNone, 1)
				sanitizedReq := req
				sanitizedReq.ReasoningEffort = "none"
				return p.chat.Generate(ctx, sanitizedReq)
			}
		}
		return resp, err
	}

	// Unlatched remote endpoint: default to chat completions (eliminating 404 probing on standard gateways)
	resp, err := p.chat.Generate(ctx, req)
	if err == nil {
		// Only latch chat when tools were executed and succeeded, ensuring that a pure conversational Turn 1
		// does not prematurely lock the latch if Turn 2 needs tool escalation on Azure
		if len(req.Tools) > 0 {
			atomic.CompareAndSwapInt32(&p.latch, latchUnset, latchChat)
		}
		return resp, nil
	}

	// Check for capability conflict
	kind := ParseCapabilityError(err)
	if kind == CapabilityErrorReasoningConflict || kind == CapabilityErrorParamRejected {
		// Diagnostic escalation to Responses API
		respResp, respErr := p.responses.Generate(ctx, req)
		if respErr == nil {
			atomic.StoreInt32(&p.latch, latchResponses)
			return respResp, nil
		}
		// If Responses API is unsupported (404/405), downgrade to chat with reasoning_effort: "none"
		if ParseCapabilityError(respErr) == CapabilityErrorEndpointUnsupported {
			atomic.StoreInt32(&p.latch, latchChat)
			atomic.StoreInt32(&p.forceNone, 1)
			sanitizedReq := req
			sanitizedReq.ReasoningEffort = "none"
			return p.chat.Generate(ctx, sanitizedReq)
		}
		return nil, respErr
	}

	// Other operational error (401, 403, 429, 5xx, or network timeout) - fail fast immediately without latching
	return nil, err
}
