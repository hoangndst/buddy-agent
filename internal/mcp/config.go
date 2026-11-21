package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config represents the top-level configuration document structure.
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig specifies execution parameters for a single MCP server.
type ServerConfig struct {
	// Disabled allows disabling a global server in project config.
	Disabled bool `json:"disabled,omitempty"`

	// Stdio subprocess transport parameters
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workDir,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`

	// Remote HTTP/SSE transport parameters
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Context token protection filters
	IncludeTools []string `json:"includeTools,omitempty"`
	ExcludeTools []string `json:"excludeTools,omitempty"`

	// Execution timeouts
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// Source records where this server was loaded from ("global" or "project").
	Source string `json:"-"`
}

// TransportType returns the deduced transport type for this server configuration.
func (c *ServerConfig) TransportType() TransportType {
	if c.URL != "" {
		return TransportSSE
	}
	return TransportStdio
}

// LoadConfig loads and merges two-tier configuration from global (~/.buddy/mcp.json)
// and project ($PROJECT_ROOT/.buddy/mcp.json) files with environment variable expansion.
func LoadConfig(projectDir, userHome string) (*Config, error) {
	if userHome == "" {
		if h, err := os.UserHomeDir(); err == nil {
			userHome = h
		}
	}
	if projectDir == "" {
		if wd, err := os.Getwd(); err == nil {
			projectDir = wd
		}
	}

	merged := make(map[string]ServerConfig)

	// 1. Read global configuration if present
	if userHome != "" {
		globalPath := filepath.Join(userHome, ".buddy", "mcp.json")
		if globalCfg, err := loadSingleConfigFile(globalPath); err == nil && globalCfg != nil {
			for name, srv := range globalCfg.MCPServers {
				srv.Source = "global"
				normalizeServerDirs(&srv, projectDir)
				merged[name] = srv
			}
		} else if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to load global mcp config (%s): %w", globalPath, err)
		}
	}

	// 2. Read project configuration if present (overrides global)
	if projectDir != "" {
		projectPath := filepath.Join(projectDir, ".buddy", "mcp.json")
		if projectCfg, err := loadSingleConfigFile(projectPath); err == nil && projectCfg != nil {
			for name, srv := range projectCfg.MCPServers {
				srv.Source = "project"
				normalizeServerDirs(&srv, projectDir)
				merged[name] = srv
			}
		} else if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to load project mcp config (%s): %w", projectPath, err)
		}
	}

	// 3. Perform recursive environment variable expansion across merged servers
	for name, srv := range merged {
		expandServerEnv(&srv)
		merged[name] = srv
	}

	return &Config{MCPServers: merged}, nil
}

func loadSingleConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid json in %s: %w", path, err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = make(map[string]ServerConfig)
	}
	return &cfg, nil
}

func normalizeServerDirs(srv *ServerConfig, baseDir string) {
	if srv.WorkDir == "" && srv.Cwd != "" {
		srv.WorkDir = srv.Cwd
	}
	if srv.WorkDir == "" && baseDir != "" {
		srv.WorkDir = baseDir
	}
	if srv.WorkDir != "" && !filepath.IsAbs(srv.WorkDir) && baseDir != "" {
		srv.WorkDir = filepath.Clean(filepath.Join(baseDir, srv.WorkDir))
	}
	if srv.Cwd != "" && !filepath.IsAbs(srv.Cwd) && baseDir != "" {
		srv.Cwd = filepath.Clean(filepath.Join(baseDir, srv.Cwd))
	}
}

func expandServerEnv(s *ServerConfig) {
	if s.Command != "" {
		s.Command = os.ExpandEnv(s.Command)
	}
	for i, arg := range s.Args {
		s.Args[i] = os.ExpandEnv(arg)
	}
	if s.Env != nil {
		expandedEnv := make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			expandedEnv[k] = os.ExpandEnv(v)
		}
		s.Env = expandedEnv
	}
	if s.URL != "" {
		s.URL = os.ExpandEnv(s.URL)
	}
	if s.Headers != nil {
		expandedHeaders := make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			expandedHeaders[k] = os.ExpandEnv(v)
		}
		s.Headers = expandedHeaders
	}
}

// SaveConfigFile writes the given config to a file with 2-space indented JSON formatting.
func SaveConfigFile(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory %s: %w", dir, err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = make(map[string]ServerConfig)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal mcp config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write mcp config to %s: %w", path, err)
	}
	return nil
}

// SaveProjectConfig writes serialized configuration to $PROJECT_ROOT/.buddy/mcp.json.
func SaveProjectConfig(projectDir string, cfg *Config) error {
	path := filepath.Join(projectDir, ".buddy", "mcp.json")
	return SaveConfigFile(path, cfg)
}

// SaveGlobalConfig writes serialized configuration to $HOME/.buddy/mcp.json.
func SaveGlobalConfig(userHome string, cfg *Config) error {
	path := filepath.Join(userHome, ".buddy", "mcp.json")
	return SaveConfigFile(path, cfg)
}

// AddOrUpdateServerEntry adds or updates a server in the specified configuration file.
func AddOrUpdateServerEntry(path, name string, srv ServerConfig) error {
	cfg, err := loadSingleConfigFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg = &Config{MCPServers: make(map[string]ServerConfig)}
		} else {
			return err
		}
	}
	cfg.MCPServers[name] = srv
	return SaveConfigFile(path, cfg)
}

// RemoveServerEntry removes a server entry from the specified configuration file.
func RemoveServerEntry(path, name string) error {
	cfg, err := loadSingleConfigFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrServerNotFound
		}
		return err
	}
	if _, exists := cfg.MCPServers[name]; !exists {
		return ErrServerNotFound
	}
	delete(cfg.MCPServers, name)
	return SaveConfigFile(path, cfg)
}
