package tui

import (
	"sort"
	"strings"
)

// SlashCommand describes a composer command and its help text.
type SlashCommand struct {
	Name        string
	Description string
	ArgsHint    string
}

// SlashCommandRegistry stores commands in name order.
type SlashCommandRegistry struct {
	commands []SlashCommand
}

// Register adds a command or replaces the command with the same name.
func (r *SlashCommandRegistry) Register(command SlashCommand) {
	for i := range r.commands {
		if r.commands[i].Name == command.Name {
			r.commands[i] = command
			return
		}
	}
	r.commands = append(r.commands, command)
	sort.Slice(r.commands, func(i, j int) bool { return r.commands[i].Name < r.commands[j].Name })
}

// Match returns commands whose names begin with prefix.
func (r *SlashCommandRegistry) Match(prefix string) []SlashCommand {
	var matches []SlashCommand
	for _, command := range r.commands {
		if strings.HasPrefix(command.Name, prefix) {
			matches = append(matches, command)
		}
	}
	return matches
}

// Commands returns registered commands in name order.
func (r *SlashCommandRegistry) Commands() []SlashCommand {
	return append([]SlashCommand(nil), r.commands...)
}
