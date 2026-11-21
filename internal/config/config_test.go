package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	os.Clearenv()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error loading config: %v", err)
	}

	if cfg.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("expected default BaseURL http://localhost:11434/v1, got %s", cfg.BaseURL)
	}
	if cfg.APIType != "auto" {
		t.Errorf("expected default APIType auto, got %s", cfg.APIType)
	}
	if cfg.ReasoningEffort != "" {
		t.Errorf("expected default ReasoningEffort empty, got %s", cfg.ReasoningEffort)
	}
	if cfg.MaxSteps != 30 {
		t.Errorf("expected default MaxSteps 30, got %d", cfg.MaxSteps)
	}
	if cfg.MaxOutputChars != 10000 {
		t.Errorf("expected default MaxOutputChars 10000, got %d", cfg.MaxOutputChars)
	}
	if cfg.MaxContextTokens != DefaultLocalContextTokens {
		t.Errorf("expected default MaxContextTokens %d, got %d", DefaultLocalContextTokens, cfg.MaxContextTokens)
	}
	if !cfg.IsLocal {
		t.Errorf("expected default IsLocal true for localhost:11434, got false")
	}
	if cfg.Debug {
		t.Errorf("expected default Debug false, got true")
	}
}

func TestLoadCustom(t *testing.T) {
	os.Clearenv()
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("BUDDY_MODEL", "gpt-4o-mini")
	t.Setenv("BUDDY_MAX_STEPS", "15")
	t.Setenv("BUDDY_MAX_OUTPUT_CHARS", "5000")
	t.Setenv("BUDDY_DEBUG", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error loading custom config: %v", err)
	}

	if cfg.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("expected custom BaseURL, got %s", cfg.BaseURL)
	}
	if cfg.APIKey != "test-key" {
		t.Errorf("expected custom APIKey, got %s", cfg.APIKey)
	}
	if cfg.Model != "gpt-4o-mini" {
		t.Errorf("expected custom Model, got %s", cfg.Model)
	}
	if cfg.MaxSteps != 15 {
		t.Errorf("expected custom MaxSteps 15, got %d", cfg.MaxSteps)
	}
	if cfg.MaxOutputChars != 5000 {
		t.Errorf("expected custom MaxOutputChars 5000, got %d", cfg.MaxOutputChars)
	}
	if cfg.MaxContextTokens != DefaultRemoteContextTokens {
		t.Errorf("expected custom remote MaxContextTokens %d, got %d", DefaultRemoteContextTokens, cfg.MaxContextTokens)
	}
	if cfg.IsLocal {
		t.Errorf("expected IsLocal false for remote endpoint, got true")
	}
	if !cfg.Debug {
		t.Errorf("expected Debug true, got false")
	}
}

func TestLoadCustom_BuddyEnvPrecedence(t *testing.T) {
	os.Clearenv()
	// Generic fallbacks
	t.Setenv("OPENAI_BASE_URL", "https://generic-openai.com/v1")
	t.Setenv("OPENAI_API_KEY", "generic-key")

	// Scoped BUDDY_* variables take precedence
	t.Setenv("BUDDY_BASE_URL", "http://localhost:11434/v1")
	t.Setenv("BUDDY_API_KEY", "buddy-scoped-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error loading config: %v", err)
	}

	if cfg.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("expected BUDDY_BASE_URL to take precedence, got %s", cfg.BaseURL)
	}
	if cfg.APIKey != "buddy-scoped-key" {
		t.Errorf("expected BUDDY_API_KEY to take precedence, got %s", cfg.APIKey)
	}
	if !cfg.IsLocal {
		t.Errorf("expected IsLocal true for localhost, got false")
	}
}

func TestLoadCustom_APITypeAndReasoningEffort(t *testing.T) {
	tests := []struct {
		name            string
		envAPIType      string
		envEffort       string
		expectedAPIType string
		expectedEffort  string
	}{
		{
			name:            "Valid lowercase chat and medium",
			envAPIType:      "chat",
			envEffort:       "medium",
			expectedAPIType: "chat",
			expectedEffort:  "medium",
		},
		{
			name:            "Whitespace and uppercase normalization",
			envAPIType:      "  RESPONSES  ",
			envEffort:       "  HIGH  ",
			expectedAPIType: "responses",
			expectedEffort:  "high",
		},
		{
			name:            "Invalid values fallback to defaults",
			envAPIType:      "invalid_protocol",
			envEffort:       "ultra_high",
			expectedAPIType: "auto",
			expectedEffort:  "",
		},
		{
			name:            "Explicit none reasoning effort",
			envAPIType:      "auto",
			envEffort:       "none",
			expectedAPIType: "auto",
			expectedEffort:  "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()
			t.Setenv("BUDDY_API_TYPE", tt.envAPIType)
			t.Setenv("BUDDY_REASONING_EFFORT", tt.envEffort)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error loading config: %v", err)
			}
			if cfg.APIType != tt.expectedAPIType {
				t.Errorf("expected APIType %q, got %q", tt.expectedAPIType, cfg.APIType)
			}
			if cfg.ReasoningEffort != tt.expectedEffort {
				t.Errorf("expected ReasoningEffort %q, got %q", tt.expectedEffort, cfg.ReasoningEffort)
			}
		})
	}
}

func TestResolveContextCeiling(t *testing.T) {
	tests := []struct {
		name     string
		envVal   string
		baseURL  string
		expected int
	}{
		{
			name:     "Localhost port 11434 default",
			envVal:   "",
			baseURL:  "http://localhost:11434/v1",
			expected: DefaultLocalContextTokens,
		},
		{
			name:     "IPv4 loopback default",
			envVal:   "",
			baseURL:  "http://127.0.0.1:8080/v1",
			expected: DefaultLocalContextTokens,
		},
		{
			name:     "IPv6 loopback default",
			envVal:   "",
			baseURL:  "http://[::1]:8080/v1",
			expected: DefaultLocalContextTokens,
		},
		{
			name:     "Remote host on port 11434",
			envVal:   "",
			baseURL:  "http://remote-server:11434/v1",
			expected: DefaultLocalContextTokens,
		},
		{
			name:     "Remote cloud endpoint default",
			envVal:   "",
			baseURL:  "https://api.openai.com/v1",
			expected: DefaultRemoteContextTokens,
		},
		{
			name:     "Explicit env override takes precedence over local",
			envVal:   "32768",
			baseURL:  "http://localhost:11434/v1",
			expected: 32768,
		},
		{
			name:     "Explicit env override takes precedence over cloud",
			envVal:   "128000",
			baseURL:  "https://api.openai.com/v1",
			expected: 128000,
		},
		{
			name:     "Invalid env override falls back to topology",
			envVal:   "invalid",
			baseURL:  "http://localhost:11434/v1",
			expected: DefaultLocalContextTokens,
		},
		{
			name:     "Negative env override falls back to topology",
			envVal:   "-10",
			baseURL:  "https://api.openai.com/v1",
			expected: DefaultRemoteContextTokens,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := resolveContextCeiling(tc.envVal, tc.baseURL)
			if actual != tc.expected {
				t.Errorf("resolveContextCeiling(%q, %q) = %d, expected %d", tc.envVal, tc.baseURL, actual, tc.expected)
			}
		})
	}
}

func TestIsLocalEndpoint(t *testing.T) {
	tests := []struct {
		url      string
		expected bool
	}{
		{"http://localhost:11434/v1", true},
		{"http://127.0.0.1:11434/v1", true},
		{"http://127.0.0.1:8000/v1", true},
		{"http://[::1]:11434/v1", true},
		{"http://[::1]:9999/v1", true},
		{"http://my-host.internal:11434/v1", true},
		{"https://api.openai.com/v1", false},
		{"https://api.anthropic.com/v1", false},
		{"http://192.168.1.100:8000/v1", false},
		{"", false},
	}

	for _, tc := range tests {
		actual := IsLocalEndpoint(tc.url)
		if actual != tc.expected {
			t.Errorf("IsLocalEndpoint(%q) = %v, expected %v", tc.url, actual, tc.expected)
		}
	}
}
