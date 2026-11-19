package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hoangndst/buddy-agent/internal/history"
	"github.com/hoangndst/buddy-agent/internal/memory"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// EngineOption configures operational parameters for the Agent Engine.
type EngineOption func(*Engine)

// WithMaxContextTokens sets the hard context token ceiling on the engine.
func WithMaxContextTokens(tokens int) EngineOption {
	return func(e *Engine) {
		if tokens > 0 {
			e.maxContextTokens = tokens
		}
	}
}

// WithWorkingTailSize sets the unpruned active turn tail size on the engine.
func WithWorkingTailSize(size int) EngineOption {
	return func(e *Engine) {
		if size > 0 {
			e.workingTailSize = size
		}
	}
}

// WithIsLocal configures whether the engine is communicating with a local LLM endpoint (e.g. Ollama).
func WithIsLocal(isLocal bool) EngineOption {
	return func(e *Engine) {
		e.isLocal = isLocal
	}
}

// Engine orchestrates the ReAct loop: prompting, tool execution, and memory feedback.
type Engine struct {
	client           ChatClient
	model            string
	maxSteps         int
	maxContextTokens int
	workingTailSize  int
	isLocal          bool
	registry         *tools.Registry
	memoryMgr        *memory.Manager
	eventHandler     EventHandler
}

// NewEngine creates a new Agent Engine.
func NewEngine(
	client ChatClient,
	model string,
	maxSteps int,
	reg *tools.Registry,
	mem *memory.Manager,
	handler EventHandler,
	opts ...EngineOption,
) *Engine {
	if maxSteps <= 0 {
		maxSteps = 30
	}
	if handler == nil {
		handler = func(Event) {}
	}
	e := &Engine{
		client:           client,
		model:            model,
		maxSteps:         maxSteps,
		maxContextTokens: 64000,
		workingTailSize:  5,
		isLocal:          false,
		registry:         reg,
		memoryMgr:        mem,
		eventHandler:     handler,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
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

	session := history.NewSession(
		systemPrompt,
		task,
		history.WithMaxContextTokens(e.maxContextTokens),
		history.WithWorkingTailSize(e.workingTailSize),
	)

	// Guard against oversized initial task or system prompt exceeding context ceiling
	if err := session.EnsureWatermarks(ctx, e.client, e.model); err != nil {
		if errors.Is(err, history.ErrContextExhausted) {
			e.eventHandler(Event{Type: EventContextExhausted, Step: 0, Error: err})
			return "", err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		e.eventHandler(Event{Type: EventError, Step: 0, Error: err})
		return "", fmt.Errorf("initial context validation failed: %w", err)
	}

	const maxTurnRetries = 2

	// 2. Main ReAct execution loop
	for step := 1; step <= e.maxSteps; step++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		e.eventHandler(Event{Type: EventStepStart, Step: step})

		var choice openai.ChatCompletionChoice
		var completionErr error

		// Bounded reactive recovery completion loop
		for retry := 0; retry <= maxTurnRetries; retry++ {
			params := openai.ChatCompletionNewParams{
				Model:    e.model,
				Messages: session.CompileMessages(),
				Tools:    e.registry.OpenAITools(),
			}

			// Local endpoint num_ctx injection (Ollama)
			reqOpts := make([]option.RequestOption, 0, 1)
			if e.isLocal {
				reqOpts = append(reqOpts, option.WithJSONSet("options.num_ctx", session.MaxContextTokens()))
			}

			resp, err := e.client.CreateChatCompletion(ctx, params, reqOpts...)
			if err == nil {
				if len(resp.Choices) == 0 {
					err := errors.New("model returned empty choices")
					e.eventHandler(Event{Type: EventError, Step: step, Error: err})
					return "", err
				}

				if resp.Usage.PromptTokens > 0 {
					session.CalibrateUsage(resp.Usage.PromptTokens)
				}
				choice = resp.Choices[0]
				completionErr = nil
				break
			}

			completionErr = err
			targetTokens, matched := ParseContextLengthError(err)
			if !matched || retry == maxTurnRetries {
				e.eventHandler(Event{Type: EventError, Step: step, Error: err})
				return "", fmt.Errorf("step %d LLM call failed: %w", step, err)
			}

			e.eventHandler(Event{
				Type:    EventContextRetrying,
				Step:    step,
				Content: fmt.Sprintf("Context limit exceeded; ratcheting down and forcing compaction (retry %d/%d)", retry+1, maxTurnRetries),
				Error:   err,
			})

			if err := session.DownscaleWatermark(targetTokens); err != nil {
				if errors.Is(err, history.ErrContextExhausted) {
					e.eventHandler(Event{Type: EventContextExhausted, Step: step, Error: err})
					return "", err
				}
				e.eventHandler(Event{Type: EventError, Step: step, Error: err})
				return "", fmt.Errorf("downscale watermark failed: %w", err)
			}

			if err := session.ForceCompaction(ctx, e.client, e.model); err != nil {
				if errors.Is(err, history.ErrContextExhausted) {
					e.eventHandler(Event{Type: EventContextExhausted, Step: step, Error: err})
					return "", err
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return "", err
				}
				e.eventHandler(Event{Type: EventError, Step: step, Error: err})
				return "", fmt.Errorf("force compaction failed: %w", err)
			}
		}

		if completionErr != nil {
			return "", completionErr
		}

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
			session.CommitSimpleAssistant(choice.Message)
			e.eventHandler(Event{
				Type:    EventFinished,
				Step:    step,
				Content: choice.Message.Content,
			})
			return choice.Message.Content, nil
		}

		// Dispatch tool calls and accumulate atomically
		toolResults := make([]history.ToolResult, 0, len(choice.Message.ToolCalls))
		cumulativeBytes := 0
		maxTurnBytes := session.MaxTurnOutputBytes()

		for _, tc := range choice.Message.ToolCalls {
			e.eventHandler(Event{
				Type:     EventToolCall,
				Step:     step,
				ToolName: tc.Function.Name,
				ToolArgs: tc.Function.Arguments,
			})

			// Telemetry hook: track file access for StateTracker
			if tc.Function.Name == "write_file" || tc.Function.Name == "edit_file" {
				var args struct{ Path string }
				if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil && args.Path != "" {
					session.RecordFileAccess(args.Path, "modified")
				}
			}

			result := e.registry.Dispatch(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))

			// Telemetry hook: track execution errors for StateTracker
			if strings.HasPrefix(result, "Tool error:") || strings.Contains(result, "exit status") || strings.Contains(result, "exit code") {
				session.RecordError(fmt.Sprintf("%s: %s", tc.Function.Name, result))
			}

			// Invariant refresh: reload system anchor if durable notes were updated
			if tc.Function.Name == "save_memory" {
				if refreshedPrompt, err := e.memoryMgr.LoadSystemPrompt(); err == nil {
					session.ReloadSystemAnchor(refreshedPrompt)
				}
			}

			// Headroom protection: cap cumulative tool outputs within turn
			cumulativeBytes += len(result)
			if cumulativeBytes > maxTurnBytes {
				exitSuffix := ""
				if idx := strings.LastIndex(result, "exit code"); idx != -1 {
					exitSuffix = " | " + strings.TrimSpace(result[idx:])
				} else if idx := strings.LastIndex(result, "exit status"); idx != -1 {
					exitSuffix = " | " + strings.TrimSpace(result[idx:])
				}
				result = "[Output truncated: cumulative turn output limit reached]" + exitSuffix
			}

			e.eventHandler(Event{
				Type:       EventToolResult,
				Step:       step,
				ToolName:   tc.Function.Name,
				ToolResult: result,
			})

			toolResults = append(toolResults, history.ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
		}

		// Atomic turn block commit
		session.CommitToolTurn(choice.Message, toolResults)

		// Watermark check: execute staged micro-pruning or macro-distillation
		if err := session.EnsureWatermarks(ctx, e.client, e.model); err != nil {
			if errors.Is(err, history.ErrContextExhausted) {
				e.eventHandler(Event{Type: EventContextExhausted, Step: step, Error: err})
				return "", err
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", err
			}
			e.eventHandler(Event{Type: EventError, Step: step, Error: err})
			return "", fmt.Errorf("context compaction failed: %w", err)
		}
	}

	maxStepErr := fmt.Errorf("reached maximum step budget (%d) without finishing", e.maxSteps)
	e.eventHandler(Event{Type: EventError, Error: maxStepErr})
	return "", maxStepErr
}
