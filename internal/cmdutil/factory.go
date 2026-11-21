package cmdutil

import (
	"os"

	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/config"
	"github.com/hoangndst/buddy-agent/internal/mcp"
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
	NewModelProvider func(cfg *config.Config, client *openai.Client) provider.ModelProvider
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
	}
}
