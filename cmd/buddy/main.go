// Package main provides the command-line interface and REPL for buddy-agent.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/config"
	"github.com/hoangndst/buddy-agent/internal/memory"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

var (
	stepStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	thoughtStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Italic(true)
	toolStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	resultStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	doneStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	errStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Setup graceful cancellation
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Initialize LLM Client with official openai-go SDK
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithRequestTimeout(120 * time.Second),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	client := openai.NewClient(opts...)
	clientAdapter := agent.NewClientAdapter(&client)

	// Initialize Memory & Tools
	memMgr := memory.NewManager(cfg.WorkspaceDir)
	reg := tools.NewRegistry(cfg.MaxOutputChars)

	reg.Register(tools.NewBashTool(cfg.WorkspaceDir, 60*time.Second, tools.WithMaxOutputBytes(cfg.MaxOutputChars)))
	reg.Register(tools.NewReadFileTool(cfg.WorkspaceDir))
	reg.Register(tools.NewWriteFileTool(cfg.WorkspaceDir))
	reg.Register(tools.NewEditFileTool(cfg.WorkspaceDir))
	reg.Register(tools.NewSaveMemoryTool(memMgr))
	reg.Register(tools.NewCreateSkillTool(memMgr))

	// Event listener for terminal output
	eventHandler := func(ev agent.Event) {
		switch ev.Type {
		case agent.EventStepStart:
			fmt.Println(stepStyle.Render(fmt.Sprintf("\n[Step %d]", ev.Step)))
		case agent.EventThought:
			fmt.Println(thoughtStyle.Render(ev.Content))
		case agent.EventToolCall:
			fmt.Println(toolStyle.Render(fmt.Sprintf("⚙ %s(%s)", ev.ToolName, ev.ToolArgs)))
		case agent.EventToolResult:
			trimmed := strings.TrimSpace(ev.ToolResult)
			if len(trimmed) > 300 {
				trimmed = trimmed[:300] + "..."
			}
			fmt.Println(resultStyle.Render(fmt.Sprintf("→ %s", trimmed)))
		case agent.EventFinished:
			fmt.Println(doneStyle.Render("\n✓ Done"))
		case agent.EventError:
			fmt.Println(errStyle.Render(fmt.Sprintf("\n✖ Error: %v", ev.Error)))
		}
	}

	engine := agent.NewEngine(clientAdapter, cfg.Model, cfg.MaxSteps, reg, memMgr, eventHandler)

	// If argument provided, run once
	if len(os.Args) > 1 {
		task := strings.Join(os.Args[1:], " ")
		fmt.Printf("Executing task: %s\n", task)
		_, err := engine.Run(ctx, task)
		if err != nil {
			os.Exit(1)
		}
		return
	}

	// Interactive REPL Mode
	fmt.Println(doneStyle.Render("=== buddy-agent CLI ready ==="))
	fmt.Printf("Model: %s | Workspace: %s\n", cfg.Model, cfg.WorkspaceDir)
	fmt.Println("Enter your prompt (or 'exit' to quit):")

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\nbuddy> ")
		if !scanner.Scan() {
			break
		}
		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}
		if input == "exit" || input == "quit" {
			break
		}

		_, _ = engine.Run(ctx, input)
	}
}
