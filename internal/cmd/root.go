// Package cmd provides the root Cobra command and execution loop for buddy-agent.
package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/client"
	daemoncmd "github.com/hoangndst/buddy-agent/internal/cmd/daemon"
	"github.com/hoangndst/buddy-agent/internal/cmd/doc"
	mcpcmd "github.com/hoangndst/buddy-agent/internal/cmd/mcp"
	sessioncmd "github.com/hoangndst/buddy-agent/internal/cmd/session"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	core "github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/hoangndst/buddy-agent/internal/memory"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/spf13/cobra"
)

var (
	thoughtStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Italic(true)
	toolStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	resultStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	doneStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	errStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
)

// NewRootCmd constructs the primary buddy-agent root command tree.
func NewRootCmd(f *cmdutil.Factory) *cobra.Command {
	var standalone bool
	var socketPath string

	rootCmd := &cobra.Command{
		Use:   "buddy [task]",
		Short: "buddy-agent is an autonomous software engineering agent",
		Long: `buddy-agent executes autonomous software engineering tasks via ReAct loops,
three-partition context memory compaction, and Model Context Protocol (MCP) integrations.`,
		SilenceUsage: true,
		Args:         cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) > 0 {
				return runTask(c.Context(), f, args, standalone, socketPath)
			}
			return runREPL(c.Context(), f)
		},
	}

	rootCmd.PersistentFlags().BoolVar(&standalone, "standalone", false, "Run task in-process without connecting to the background daemon")
	rootCmd.PersistentFlags().StringVar(&socketPath, "socket", "", "Path to the Unix domain socket")

	rootCmd.AddCommand(mcpcmd.NewCmdMCP(f))
	rootCmd.AddCommand(doc.NewCmdDoc(f))
	rootCmd.AddCommand(sessioncmd.NewCmdSession(f))
	rootCmd.AddCommand(NewCmdResume(f))
	rootCmd.AddCommand(daemoncmd.NewCmdDaemon(f))
	rootCmd.AddCommand(NewCmdAttach(f))

	return rootCmd
}

// formatToolPreview produces clean, human-readable tool invocation previews.
func formatToolPreview(name, rawArgs string) string {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(rawArgs), &args); err == nil {
		switch name {
		case "bash":
			if cmd, ok := args["command"].(string); ok && cmd != "" {
				return fmt.Sprintf("⚙ bash: %s", cmd)
			}
		case "read_file":
			if p, ok := args["path"].(string); ok && p != "" {
				return fmt.Sprintf("⚙ read_file: %s", p)
			}
		case "write_file":
			if p, ok := args["path"].(string); ok && p != "" {
				return fmt.Sprintf("⚙ write_file: %s", p)
			}
		case "edit_file":
			if p, ok := args["path"].(string); ok && p != "" {
				return fmt.Sprintf("⚙ edit_file: %s", p)
			}
		case "save_memory":
			if topic, ok := args["topic"].(string); ok && topic != "" {
				return fmt.Sprintf("⚙ save_memory: %s", topic)
			}
		case "create_skill":
			if n, ok := args["name"].(string); ok && n != "" {
				return fmt.Sprintf("⚙ create_skill: %s", n)
			}
		default:
			for _, key := range []string{"path", "query", "command", "url", "name", "id", "prompt"} {
				if val, ok := args[key].(string); ok && val != "" {
					return fmt.Sprintf("⚙ %s: %s", name, val)
				}
			}
		}
	}
	return fmt.Sprintf("⚙ %s(%s)", name, rawArgs)
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

	var prov provider.ModelProvider
	if f.NewModelProvider != nil {
		prov = f.NewModelProvider(cfg, &client)
	} else {
		prov = provider.NewAutoProvider(
			provider.NewChatCompletionsProvider(&client),
			provider.NewResponsesProvider(&client),
			cfg.IsLocal,
		)
	}

	// Initialize Memory & Native Tools
	memMgr := memory.NewManager(cfg.WorkspaceDir)
	reg := tools.NewRegistry(cfg.MaxOutputChars)

	reg.Register(tools.NewBashTool(cfg.WorkspaceDir, 60*time.Second, tools.WithMaxOutputBytes(cfg.MaxOutputChars)))
	reg.Register(tools.NewReadFileTool(cfg.WorkspaceDir))
	reg.Register(tools.NewListDirectoryTool(cfg.WorkspaceDir))
	reg.Register(tools.NewSearchFilesTool(cfg.WorkspaceDir))
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
			// Unobtrusive: raw step banners eliminated for clean interactive REPL UX
		case agent.EventReasoning:
			styledReasoning := thoughtStyle.Render(fmt.Sprintf("💭 Thinking:\n   %s", strings.ReplaceAll(strings.TrimSpace(ev.Content), "\n", "\n   ")))
			_, _ = fmt.Fprintln(f.IOStreams.Out, styledReasoning)
		case agent.EventTextComplete:
			_, _ = fmt.Fprintln(f.IOStreams.Out, ev.Content)
		case agent.EventToolCall:
			preview := formatToolPreview(ev.ToolName, ev.ToolArgs)
			if ev.Step > 1 {
				preview = fmt.Sprintf("⚙ [Step %d] %s", ev.Step, strings.TrimPrefix(preview, "⚙ "))
			}
			_, _ = fmt.Fprintln(f.IOStreams.Out, toolStyle.Render(preview))
		case agent.EventToolResult:
			trimmed := strings.TrimSpace(ev.ToolResult)
			if len(trimmed) > 300 {
				trimmed = trimmed[:300] + "..."
			}
			if ev.ToolExitCode > 0 {
				_, _ = fmt.Fprintln(f.IOStreams.Out, resultStyle.Render(fmt.Sprintf("→ (exit %d) %s", ev.ToolExitCode, trimmed)))
			} else {
				_, _ = fmt.Fprintln(f.IOStreams.Out, resultStyle.Render(fmt.Sprintf("→ %s", trimmed)))
			}
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

	// Initialize Persistence Store
	var store persistence.Store
	var storeCleanup func()
	if f.Store != nil {
		s, sCleanup, sErr := f.Store()
		if sErr == nil && s != nil {
			store = s
			storeCleanup = sCleanup
		}
	}

	combinedCleanup := func() {
		cleanupFunc()
		if storeCleanup != nil {
			storeCleanup()
		}
	}

	// Register InvokeSubagentTool on the root engine (depth = 0)
	subTool := agent.NewInvokeSubagentTool(agent.SubagentToolConfig{
		Depth:            0,
		ParentRegistry:   reg,
		Provider:         prov,
		Model:            cfg.Model,
		WorkspaceDir:     cfg.WorkspaceDir,
		ReasoningEffort:  cfg.ReasoningEffort,
		Store:            store,
		EventHandler:     eventHandler,
		IsLocal:          cfg.IsLocal,
		MaxContextTokens: cfg.MaxContextTokens,
	})
	reg.Register(subTool)

	engine := agent.NewEngine(
		prov,
		cfg.Model,
		cfg.MaxSteps,
		reg,
		memMgr,
		eventHandler,
		agent.WithMaxContextTokens(cfg.MaxContextTokens),
		agent.WithWorkingTailSize(cfg.WorkingTailSize),
		agent.WithIsLocal(cfg.IsLocal),
		agent.WithReasoningEffort(cfg.ReasoningEffort),
		agent.WithStore(store),
		agent.WithWorkspaceDir(cfg.WorkspaceDir),
	)

	return engine, combinedCleanup, nil
}

func runTask(ctx context.Context, f *cmdutil.Factory, args []string, standalone bool, socketPath string) error {
	task := strings.Join(args, " ")
	if !standalone {
		// Attempt to execute via background daemon
		cl, err := client.ConnectOrSpawn(socketPath)
		if err == nil {
			defer func() { _ = cl.Close() }()
			cfg, _ := f.ConfigLoader()
			workspaceDir := ""
			model := ""
			maxSteps := 0
			maxContextTokens := 0
			reasoningEffort := ""
			baseURL := ""
			apiKey := ""
			apiType := ""
			if cfg != nil {
				workspaceDir = cfg.WorkspaceDir
				model = cfg.Model
				maxSteps = cfg.MaxSteps
				maxContextTokens = cfg.MaxContextTokens
				reasoningEffort = cfg.ReasoningEffort
				baseURL = cfg.BaseURL
				apiKey = cfg.APIKey
				apiType = cfg.APIType
			}
			sessionID, startErr := cl.StartSession(protocol.SessionStartParams{
				Task:             task,
				WorkspaceDir:     workspaceDir,
				Model:            model,
				MaxSteps:         maxSteps,
				MaxContextTokens: maxContextTokens,
				ReasoningEffort:  reasoningEffort,
				BaseURL:          baseURL,
				APIKey:           apiKey,
				APIType:          apiType,
			})
			if startErr != nil {
				// Avoid ambiguous double-execution: if daemon connection succeeded but start failed, abort immediately
				return fmt.Errorf("failed to start daemon session: %w", startErr)
			}
			_, _ = fmt.Fprintf(f.IOStreams.Out, "Dispatched task to daemon (Session ID: %s)\n", sessionID)
			return streamClientEvents(ctx, f, cl, sessionID)
		}
		// Graceful fallback to standalone ONLY if daemon connection/spawn failed before request submission
		_, _ = fmt.Fprintln(f.IOStreams.Out, "Notice: Daemon unavailable; running task in-process.")
	}

	engine, cleanup, err := setupEngine(ctx, f)
	if err != nil {
		return err
	}
	defer cleanup()

	_, _ = fmt.Fprintf(f.IOStreams.Out, "Executing task: %s\n", task)
	_, err = engine.Run(ctx, task)
	return err
}

func streamClientEvents(ctx context.Context, f *cmdutil.Factory, cl *client.Client, sessionID string) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	inputScanner := bufio.NewScanner(f.IOStreams.In)
	permCh := cl.Permissions()
	eventsCh := cl.Events()

	for {
		select {
		case <-sigCh:
			_, _ = fmt.Fprintf(f.IOStreams.Out, "\nDetaching from session %s. Task continues in background.\nRun 'buddy attach %s' to reconnect.\n", sessionID, sessionID)
			_ = cl.DetachSession(sessionID)
			return nil

		case <-ctx.Done():
			_, _ = fmt.Fprintf(f.IOStreams.Out, "\nDetaching from session %s. Task continues in background.\nRun 'buddy attach %s' to reconnect.\n", sessionID, sessionID)
			_ = cl.DetachSession(sessionID)
			return nil

		case req, ok := <-permCh:
			if !ok {
				permCh = nil
				continue
			}
			_, _ = fmt.Fprintf(f.IOStreams.Out, "\n%s Tool %s requests permission with arguments: %+v\nAllow? [y/N]: ",
				toolStyle.Render("⚡ [PERMISSION REQUIRED]"), req.Tool, req.Arguments)
			decision := "deny"
			if inputScanner.Scan() {
				text := strings.TrimSpace(strings.ToLower(inputScanner.Text()))
				if text == "y" || text == "yes" {
					decision = "allow"
				}
			}
			_ = cl.RespondPermission(req.RequestID, decision)

		case evt, ok := <-eventsCh:
			if !ok {
				return fmt.Errorf("connection to daemon closed unexpectedly before session completed")
			}
			renderEvent(f, evt)
			if evt.Type == string(agent.EventFinished) {
				return nil
			}
			if evt.Type == string(agent.EventError) {
				isTerminal, _ := evt.Data["terminal"].(bool)
				if isTerminal {
					errMsg := evt.Content
					if errMsg == "" && evt.Data != nil {
						if e, ok := evt.Data["error"].(string); ok {
							errMsg = e
						}
					}
					return fmt.Errorf("task failed: %s", errMsg)
				}
			}
		}
	}
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
