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
	if cfg.MaxSteps != 30 {
		t.Errorf("expected default MaxSteps 30, got %d", cfg.MaxSteps)
	}
	if cfg.MaxOutputChars != 10000 {
		t.Errorf("expected default MaxOutputChars 10000, got %d", cfg.MaxOutputChars)
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
	if !cfg.Debug {
		t.Errorf("expected Debug true, got false")
	}
}
