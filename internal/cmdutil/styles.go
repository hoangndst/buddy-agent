package cmdutil

import (
	"encoding/json"
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

var (
	// ThoughtStyle renders reasoning and thought blocks.
	ThoughtStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("246")).Italic(true)
	// ToolStyle renders tool invocation action previews.
	ToolStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	// ResultStyle renders tool output summaries.
	ResultStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	// DoneStyle renders task completion headers.
	DoneStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	// ErrStyle renders error banners.
	ErrStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
)

// FormatToolPreview produces clean, human-readable tool invocation previews.
func FormatToolPreview(name, rawArgs string) string {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(rawArgs), &args); err == nil {
		switch name {
		case "bash":
			if cmd, ok := args["command"].(string); ok && cmd != "" {
				return fmt.Sprintf("⚙ bash: %s", cmd)
			}
		case "read_file":
			if p, ok := args["path"].(string); ok && p != "" {
				return fmt.Sprintf("⚙ read_file: %s", p)
			}
		case "write_file":
			if p, ok := args["path"].(string); ok && p != "" {
				return fmt.Sprintf("⚙ write_file: %s", p)
			}
		case "edit_file":
			if p, ok := args["path"].(string); ok && p != "" {
				return fmt.Sprintf("⚙ edit_file: %s", p)
			}
		case "save_memory":
			if topic, ok := args["topic"].(string); ok && topic != "" {
				return fmt.Sprintf("⚙ save_memory: %s", topic)
			}
		case "create_skill":
			if n, ok := args["name"].(string); ok && n != "" {
				return fmt.Sprintf("⚙ create_skill: %s", n)
			}
		default:
			for _, key := range []string{"path", "query", "command", "url", "name", "id", "prompt"} {
				if val, ok := args[key].(string); ok && val != "" {
					return fmt.Sprintf("⚙ %s: %s", name, val)
				}
			}
		}
	}
	return fmt.Sprintf("⚙ %s(%s)", name, rawArgs)
}
