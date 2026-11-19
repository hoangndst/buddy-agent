package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hoangndst/buddy-agent/internal/memory"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
)

// Engine orchestrates the ReAct loop: prompting, tool execution, and memory feedback.
type Engine struct {
	client       ChatClient
	model        string
	maxSteps     int
	registry     *tools.Registry
	memoryMgr    *memory.Manager
	eventHandler EventHandler
}

// NewEngine creates a new Agent Engine.
func NewEngine(
	client ChatClient,
	model string,
	maxSteps int,
	reg *tools.Registry,
	mem *memory.Manager,
	handler EventHandler,
) *Engine {
	if maxSteps <= 0 {
		maxSteps = 30
	}
	if handler == nil {
		handler = func(Event) {}
	}
	return &Engine{
		client:       client,
		model:        model,
		maxSteps:     maxSteps,
		registry:     reg,
		memoryMgr:    mem,
		eventHandler: handler,
	}
}

// Run executes a multi-step task to completion or until max steps are exhausted.
func (e *Engine) Run(ctx context.Context, task string) (string, error) {
	if task == "" {
		return "", errors.New("task cannot be empty")
	}

	// 1. Build initial system prompt from memory and skills
	systemPrompt, err := e.memoryMgr.LoadSystemPrompt()
	if err != nil {
		return "", fmt.Errorf("failed to load system prompt: %w", err)
	}

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(systemPrompt),
		openai.UserMessage(task),
	}

	// 2. Main ReAct execution loop
	for step := 1; step <= e.maxSteps; step++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		e.eventHandler(Event{Type: EventStepStart, Step: step})

		params := openai.ChatCompletionNewParams{
			Model:    e.model,
			Messages: messages,
			Tools:    e.registry.OpenAITools(),
		}

		resp, err := e.client.CreateChatCompletion(ctx, params)
		if err != nil {
			e.eventHandler(Event{Type: EventError, Step: step, Error: err})
			return "", fmt.Errorf("step %d LLM call failed: %w", step, err)
		}

		if len(resp.Choices) == 0 {
			err := errors.New("model returned empty choices")
			e.eventHandler(Event{Type: EventError, Step: step, Error: err})
			return "", err
		}

		choice := resp.Choices[0]
		messages = append(messages, choice.Message.ToParam())

		// Emit thought if model generated reasoning or explanation
		if choice.Message.Content != "" {
			e.eventHandler(Event{
				Type:    EventThought,
				Step:    step,
				Content: choice.Message.Content,
			})
		}

		// If no tools called, the agent has finished its task
		if len(choice.Message.ToolCalls) == 0 {
			e.eventHandler(Event{
				Type:    EventFinished,
				Step:    step,
				Content: choice.Message.Content,
			})
			return choice.Message.Content, nil
		}

		// Dispatch tool calls
		for _, tc := range choice.Message.ToolCalls {
			e.eventHandler(Event{
				Type:     EventToolCall,
				Step:     step,
				ToolName: tc.Function.Name,
				ToolArgs: tc.Function.Arguments,
			})

			result := e.registry.Dispatch(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))

			e.eventHandler(Event{
				Type:       EventToolResult,
				Step:       step,
				ToolName:   tc.Function.Name,
				ToolResult: result,
			})

			messages = append(messages, openai.ToolMessage(result, tc.ID))
		}
	}

	maxStepErr := fmt.Errorf("reached maximum step budget (%d) without finishing", e.maxSteps)
	e.eventHandler(Event{Type: EventError, Error: maxStepErr})
	return "", maxStepErr
}
