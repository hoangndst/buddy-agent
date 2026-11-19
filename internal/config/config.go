// Package config manages runtime configuration loading from environment variables and .env files.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config holds runtime configuration for buddy-agent.
type Config struct {
	BaseURL          string
	APIKey           string
	Model            string
	MaxSteps         int
	MaxOutputChars   int
	MaxContextTokens int
	WorkingTailSize  int
	WorkspaceDir     string
	Debug            bool
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

	model := getEnv("BUDDY_MODEL", "qwen2.5-coder:7b")

	maxContextTokens, err := strconv.Atoi(getEnv("BUDDY_MAX_CONTEXT_TOKENS", ""))
	if err != nil || maxContextTokens <= 0 {
		maxContextTokens = inferModelContextLimit(model)
	}

	workingTailSize, err := strconv.Atoi(getEnv("BUDDY_WORKING_TAIL_SIZE", "5"))
	if err != nil || workingTailSize <= 0 {
		workingTailSize = 5
	}

	debug, _ := strconv.ParseBool(getEnv("BUDDY_DEBUG", "false"))

	return &Config{
		BaseURL:          getEnv("OPENAI_BASE_URL", "http://localhost:11434/v1"),
		APIKey:           getEnv("OPENAI_API_KEY", ""),
		Model:            model,
		MaxSteps:         maxSteps,
		MaxOutputChars:   maxOutputChars,
		MaxContextTokens: maxContextTokens,
		WorkingTailSize:  workingTailSize,
		WorkspaceDir:     absWorkspace,
		Debug:            debug,
	}, nil
}

// inferModelContextLimit estimates context window capacity based on model name heuristics.
func inferModelContextLimit(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "128k") || strings.Contains(m, "gpt-4o"):
		return 64000 // Conservative 64k default ceiling within 128k window
	case strings.Contains(m, "64k"):
		return 65536
	case strings.Contains(m, "32k"):
		return 32768
	case strings.Contains(m, "16k"):
		return 16384
	case strings.Contains(m, "8k"):
		return 8192
	default:
		return 64000
	}
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}
