// Package config manages runtime configuration loading from environment variables and .env files.
package config

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/joho/godotenv"
)

// Config holds runtime configuration for buddy-agent.
type Config struct {
	BaseURL        string
	APIKey         string
	Model          string
	MaxSteps       int
	MaxOutputChars int
	WorkspaceDir   string
	Debug          bool
}

// Load loads configuration from environment variables, optionally reading from .env if present.
func Load() (*Config, error) {
	_ = godotenv.Load()

	workspace := getEnv("BUDDY_WORKSPACE", ".")
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		absWorkspace = workspace
	}

	maxSteps, err := strconv.Atoi(getEnv("BUDDY_MAX_STEPS", "30"))
	if err != nil || maxSteps <= 0 {
		maxSteps = 30
	}

	maxOutputChars, err := strconv.Atoi(getEnv("BUDDY_MAX_OUTPUT_CHARS", "10000"))
	if err != nil || maxOutputChars <= 0 {
		maxOutputChars = 10000
	}

	debug, _ := strconv.ParseBool(getEnv("BUDDY_DEBUG", "false"))

	return &Config{
		BaseURL:        getEnv("OPENAI_BASE_URL", "http://localhost:11434/v1"),
		APIKey:         getEnv("OPENAI_API_KEY", ""),
		Model:          getEnv("BUDDY_MODEL", "qwen2.5-coder:7b"),
		MaxSteps:       maxSteps,
		MaxOutputChars: maxOutputChars,
		WorkspaceDir:   absWorkspace,
		Debug:          debug,
	}, nil
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}
