package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/client"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/config"
	"github.com/hoangndst/buddy-agent/internal/daemon"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	core "github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/hoangndst/buddy-agent/internal/memory"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/spf13/cobra"
)

func newCmdStart(f *cmdutil.Factory) *cobra.Command {
	var socketPath string
	var foreground bool
	var detach bool

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the buddy-agent background daemon",
		RunE: func(c *cobra.Command, _ []string) error {
			if socketPath == "" {
				socketPath = daemon.GetDefaultSocketPath()
			}

			if !foreground {
				// Spawn detached background process
				execPath, err := os.Executable()
				if err != nil {
					execPath = os.Args[0]
				}

				cmdArgs := []string{"daemon", "start", "--foreground"}
				if socketPath != "" {
					cmdArgs = append(cmdArgs, "--socket", socketPath)
				}

				proc := exec.Command(execPath, cmdArgs...)
				// Detach from current process group
				client.SetDetachedProcess(proc)
				if err := proc.Start(); err != nil {
					return fmt.Errorf("failed to start background daemon: %w", err)
				}

				// Wait for socket to become active
				deadline := time.Now().Add(2500 * time.Millisecond)
				for time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
					cl, err := client.Connect(socketPath)
					if err == nil {
						_ = cl.Close()
						_, _ = fmt.Fprintf(f.IOStreams.Out, "Daemon started in background (PID: %d, socket: %s)\n", proc.Process.Pid, socketPath)
						return nil
					}
				}
				return fmt.Errorf("daemon started (PID: %d) but socket %s did not become ready", proc.Process.Pid, socketPath)
			}

			// Foreground execution mode
			sockMgr := daemon.NewSocketManager(socketPath)

			engineFactory := func(sessionID string, params protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
				return buildEngineForSession(c.Context(), f, sessionID, params, emitHandler)
			}

			sessionMgr := daemon.NewSessionManager(engineFactory)
			server := daemon.NewServer(sockMgr, sessionMgr)

			if err := server.Start(); err != nil {
				return fmt.Errorf("daemon failed to start: %w", err)
			}

			_, _ = fmt.Fprintf(f.IOStreams.Out, "Daemon listening on %s (PID: %d)\n", socketPath, os.Getpid())

			// Wait for termination signal or programmatic server shutdown
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			select {
			case sig := <-sigCh:
				_, _ = fmt.Fprintf(f.IOStreams.Out, "\nReceived signal %s; shutting down daemon...\n", sig)
				return server.Stop()
			case <-server.Done():
				_, _ = fmt.Fprintln(f.IOStreams.Out, "Daemon stopped via RPC.")
				return nil
			}
		},
	}

	cmd.Flags().StringVar(&socketPath, "socket", "", "Path to the Unix domain socket")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "Run daemon in the foreground")
	cmd.Flags().BoolVar(&detach, "detach", false, "Run daemon detached in the background (default)")

	return cmd
}

func buildEngineForSession(
	ctx context.Context,
	f *cmdutil.Factory,
	sessionID string,
	params protocol.SessionStartParams,
	emitHandler agent.EventHandler,
) (*agent.Engine, func(), error) {
	cfg, err := f.ConfigLoader()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	apiKey := cfg.APIKey
	if params.APIKey != nil {
		apiKey = *params.APIKey
	} else if params.BaseURL != "" && params.BaseURL != cfg.BaseURL {
		apiKey = ""
	}
	baseURL := cfg.BaseURL
	if params.BaseURL != "" {
		baseURL = params.BaseURL
	}
	isLocal := cfg.IsLocal
	if params.BaseURL != "" {
		isLocal = config.IsLocalEndpoint(baseURL)
	}
	apiType := cfg.APIType
	if params.APIType != "" {
		apiType = params.APIType
	}

	model := cfg.Model
	if params.Model != "" {
		model = params.Model
	}
	maxSteps := cfg.MaxSteps
	if params.MaxSteps > 0 {
		maxSteps = params.MaxSteps
	}
	effort := cfg.ReasoningEffort
	if params.ReasoningEffort != nil {
		effort = *params.ReasoningEffort
	}
	maxCtx := cfg.MaxContextTokens
	if params.MaxContextTokens > 0 {
		maxCtx = params.MaxContextTokens
	} else if params.BaseURL != "" && isLocal != cfg.IsLocal {
		if isLocal {
			maxCtx = config.DefaultLocalContextTokens
		} else {
			maxCtx = config.DefaultRemoteContextTokens
		}
	}

	wsDir := cfg.WorkspaceDir
	if params.WorkspaceDir != "" {
		wsDir = params.WorkspaceDir
	}

	sessionCfg := *cfg
	sessionCfg.APIKey = apiKey
	sessionCfg.BaseURL = baseURL
	sessionCfg.IsLocal = isLocal
	sessionCfg.Model = model
	sessionCfg.MaxSteps = maxSteps
	sessionCfg.ReasoningEffort = effort
	sessionCfg.MaxContextTokens = maxCtx
	sessionCfg.WorkspaceDir = wsDir
	sessionCfg.APIType = apiType

	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithRequestTimeout(120 * time.Second),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	llmClient := openai.NewClient(opts...)

	var prov provider.ModelProvider
	if f.NewModelProvider != nil {
		prov = f.NewModelProvider(&sessionCfg, &llmClient)
	} else {
		switch apiType {
		case "chat":
			prov = provider.NewChatCompletionsProvider(&llmClient)
		case "responses":
			prov = provider.NewResponsesProvider(&llmClient)
		default:
			prov = provider.NewAutoProvider(
				provider.NewChatCompletionsProvider(&llmClient),
				provider.NewResponsesProvider(&llmClient),
				isLocal,
			)
		}
	}

	memMgr := memory.NewManager(wsDir)
	reg := tools.NewRegistry(cfg.MaxOutputChars)

	reg.Register(tools.NewBashTool(wsDir, 60*time.Second, tools.WithMaxOutputBytes(cfg.MaxOutputChars)))
	reg.Register(tools.NewReadFileTool(wsDir))
	reg.Register(tools.NewListDirectoryTool(wsDir))
	reg.Register(tools.NewSearchFilesTool(wsDir))
	reg.Register(tools.NewWriteFileTool(wsDir))
	reg.Register(tools.NewEditFileTool(wsDir))
	reg.Register(tools.NewSaveMemoryTool(memMgr))
	reg.Register(tools.NewCreateSkillTool(memMgr))

	var cleanups []func()

	var mcpCfg *core.Config
	var mcpErr error
	if params.MCPConfig != nil {
		mcpCfg = params.MCPConfig
	} else if f.MCPConfigLoader != nil {
		home, _ := f.UserHome()
		mcpCfg, mcpErr = f.MCPConfigLoader(wsDir, home)
	}
	if mcpErr == nil && mcpCfg != nil && len(mcpCfg.MCPServers) > 0 {
		for name, srv := range mcpCfg.MCPServers {
			if srv.WorkDir == "" && srv.Cwd != "" {
				srv.WorkDir = srv.Cwd
			}
			if srv.WorkDir == "" && wsDir != "" {
				srv.WorkDir = wsDir
			}
			if srv.WorkDir != "" && !filepath.IsAbs(srv.WorkDir) && wsDir != "" {
				srv.WorkDir = filepath.Clean(filepath.Join(wsDir, srv.WorkDir))
			}
			if srv.Cwd != "" && !filepath.IsAbs(srv.Cwd) && wsDir != "" {
				srv.Cwd = filepath.Clean(filepath.Join(wsDir, srv.Cwd))
			}
			mcpCfg.MCPServers[name] = srv
		}
		mcpMgr := f.NewClientManager(mcpCfg)
		_ = mcpMgr.StartAll(ctx)
		cleanups = append(cleanups, func() {
			_ = mcpMgr.Close()
		})
		for _, t := range mcpMgr.Tools() {
			reg.Register(t)
		}
		reg.Register(core.NewResourceListerTool(mcpMgr))
		reg.Register(core.NewResourceReaderTool(mcpMgr))
	}

	var store persistence.Store
	if f.StoreForWorkspace != nil {
		s, closer, sErr := f.StoreForWorkspace(wsDir, params.DBPath)
		if sErr == nil && s != nil {
			store = s
			if closer != nil {
				cleanups = append(cleanups, func() {
					closer()
				})
			}
		}
	} else if f.Store != nil {
		s, closer, sErr := f.Store()
		if sErr == nil && s != nil {
			store = s
			if closer != nil {
				cleanups = append(cleanups, func() {
					closer()
				})
			}
		}
	}

	subTool := agent.NewInvokeSubagentTool(agent.SubagentToolConfig{
		Depth:            0,
		ParentSessionID:  sessionID,
		ParentRegistry:   reg,
		Provider:         prov,
		Model:            model,
		WorkspaceDir:     wsDir,
		ReasoningEffort:  effort,
		Store:            store,
		EventHandler:     emitHandler,
		IsLocal:          isLocal,
		MaxContextTokens: maxCtx,
	})
	reg.Register(subTool)

	eng := agent.NewEngine(
		prov,
		model,
		maxSteps,
		reg,
		memMgr,
		emitHandler,
		agent.WithMaxContextTokens(maxCtx),
		agent.WithWorkingTailSize(cfg.WorkingTailSize),
		agent.WithIsLocal(isLocal),
		agent.WithReasoningEffort(effort),
		agent.WithStore(store),
		agent.WithWorkspaceDir(wsDir),
		agent.WithSessionIDOverride(sessionID),
	)

	cleanup := func() {
		for _, c := range cleanups {
			c()
		}
	}

	return eng, cleanup, nil
}
