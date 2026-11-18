// Package tools provides the tool registry, execution sandbox, and built-in operations.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sashabaranov/go-openai"
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

// OpenAITools converts registered tools into the OpenAI Tool format.
func (r *Registry) OpenAITools() []openai.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	res := make([]openai.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		res = append(res, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return res
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
