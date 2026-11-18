// Package tools provides the tool registry, execution sandbox, and built-in operations.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

// Tool represents an executable function callable by the agent.
type Tool interface {
	Name() string
	Description() string
	Parameters() any
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry stores and dispatches agent tools.
type Registry struct {
	mu             sync.RWMutex
	tools          map[string]Tool
	maxOutputChars int
}

// NewRegistry initializes an empty tool registry.
func NewRegistry(maxOutputChars int) *Registry {
	if maxOutputChars <= 0 {
		maxOutputChars = 10000
	}
	return &Registry{
		tools:          make(map[string]Tool),
		maxOutputChars: maxOutputChars,
	}
}

// Register adds a tool to the registry.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Get retrieves a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// OpenAITools converts registered tools into the official OpenAI Tool format,
// strictly sorted alphabetically by tool name to preserve LLM provider KV prompt caches.
func (r *Registry) OpenAITools() []openai.ChatCompletionToolParam {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)

	res := make([]openai.ChatCompletionToolParam, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		res = append(res, openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        t.Name(),
				Description: param.NewOpt(t.Description()),
				Parameters:  CanonicalizeParameters(t.Parameters()),
			},
		})
	}
	return res
}

// CanonicalizeParameters normalizes tool parameters into shared.FunctionParameters.
// If input is nil or empty, a valid JSON Schema object schema is produced.
func CanonicalizeParameters(raw any) shared.FunctionParameters {
	if raw == nil {
		return shared.FunctionParameters{
			"type":       "object",
			"properties": map[string]any{},
		}
	}

	switch v := raw.(type) {
	case shared.FunctionParameters:
		ensureObjectType(v)
		return v
	case map[string]any:
		params := shared.FunctionParameters(v)
		ensureObjectType(params)
		return params
	default:
		// Marshal and unmarshal arbitrary structs or byte slices into map
		data, err := json.Marshal(raw)
		if err != nil {
			return shared.FunctionParameters{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		var parsed map[string]any
		if err := json.Unmarshal(data, &parsed); err != nil {
			return shared.FunctionParameters{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		params := shared.FunctionParameters(parsed)
		ensureObjectType(params)
		return params
	}
}

func ensureObjectType(p shared.FunctionParameters) {
	if _, ok := p["type"]; !ok {
		p["type"] = "object"
	}
	if _, ok := p["properties"]; !ok {
		p["properties"] = map[string]any{}
	}
}

// Dispatch executes a tool by name, catches panics, and truncates the response.
func (r *Registry) Dispatch(ctx context.Context, name string, rawArgs json.RawMessage) (result string) {
	defer func() {
		if rec := recover(); rec != nil {
			result = fmt.Sprintf("Tool panic: %v", rec)
		}
		result = TruncateOutput(result, r.maxOutputChars)
	}()

	t, exists := r.Get(name)
	if !exists {
		return fmt.Sprintf("Error: tool %q is not registered", name)
	}

	out, err := t.Execute(ctx, rawArgs)
	if err != nil {
		return fmt.Sprintf("Tool error: %v", err)
	}

	return out
}
