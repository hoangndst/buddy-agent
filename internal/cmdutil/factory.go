package cmdutil

import (
	"os"
	"path/filepath"

	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/config"
	"github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/openai/openai-go"
)

// Factory provides dependency injection for CLI commands, enabling decoupled
// testing of configuration, IO streams, and runtime client managers.
type Factory struct {
	IOStreams        *IOStreams
	ConfigLoader     func() (*config.Config, error)
	MCPConfigLoader  func(projectDir, userHome string) (*mcp.Config, error)
	WorkDir          func() (string, error)
	UserHome         func() (string, error)
	NewClientManager func(cfg *mcp.Config) *mcp.ClientManager
	NewModelProvider  func(cfg *config.Config, client *openai.Client) provider.ModelProvider
	Store             func() (persistence.Store, func(), error)
	StoreForWorkspace func(wsDir string) (persistence.Store, func(), error)
}

// NewFactory constructs a default production command factory.
func NewFactory() *Factory {
	return &Factory{
		IOStreams:       SystemIOStreams(),
		ConfigLoader:    config.Load,
		MCPConfigLoader: mcp.LoadConfig,
		WorkDir:         os.Getwd,
		UserHome:        os.UserHomeDir,
		NewClientManager: func(cfg *mcp.Config) *mcp.ClientManager {
			return mcp.NewClientManager(cfg)
		},
		NewModelProvider: func(cfg *config.Config, client *openai.Client) provider.ModelProvider {
			switch cfg.APIType {
			case "chat":
				return provider.NewChatCompletionsProvider(client)
			case "responses":
				return provider.NewResponsesProvider(client)
			default: // "auto"
				chat := provider.NewChatCompletionsProvider(client)
				resp := provider.NewResponsesProvider(client)
				return provider.NewAutoProvider(chat, resp, cfg.IsLocal)
			}
		},
		Store: func() (persistence.Store, func(), error) {
			cfg, err := config.Load()
			if err != nil {
				return nil, nil, err
			}
			dbPath := cfg.DBPath
			if dbPath == "" {
				ws := cfg.WorkspaceDir
				if ws == "" {
					ws = "."
				}
				dbPath = filepath.Join(ws, ".buddy", "history.db")
			}
			store, err := persistence.NewSQLiteStore(dbPath)
			if err != nil {
				if home, homeErr := os.UserHomeDir(); homeErr == nil {
					fallbackPath := filepath.Join(home, ".buddy", "history.db")
					store, err = persistence.NewSQLiteStore(fallbackPath)
				}
				if err != nil {
					return nil, nil, err
				}
			}
			cleanup := func() {
				_ = store.Close()
			}
			return store, cleanup, nil
		},
		StoreForWorkspace: func(wsDir string) (persistence.Store, func(), error) {
			cfg, err := config.Load()
			if err != nil {
				return nil, nil, err
			}
			dbPath := cfg.DBPath
			if dbPath == "" {
				ws := wsDir
				if ws == "" {
					ws = cfg.WorkspaceDir
				}
				if ws == "" {
					ws = "."
				}
				dbPath = filepath.Join(ws, ".buddy", "history.db")
			}
			store, err := persistence.NewSQLiteStore(dbPath)
			if err != nil {
				if home, homeErr := os.UserHomeDir(); homeErr == nil {
					fallbackPath := filepath.Join(home, ".buddy", "history.db")
					store, err = persistence.NewSQLiteStore(fallbackPath)
				}
				if err != nil {
					return nil, nil, err
				}
			}
			cleanup := func() {
				_ = store.Close()
			}
			return store, cleanup, nil
		},
	}
}
