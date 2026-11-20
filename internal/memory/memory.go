// Package memory handles file-backed persona, durable notes, and dynamic procedural skills.
package memory

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed prompts/default_soul.md
var defaultSoulPrompt string

// Manager coordinates file-backed identity, durable memory, and dynamic skills.
type Manager struct {
	workspace string
}

// NewManager creates a memory manager anchored at workspace.
func NewManager(workspace string) *Manager {
	return &Manager{workspace: workspace}
}

// LoadSystemPrompt combines SOUL.md, USER.md, MEMORY.md, and skills/*.md.
func (m *Manager) LoadSystemPrompt() (string, error) {
	var sb strings.Builder

	// 1. Core Identity & Directives (SOUL.md)
	soulPath := filepath.Join(m.workspace, "SOUL.md")
	soulContent, err := os.ReadFile(soulPath)
	if err != nil {
		if os.IsNotExist(err) {
			sb.WriteString(strings.TrimSpace(defaultSoulPrompt))
			sb.WriteString("\n\n")
		} else {
			return "", fmt.Errorf("failed to read SOUL.md: %w", err)
		}
	} else {
		sb.Write(soulContent)
		sb.WriteString("\n\n")
	}

	// 2. User Context (USER.md)
	userPath := filepath.Join(m.workspace, "USER.md")
	if userContent, err := os.ReadFile(userPath); err == nil && len(userContent) > 0 {
		sb.WriteString("## User Context & Environment\n")
		sb.Write(userContent)
		sb.WriteString("\n\n")
	}

	// 3. Durable Memory / Scratchpad (MEMORY.md)
	memoryPath := filepath.Join(m.workspace, "MEMORY.md")
	if memoryContent, err := os.ReadFile(memoryPath); err == nil && len(memoryContent) > 0 {
		sb.WriteString("## Persistent Memory & Project Facts\n")
		sb.Write(memoryContent)
		sb.WriteString("\n\n")
	}

	// 4. Procedural Skills (skills/*.md)
	skills, err := m.LoadSkills()
	if err == nil && len(skills) > 0 {
		sb.WriteString("## Available Procedural Skills\n")
		for _, s := range skills {
			sb.WriteString(fmt.Sprintf("### Skill: %s\n%s\n\n", s.Name, s.Content))
		}
	}

	return strings.TrimSpace(sb.String()), nil
}

// Skill represents a learned procedural capability.
type Skill struct {
	Name    string
	Content string
}

// LoadSkills scans the skills/ directory and loads all markdown skill files.
func (m *Manager) LoadSkills() ([]Skill, error) {
	skillsDir := filepath.Join(m.workspace, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var skills []Skill
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			path := filepath.Join(skillsDir, entry.Name())
			content, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			skills = append(skills, Skill{
				Name:    strings.TrimSuffix(entry.Name(), ".md"),
				Content: strings.TrimSpace(string(content)),
			})
		}
	}
	return skills, nil
}

// AppendMemory appends a durable note or finding to MEMORY.md.
func (m *Manager) AppendMemory(entry string) (err error) {
	path := filepath.Join(m.workspace, "MEMORY.md")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open MEMORY.md: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("failed to close MEMORY.md: %w", closeErr)
		}
	}()

	trimmed := strings.TrimSpace(entry)
	if _, err := fmt.Fprintf(f, "\n- %s\n", trimmed); err != nil {
		return fmt.Errorf("failed to write to MEMORY.md: %w", err)
	}
	return nil
}

// SaveSkill saves a procedural skill to skills/<name>.md.
func (m *Manager) SaveSkill(name, content string) error {
	trimmedName := strings.TrimSpace(name)
	if trimmedName == "" {
		return errors.New("skill name cannot be empty")
	}

	// Reject path separators and traversal attempts
	if strings.ContainsAny(trimmedName, `/\`) || strings.Contains(trimmedName, "..") {
		return fmt.Errorf("invalid skill name %q: must not contain path separators or traversal sequences", name)
	}

	absWorkspace, err := filepath.Abs(m.workspace)
	if err != nil {
		return fmt.Errorf("failed to resolve workspace directory: %w", err)
	}
	skillsDir := filepath.Clean(filepath.Join(absWorkspace, "skills"))
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		return fmt.Errorf("failed to create skills directory: %w", err)
	}

	safeName := strings.ToLower(strings.ReplaceAll(trimmedName, " ", "-"))
	if !strings.HasSuffix(safeName, ".md") {
		safeName += ".md"
	}

	targetPath := filepath.Clean(filepath.Join(skillsDir, safeName))
	rel, err := filepath.Rel(skillsDir, targetPath)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." || rel != safeName {
		return fmt.Errorf("access denied: skill name %q escapes skills directory", name)
	}

	if err := os.WriteFile(targetPath, []byte(strings.TrimSpace(content)+"\n"), 0644); err != nil {
		return fmt.Errorf("failed to write skill file: %w", err)
	}
	return nil
}
