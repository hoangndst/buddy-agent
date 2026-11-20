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
	"github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/hoangndst/buddy-agent/internal/memory"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/spf13/cobra"
)

var (
	stepStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	thoughtStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Italic(true)
	toolStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	resultStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	doneStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	errStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
)

func newRootCmd(ctx context.Context) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "buddy [task]",
		Short: "buddy-agent is an autonomous software engineering agent",
		Long:  "buddy-agent executes autonomous software engineering tasks via ReAct loops, context compaction, and Model Context Protocol (MCP) tool integration.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load configuration: %w", err)
			}

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

			// Initialize Memory & Native Tools
			memMgr := memory.NewManager(cfg.WorkspaceDir)
			reg := tools.NewRegistry(cfg.MaxOutputChars)

			reg.Register(tools.NewBashTool(cfg.WorkspaceDir, 60*time.Second, tools.WithMaxOutputBytes(cfg.MaxOutputChars)))
			reg.Register(tools.NewReadFileTool(cfg.WorkspaceDir))
			reg.Register(tools.NewWriteFileTool(cfg.WorkspaceDir))
			reg.Register(tools.NewEditFileTool(cfg.WorkspaceDir))
			reg.Register(tools.NewSaveMemoryTool(memMgr))
			reg.Register(tools.NewCreateSkillTool(memMgr))

			// Initialize MCP Client Engine and discover tools
			wd, _ := os.Getwd()
			home, _ := os.UserHomeDir()
			mcpCfg, mcpErr := mcp.LoadConfig(wd, home)
			if mcpErr == nil && mcpCfg != nil && len(mcpCfg.MCPServers) > 0 {
				mcpMgr := mcp.NewClientManager(mcpCfg)
				_ = mcpMgr.StartAll(ctx)
				defer func() {
					_ = mcpMgr.Close()
				}()

				for _, t := range mcpMgr.Tools() {
					reg.Register(t)
				}
				reg.Register(mcp.NewResourceListerTool(mcpMgr))
				reg.Register(mcp.NewResourceReaderTool(mcpMgr))
			}

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
				case agent.EventCompactionStart:
					fmt.Println(thoughtStyle.Render("⚡ Compacting context history..."))
				case agent.EventCompactionDone:
					fmt.Println(thoughtStyle.Render("✓ Context compaction complete"))
				case agent.EventContextRetrying:
					fmt.Println(thoughtStyle.Render(fmt.Sprintf("⚡ Context limit reached; self-healing with compaction (step %d)...", ev.Step)))
				case agent.EventContextExhausted:
					fmt.Println(errStyle.Render(fmt.Sprintf("\n✖ Context Exhausted: %v", ev.Error)))
				case agent.EventError:
					fmt.Println(errStyle.Render(fmt.Sprintf("\n✖ Error: %v", ev.Error)))
				}
			}

			engine := agent.NewEngine(
				clientAdapter,
				cfg.Model,
				cfg.MaxSteps,
				reg,
				memMgr,
				eventHandler,
				agent.WithMaxContextTokens(cfg.MaxContextTokens),
				agent.WithWorkingTailSize(cfg.WorkingTailSize),
				agent.WithIsLocal(cfg.IsLocal),
			)

			// If argument provided, run once
			if len(args) > 0 {
				task := strings.Join(args, " ")
				fmt.Printf("Executing task: %s\n", task)
				_, err := engine.Run(ctx, task)
				if err != nil {
					return err
				}
				return nil
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
			return nil
		},
	}

	rootCmd.AddCommand(newMCPCmd(ctx))
	return rootCmd
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	rootCmd := newRootCmd(ctx)
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		log.Fatal(err)
	}
}
