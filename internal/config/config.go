// Package config manages runtime configuration loading from environment variables and .env files.
package config

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

const (
	// DefaultRemoteContextTokens is the default context limit for cloud endpoints.
	DefaultRemoteContextTokens = 64000
	// DefaultLocalContextTokens is the default context limit for local/loopback endpoints to protect GPU VRAM.
	DefaultLocalContextTokens = 16384
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
	IsLocal          bool
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
	baseURL := getEnv("BUDDY_BASE_URL", getEnv("OPENAI_BASE_URL", "http://localhost:11434/v1"))
	apiKey := getEnv("BUDDY_API_KEY", getEnv("OPENAI_API_KEY", ""))

	maxContextTokens := resolveContextCeiling(os.Getenv("BUDDY_MAX_CONTEXT_TOKENS"), baseURL)

	workingTailSize, err := strconv.Atoi(getEnv("BUDDY_WORKING_TAIL_SIZE", "5"))
	if err != nil || workingTailSize <= 0 {
		workingTailSize = 5
	}

	debug, _ := strconv.ParseBool(getEnv("BUDDY_DEBUG", "false"))
	isLocal := IsLocalEndpoint(baseURL)

	return &Config{
		BaseURL:          baseURL,
		APIKey:           apiKey,
		Model:            model,
		MaxSteps:         maxSteps,
		MaxOutputChars:   maxOutputChars,
		MaxContextTokens: maxContextTokens,
		WorkingTailSize:  workingTailSize,
		WorkspaceDir:     absWorkspace,
		Debug:            debug,
		IsLocal:          isLocal,
	}, nil
}

// resolveContextCeiling determines the baseline context window ceiling.
// Explicit BUDDY_MAX_CONTEXT_TOKENS takes absolute precedence if defined and > 0.
// Otherwise, local loopbacks/Ollama ports default to 16,384 tokens, and remote endpoints default to 64,000.
func resolveContextCeiling(envVal, baseURL string) int {
	if envVal != "" {
		if tokens, err := strconv.Atoi(envVal); err == nil && tokens > 0 {
			return tokens
		}
	}
	if IsLocalEndpoint(baseURL) {
		return DefaultLocalContextTokens
	}
	return DefaultRemoteContextTokens
}

// IsLocalEndpoint checks whether a base URL points to a local machine or Ollama daemon.
func IsLocalEndpoint(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		lower := strings.ToLower(rawURL)
		return strings.Contains(lower, "localhost") || strings.Contains(lower, "127.0.0.1") || strings.Contains(lower, "::1") || strings.Contains(lower, "11434")
	}

	host := parsed.Hostname()
	port := parsed.Port()

	if port == "11434" {
		return true
	}

	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		return true
	}

	return false
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok && val != "" {
		return val
	}
	return defaultVal
}
