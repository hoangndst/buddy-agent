package cmdutil

import (
	"os"

	"github.com/hoangndst/buddy-agent/internal/config"
	"github.com/hoangndst/buddy-agent/internal/mcp"
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
	}
}
