package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hoangndst/buddy-agent/internal/memory"
)

// SaveMemoryTool appends knowledge to durable memory.
type SaveMemoryTool struct {
	mgr *memory.Manager
}

// NewSaveMemoryTool creates a new SaveMemoryTool bound to memory manager.
func NewSaveMemoryTool(mgr *memory.Manager) *SaveMemoryTool {
	return &SaveMemoryTool{mgr: mgr}
}

// Name returns the identifier of the tool.
func (s *SaveMemoryTool) Name() string {
	return "save_memory"
}

// Description returns a human-readable explanation of the tool.
func (s *SaveMemoryTool) Description() string {
	return "Save an important finding, user preference, or project fact to long-term memory (MEMORY.md)."
}

// Parameters returns the JSON schema definition for tool arguments.
func (s *SaveMemoryTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"fact": map[string]any{
				"type":        "string",
				"description": "The fact, rule, or preference to persist.",
			},
		},
		"required": []string{"fact"},
	}
}

type saveMemoryInput struct {
	Fact string `json:"fact"`
}

// Execute appends the fact to long-term memory.
func (s *SaveMemoryTool) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var input saveMemoryInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if input.Fact == "" {
		return "", errors.New("fact cannot be empty")
	}

	if err := s.mgr.AppendMemory(input.Fact); err != nil {
		return "", fmt.Errorf("failed to save memory: %w", err)
	}

	return "Fact successfully persisted to long-term memory.", nil
}

// CreateSkillTool writes a reusable skill to the skills/ directory.
type CreateSkillTool struct {
	mgr *memory.Manager
}

// NewCreateSkillTool creates a new CreateSkillTool bound to memory manager.
func NewCreateSkillTool(mgr *memory.Manager) *CreateSkillTool {
	return &CreateSkillTool{mgr: mgr}
}

// Name returns the identifier of the tool.
func (c *CreateSkillTool) Name() string {
	return "create_skill"
}

// Description returns a human-readable explanation of the tool.
func (c *CreateSkillTool) Description() string {
	return "Save a discovered workflow, troubleshooting procedure, or reusable guide to skills/<name>.md."
}

// Parameters returns the JSON schema definition for tool arguments.
func (c *CreateSkillTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Short identifier for the skill (e.g. docker-debug, git-rebase-flow).",
			},
			"instructions": map[string]any{
				"type":        "string",
				"description": "Clear step-by-step instructions and notes for performing the task.",
			},
		},
		"required": []string{"name", "instructions"},
	}
}

type createSkillInput struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
}

// Execute persists the skill procedure to disk.
func (c *CreateSkillTool) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var input createSkillInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if input.Name == "" || input.Instructions == "" {
		return "", errors.New("both name and instructions are required")
	}

	if err := c.mgr.SaveSkill(input.Name, input.Instructions); err != nil {
		return "", fmt.Errorf("failed to create skill: %w", err)
	}

	return fmt.Sprintf("Skill %q successfully created in skills/ directory.", input.Name), nil
}
