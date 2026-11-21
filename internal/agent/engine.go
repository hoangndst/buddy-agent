package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent/provider"
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

// WithReasoningEffort sets the reasoning effort parameter on the engine.
func WithReasoningEffort(effort string) EngineOption {
	return func(e *Engine) {
		e.reasoningEffort = effort
	}
}

// WithCompletionClient configures a custom completion client for memory compaction summarization.
func WithCompletionClient(client history.CompletionClient) EngineOption {
	return func(e *Engine) {
		e.compClient = client
	}
}

// providerCompletionAdapter bridges a ModelProvider to history.CompletionClient.
type providerCompletionAdapter struct {
	prov  provider.ModelProvider
	model string
}

func (a *providerCompletionAdapter) CreateChatCompletion(ctx context.Context, params openai.ChatCompletionNewParams, _ ...option.RequestOption) (*openai.ChatCompletion, error) {
	req := provider.GenerateRequest{
		Model:    params.Model,
		Messages: params.Messages,
	}
	resp, err := a.prov.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	return &openai.ChatCompletion{
		Choices: []openai.ChatCompletionChoice{
			{
				Message: resp.ToChatMessage(),
			},
		},
		Usage: openai.CompletionUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}, nil
}

// Engine orchestrates the ReAct loop: prompting, tool execution, and memory feedback.
type Engine struct {
	provider         provider.ModelProvider
	compClient       history.CompletionClient
	model            string
	reasoningEffort  string
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
	prov provider.ModelProvider,
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
		provider:         prov,
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
	if e.compClient == nil {
		if comp, ok := prov.(history.CompletionClient); ok {
			e.compClient = comp
		} else {
			e.compClient = &providerCompletionAdapter{prov: e.provider, model: e.model}
		}
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
	if err := session.EnsureWatermarks(ctx, e.compClient, e.model); err != nil {
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

		e.eventHandler(Event{Type: EventStepStart, Step: step, Timestamp: time.Now()})

		var genResp *provider.GenerateResponse
		var completionErr error

		// Bounded reactive recovery completion loop
		for retry := 0; retry <= maxTurnRetries; retry++ {
			genReq := provider.GenerateRequest{
				Model:           e.model,
				Messages:        session.CompileMessages(),
				Tools:           e.registry.Tools(),
				ReasoningEffort: e.reasoningEffort,
				IsLocal:         e.isLocal,
				LocalNumCtx:     session.MaxContextTokens(),
			}

			startCall := time.Now()
			resp, err := e.provider.Generate(ctx, genReq)
			callDuration := time.Since(startCall)

			if err == nil {
				if resp.Usage.PromptTokens > 0 {
					session.CalibrateUsage(resp.Usage.PromptTokens)
				}
				genResp = resp
				completionErr = nil
				break
			}

			completionErr = err
			targetTokens, matched := ParseContextLengthError(err)
			if !matched || retry == maxTurnRetries {
				e.eventHandler(Event{Type: EventError, Step: step, Duration: callDuration, Error: err})
				return "", fmt.Errorf("step %d LLM call failed: %w", step, err)
			}

			e.eventHandler(Event{
				Type:     EventContextRetrying,
				Step:     step,
				Duration: callDuration,
				Content:  fmt.Sprintf("Context limit exceeded; ratcheting down and forcing compaction (retry %d/%d)", retry+1, maxTurnRetries),
				Error:    err,
			})

			if err := session.DownscaleWatermark(targetTokens); err != nil {
				if errors.Is(err, history.ErrContextExhausted) {
					e.eventHandler(Event{Type: EventContextExhausted, Step: step, Error: err})
					return "", err
				}
				e.eventHandler(Event{Type: EventError, Step: step, Error: err})
				return "", fmt.Errorf("downscale watermark failed: %w", err)
			}

			e.eventHandler(Event{Type: EventCompactionStart, Step: step})
			if err := session.ForceCompaction(ctx, e.compClient, e.model); err != nil {
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
			e.eventHandler(Event{Type: EventCompactionDone, Step: step})
		}

		if completionErr != nil {
			return "", completionErr
		}

		// Emit reasoning event if model produced chain-of-thought tokens
		if genResp.ReasoningContent != "" {
			e.eventHandler(Event{
				Type:      EventReasoning,
				Step:      step,
				Timestamp: time.Now(),
				Content:   genResp.ReasoningContent,
				Protocol:  genResp.Protocol,
				Usage:     genResp.Usage,
			})
		}

		// If no tools called, task has concluded
		if len(genResp.ToolCalls) == 0 {
			session.CommitSimpleAssistant(genResp.ToChatMessage())
			e.eventHandler(Event{
				Type:      EventTextComplete,
				Step:      step,
				Timestamp: time.Now(),
				Content:   genResp.Content,
				Protocol:  genResp.Protocol,
				Usage:     genResp.Usage,
			})
			e.eventHandler(Event{
				Type:      EventFinished,
				Step:      step,
				Timestamp: time.Now(),
				Content:   genResp.Content,
				Protocol:  genResp.Protocol,
				Usage:     genResp.Usage,
			})
			return genResp.Content, nil
		}

		// Emit conversational text complete before tool calls if content is present
		if genResp.Content != "" {
			e.eventHandler(Event{
				Type:      EventTextComplete,
				Step:      step,
				Timestamp: time.Now(),
				Content:   genResp.Content,
				Protocol:  genResp.Protocol,
				Usage:     genResp.Usage,
			})
		}

		// Dispatch tool calls and accumulate atomically
		toolResults := make([]history.ToolResult, 0, len(genResp.ToolCalls))
		cumulativeBytes := 0
		maxTurnBytes := session.MaxTurnOutputBytes()

		for _, tc := range genResp.ToolCalls {
			e.eventHandler(Event{
				Type:       EventToolCall,
				Step:       step,
				Timestamp:  time.Now(),
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				ToolArgs:   tc.Arguments,
				Protocol:   genResp.Protocol,
			})

			// Telemetry hook: track file access for StateTracker
			if tc.Name == "write_file" || tc.Name == "edit_file" {
				var args struct{ Path string }
				if json.Unmarshal([]byte(tc.Arguments), &args) == nil && args.Path != "" {
					session.RecordFileAccess(args.Path, "modified")
				}
			}

			startTool := time.Now()
			result := e.registry.Dispatch(ctx, tc.Name, json.RawMessage(tc.Arguments))
			toolDuration := time.Since(startTool)
			exitCode := parseToolExitCode(result)

			// Telemetry hook: track execution errors for StateTracker
			if strings.HasPrefix(result, "Tool error:") || strings.Contains(result, "exit status") || strings.Contains(result, "exit code") || strings.Contains(result, "Exit Code") {
				session.RecordError(fmt.Sprintf("%s: %s", tc.Name, result))
			}

			// Invariant refresh: reload system anchor if durable notes were updated
			if tc.Name == "save_memory" {
				if refreshedPrompt, err := e.memoryMgr.LoadSystemPrompt(); err == nil {
					session.ReloadSystemAnchor(refreshedPrompt)
				}
			}

			// Headroom protection: cap cumulative tool outputs within turn
			cumulativeBytes += len(result)
			if cumulativeBytes > maxTurnBytes {
				exitSuffix := ""
				for _, line := range strings.Split(result, "\n") {
					lower := strings.ToLower(line)
					if strings.Contains(lower, "exit code") || strings.Contains(lower, "exit status") {
						exitSuffix = " | " + strings.TrimSpace(line)
						break
					}
				}
				result = "[Output truncated: cumulative turn output limit reached]" + exitSuffix
			}

			e.eventHandler(Event{
				Type:         EventToolResult,
				Step:         step,
				Timestamp:    time.Now(),
				Duration:     toolDuration,
				ToolCallID:   tc.ID,
				ToolName:     tc.Name,
				ToolResult:   result,
				ToolExitCode: exitCode,
				Protocol:     genResp.Protocol,
			})

			toolResults = append(toolResults, history.ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    result,
			})
		}

		// Atomic turn block commit
		session.CommitToolTurn(genResp.ToChatMessage(), toolResults)

		// Watermark check: execute staged micro-pruning or macro-distillation
		preTokens := session.CurrentTokens()
		if preTokens >= int(float64(session.MaxContextTokens())*0.70) {
			e.eventHandler(Event{Type: EventCompactionStart, Step: step, Timestamp: time.Now()})
		}
		if err := session.EnsureWatermarks(ctx, e.compClient, e.model); err != nil {
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
		if session.CurrentTokens() < preTokens {
			e.eventHandler(Event{Type: EventCompactionDone, Step: step, Timestamp: time.Now()})
		}
	}

	maxStepErr := fmt.Errorf("reached maximum step budget (%d) without finishing", e.maxSteps)
	e.eventHandler(Event{Type: EventError, Error: maxStepErr})
	return "", maxStepErr
}

// parseToolExitCode extracts a process exit code from tool output or defaults to 0 on success.
func parseToolExitCode(output string) int {
	if strings.Contains(output, "exit status ") {
		parts := strings.Split(output, "exit status ")
		if len(parts) > 1 {
			var code int
			if _, err := fmt.Sscanf(parts[1], "%d", &code); err == nil {
				return code
			}
		}
		return 1
	}
	if strings.HasPrefix(output, "Tool error:") || strings.HasPrefix(output, "Error:") || strings.HasPrefix(output, "error:") {
		return -1
	}
	return 0
}
