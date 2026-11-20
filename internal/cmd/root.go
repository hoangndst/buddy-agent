// Package cmd provides the root Cobra command and execution loop for buddy-agent.
package cmd

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/cmd/doc"
	mcpcmd "github.com/hoangndst/buddy-agent/internal/cmd/mcp"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	core "github.com/hoangndst/buddy-agent/internal/mcp"
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

// NewRootCmd constructs the primary buddy-agent root command tree.
func NewRootCmd(f *cmdutil.Factory) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "buddy [task]",
		Short: "buddy-agent is an autonomous software engineering agent",
		Long: `buddy-agent executes autonomous software engineering tasks via ReAct loops,
three-partition context memory compaction, and Model Context Protocol (MCP) integrations.`,
		SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) > 0 {
				return runTask(c.Context(), f, args)
			}
			return runREPL(c.Context(), f)
		},
	}

	rootCmd.AddCommand(mcpcmd.NewCmdMCP(f))
	rootCmd.AddCommand(doc.NewCmdDoc(f))

	return rootCmd
}

func setupEngine(ctx context.Context, f *cmdutil.Factory) (*agent.Engine, func(), error) {
	cfg, err := f.ConfigLoader()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load configuration: %w", err)
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
	cleanupFunc := func() {}
	wd, _ := f.WorkDir()
	home, _ := f.UserHome()
	mcpCfg, mcpErr := f.MCPConfigLoader(wd, home)
	if mcpErr == nil && mcpCfg != nil && len(mcpCfg.MCPServers) > 0 {
		mcpMgr := f.NewClientManager(mcpCfg)
		_ = mcpMgr.StartAll(ctx)
		cleanupFunc = func() {
			_ = mcpMgr.Close()
		}

		for _, t := range mcpMgr.Tools() {
			reg.Register(t)
		}
		reg.Register(core.NewResourceListerTool(mcpMgr))
		reg.Register(core.NewResourceReaderTool(mcpMgr))
	}

	// Terminal event listener rendering via IOStreams
	eventHandler := func(ev agent.Event) {
		switch ev.Type {
		case agent.EventStepStart:
			_, _ = fmt.Fprintln(f.IOStreams.Out, stepStyle.Render(fmt.Sprintf("\n[Step %d]", ev.Step)))
		case agent.EventThought:
			_, _ = fmt.Fprintln(f.IOStreams.Out, thoughtStyle.Render(ev.Content))
		case agent.EventToolCall:
			_, _ = fmt.Fprintln(f.IOStreams.Out, toolStyle.Render(fmt.Sprintf("⚙ %s(%s)", ev.ToolName, ev.ToolArgs)))
		case agent.EventToolResult:
			trimmed := strings.TrimSpace(ev.ToolResult)
			if len(trimmed) > 300 {
				trimmed = trimmed[:300] + "..."
			}
			_, _ = fmt.Fprintln(f.IOStreams.Out, resultStyle.Render(fmt.Sprintf("→ %s", trimmed)))
		case agent.EventFinished:
			_, _ = fmt.Fprintln(f.IOStreams.Out, doneStyle.Render("\n✓ Done"))
		case agent.EventCompactionStart:
			_, _ = fmt.Fprintln(f.IOStreams.Out, thoughtStyle.Render("⚡ Compacting context history..."))
		case agent.EventCompactionDone:
			_, _ = fmt.Fprintln(f.IOStreams.Out, thoughtStyle.Render("✓ Context compaction complete"))
		case agent.EventContextRetrying:
			_, _ = fmt.Fprintln(f.IOStreams.Out, thoughtStyle.Render(fmt.Sprintf("⚡ Context limit reached; self-healing with compaction (step %d)...", ev.Step)))
		case agent.EventContextExhausted:
			_, _ = fmt.Fprintln(f.IOStreams.ErrOut, errStyle.Render(fmt.Sprintf("\n✖ Context Exhausted: %v", ev.Error)))
		case agent.EventError:
			_, _ = fmt.Fprintln(f.IOStreams.ErrOut, errStyle.Render(fmt.Sprintf("\n✖ Error: %v", ev.Error)))
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

	return engine, cleanupFunc, nil
}

func runTask(ctx context.Context, f *cmdutil.Factory, args []string) error {
	engine, cleanup, err := setupEngine(ctx, f)
	if err != nil {
		return err
	}
	defer cleanup()

	task := strings.Join(args, " ")
	_, _ = fmt.Fprintf(f.IOStreams.Out, "Executing task: %s\n", task)
	_, err = engine.Run(ctx, task)
	return err
}

func runREPL(ctx context.Context, f *cmdutil.Factory) error {
	engine, cleanup, err := setupEngine(ctx, f)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg, _ := f.ConfigLoader()
	model := "unknown"
	workspace := "."
	if cfg != nil {
		model = cfg.Model
		workspace = cfg.WorkspaceDir
	}

	_, _ = fmt.Fprintln(f.IOStreams.Out, doneStyle.Render("=== buddy-agent CLI ready ==="))
	_, _ = fmt.Fprintf(f.IOStreams.Out, "Model: %s | Workspace: %s\n", model, workspace)
	_, _ = fmt.Fprintln(f.IOStreams.Out, "Enter your prompt (or 'exit' to quit):")

	scanner := bufio.NewScanner(f.IOStreams.In)
	for {
		_, _ = fmt.Fprint(f.IOStreams.Out, "\nbuddy> ")
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
}
