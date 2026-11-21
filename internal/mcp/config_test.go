package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_TwoTierMergeAndPrecedence(t *testing.T) {
	tempDir := t.TempDir()
	userHome := filepath.Join(tempDir, "home")
	projectDir := filepath.Join(tempDir, "project")

	// 1. Create global config
	globalJSON := `{
  "mcpServers": {
    "sqlite": {
      "command": "global-sqlite",
      "args": ["--mode", "read-only"]
    },
    "fetch": {
      "command": "global-fetch",
      "args": ["--timeout", "30"]
    }
  }
}`
	globalPath := filepath.Join(userHome, ".buddy", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(globalPath), 0755); err != nil {
		t.Fatalf("failed to create global dir: %v", err)
	}
	if err := os.WriteFile(globalPath, []byte(globalJSON), 0644); err != nil {
		t.Fatalf("failed to write global json: %v", err)
	}

	// 2. Create project config
	projectJSON := `{
  "mcpServers": {
    "sqlite": {
      "command": "project-sqlite",
      "args": ["--path", "./app.db"]
    },
    "postgres": {
      "url": "http://localhost:8080/sse"
    }
  }
}`
	projectPath := filepath.Join(projectDir, ".buddy", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}
	if err := os.WriteFile(projectPath, []byte(projectJSON), 0644); err != nil {
		t.Fatalf("failed to write project json: %v", err)
	}

	// 3. Load merged configuration
	cfg, err := LoadConfig(projectDir, userHome)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(cfg.MCPServers) != 3 {
		t.Fatalf("expected 3 servers, got %d", len(cfg.MCPServers))
	}

	// Verify project override
	sqlite := cfg.MCPServers["sqlite"]
	if sqlite.Command != "project-sqlite" || sqlite.Source != "project" {
		t.Errorf("expected project-sqlite override with source 'project', got command=%q, source=%q", sqlite.Command, sqlite.Source)
	}

	// Verify global inheritance
	fetch := cfg.MCPServers["fetch"]
	if fetch.Command != "global-fetch" || fetch.Source != "global" {
		t.Errorf("expected global-fetch with source 'global', got command=%q, source=%q", fetch.Command, fetch.Source)
	}

	// Verify project-only addition
	postgres := cfg.MCPServers["postgres"]
	if postgres.URL != "http://localhost:8080/sse" || postgres.Source != "project" {
		t.Errorf("expected postgres with source 'project', got url=%q, source=%q", postgres.URL, postgres.Source)
	}
	if postgres.TransportType() != TransportSSE {
		t.Errorf("expected TransportSSE, got %v", postgres.TransportType())
	}
	if sqlite.TransportType() != TransportStdio {
		t.Errorf("expected TransportStdio, got %v", sqlite.TransportType())
	}
}

func TestLoadConfig_EnvExpansion(t *testing.T) {
	tempDir := t.TempDir()
	userHome := filepath.Join(tempDir, "home")
	projectDir := filepath.Join(tempDir, "project")

	t.Setenv("TEST_BIN_DIR", "/usr/local/bin")
	t.Setenv("TEST_DB_NAME", "production.db")
	t.Setenv("TEST_API_KEY", "secret-token-12345")
	t.Setenv("TEST_REMOTE_HOST", "api.example.com")

	projectJSON := `{
  "mcpServers": {
    "expanded": {
      "command": "${TEST_BIN_DIR}/my-mcp",
      "args": ["--db", "$TEST_DB_NAME"],
      "env": {
        "API_KEY": "${TEST_API_KEY}"
      },
      "url": "https://${TEST_REMOTE_HOST}/mcp",
      "headers": {
        "Authorization": "Bearer ${TEST_API_KEY}"
      }
    }
  }
}`
	projectPath := filepath.Join(projectDir, ".buddy", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}
	if err := os.WriteFile(projectPath, []byte(projectJSON), 0644); err != nil {
		t.Fatalf("failed to write project json: %v", err)
	}

	cfg, err := LoadConfig(projectDir, userHome)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	srv, ok := cfg.MCPServers["expanded"]
	if !ok {
		t.Fatal("expected 'expanded' server entry")
	}

	if srv.Command != "/usr/local/bin/my-mcp" {
		t.Errorf("expected command '/usr/local/bin/my-mcp', got %q", srv.Command)
	}
	if len(srv.Args) != 2 || srv.Args[1] != "production.db" {
		t.Errorf("expected args ['--db', 'production.db'], got %v", srv.Args)
	}
	if srv.Env["API_KEY"] != "secret-token-12345" {
		t.Errorf("expected env API_KEY='secret-token-12345', got %q", srv.Env["API_KEY"])
	}
	if srv.URL != "https://api.example.com/mcp" {
		t.Errorf("expected URL 'https://api.example.com/mcp', got %q", srv.URL)
	}
	if srv.Headers["Authorization"] != "Bearer secret-token-12345" {
		t.Errorf("expected header 'Bearer secret-token-12345', got %q", srv.Headers["Authorization"])
	}
}

func TestLoadConfig_DisabledFlag(t *testing.T) {
	tempDir := t.TempDir()
	userHome := filepath.Join(tempDir, "home")
	projectDir := filepath.Join(tempDir, "project")

	globalJSON := `{
  "mcpServers": {
    "disabled_server": {
      "command": "global-cmd",
      "disabled": false
    }
  }
}`
	projectJSON := `{
  "mcpServers": {
    "disabled_server": {
      "command": "global-cmd",
      "disabled": true
    }
  }
}`
	globalPath := filepath.Join(userHome, ".buddy", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(globalPath), 0755)
	_ = os.WriteFile(globalPath, []byte(globalJSON), 0644)

	projectPath := filepath.Join(projectDir, ".buddy", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(projectPath), 0755)
	_ = os.WriteFile(projectPath, []byte(projectJSON), 0644)

	cfg, err := LoadConfig(projectDir, userHome)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	srv := cfg.MCPServers["disabled_server"]
	if !srv.Disabled {
		t.Errorf("expected disabled_server to have Disabled=true, got %v", srv.Disabled)
	}
}

func TestAddOrUpdateAndRemoveServerEntry(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "subdir", ".buddy", "mcp.json")

	// Add stdio server
	err := AddOrUpdateServerEntry(configPath, "sqlite", ServerConfig{
		Command: "uvx",
		Args:    []string{"mcp-server-sqlite", "--db-path", "./test.db"},
	})
	if err != nil {
		t.Fatalf("AddOrUpdateServerEntry failed: %v", err)
	}

	// Verify file saved
	loaded, err := loadSingleConfigFile(configPath)
	if err != nil {
		t.Fatalf("loadSingleConfigFile failed: %v", err)
	}
	if len(loaded.MCPServers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(loaded.MCPServers))
	}
	if loaded.MCPServers["sqlite"].Command != "uvx" {
		t.Errorf("expected command uvx, got %q", loaded.MCPServers["sqlite"].Command)
	}

	// Add remote SSE server
	err = AddOrUpdateServerEntry(configPath, "remote-docs", ServerConfig{
		URL:     "https://mcp.example.com/sse",
		Headers: map[string]string{"Authorization": "Bearer token123"},
	})
	if err != nil {
		t.Fatalf("AddOrUpdateServerEntry failed: %v", err)
	}

	loaded, err = loadSingleConfigFile(configPath)
	if err != nil {
		t.Fatalf("loadSingleConfigFile failed: %v", err)
	}
	if len(loaded.MCPServers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(loaded.MCPServers))
	}

	// Remove server
	err = RemoveServerEntry(configPath, "sqlite")
	if err != nil {
		t.Fatalf("RemoveServerEntry failed: %v", err)
	}

	loaded, err = loadSingleConfigFile(configPath)
	if err != nil {
		t.Fatalf("loadSingleConfigFile failed: %v", err)
	}
	if len(loaded.MCPServers) != 1 {
		t.Fatalf("expected 1 server after removal, got %d", len(loaded.MCPServers))
	}
	if _, exists := loaded.MCPServers["sqlite"]; exists {
		t.Errorf("expected sqlite to be removed")
	}

	// Remove non-existent returns ErrServerNotFound
	err = RemoveServerEntry(configPath, "non-existent")
	if err != ErrServerNotFound {
		t.Errorf("expected ErrServerNotFound, got %v", err)
	}
}

func TestLoadConfig_MalformedJSON(t *testing.T) {
	tempDir := t.TempDir()
	userHome := filepath.Join(tempDir, "home")
	projectDir := filepath.Join(tempDir, "project")

	projectPath := filepath.Join(projectDir, ".buddy", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(projectPath), 0755)
	_ = os.WriteFile(projectPath, []byte("{invalid json syntax}"), 0644)

	_, err := LoadConfig(projectDir, userHome)
	if err == nil {
		t.Fatal("expected error on malformed json, got nil")
	}
}

func TestLoadConfig_WorkDirInheritance(t *testing.T) {
	tempDir := t.TempDir()
	userHome := filepath.Join(tempDir, "home")
	projectDir := filepath.Join(tempDir, "project")

	projectJSON := `{
  "mcpServers": {
    "default-wd": {
      "command": "sh",
      "args": ["-c", "pwd"]
    },
    "custom-wd": {
      "command": "sh",
      "args": ["-c", "pwd"],
      "workDir": "/custom/path"
    }
  }
}`
	projectPath := filepath.Join(projectDir, ".buddy", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(projectPath), 0755)
	_ = os.WriteFile(projectPath, []byte(projectJSON), 0644)

	cfg, err := LoadConfig(projectDir, userHome)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	defSrv := cfg.MCPServers["default-wd"]
	if defSrv.WorkDir != projectDir {
		t.Errorf("expected default-wd WorkDir to be %q, got %q", projectDir, defSrv.WorkDir)
	}
	customSrv := cfg.MCPServers["custom-wd"]
	if customSrv.WorkDir != "/custom/path" {
		t.Errorf("expected custom-wd WorkDir to be '/custom/path', got %q", customSrv.WorkDir)
	}

	cmd, _, _, err := buildStdioCommand(defSrv)
	if err != nil {
		t.Fatalf("buildStdioCommand failed: %v", err)
	}
	if cmd.Dir != projectDir {
		t.Errorf("expected cmd.Dir to be %q, got %q", projectDir, cmd.Dir)
	}
}

func TestLoadConfig_RelativeWorkDirResolution(t *testing.T) {
	tempDir := t.TempDir()
	projectDir := filepath.Join(tempDir, "project")

	projectJSON := `{
  "mcpServers": {
    "relative-tool": {
      "command": "node",
      "args": ["server.js"],
      "workDir": "./custom/tools",
      "cwd": "scripts"
    }
  }
}`
	projectPath := filepath.Join(projectDir, ".buddy", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(projectPath), 0755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}
	if err := os.WriteFile(projectPath, []byte(projectJSON), 0644); err != nil {
		t.Fatalf("failed to write project json: %v", err)
	}

	cfg, err := LoadConfig(projectDir, "")
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	srv, ok := cfg.MCPServers["relative-tool"]
	if !ok {
		t.Fatal("expected relative-tool in MCPServers")
	}

	expectedWorkDir := filepath.Join(projectDir, "custom", "tools")
	if srv.WorkDir != expectedWorkDir {
		t.Errorf("expected normalized absolute WorkDir %q, got %q", expectedWorkDir, srv.WorkDir)
	}

	expectedCwd := filepath.Join(projectDir, "scripts")
	if srv.Cwd != expectedCwd {
		t.Errorf("expected normalized absolute Cwd %q, got %q", expectedCwd, srv.Cwd)
	}
}
