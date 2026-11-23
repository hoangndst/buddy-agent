package cmd

import (
	"sort"
	"strings"
)

type SlashCommand struct {
	Name        string
	Description string
	ArgsHint    string
}

type SlashCommandRegistry struct {
	commands []SlashCommand
}

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

func (r *SlashCommandRegistry) Match(prefix string) []SlashCommand {
	var matches []SlashCommand
	for _, command := range r.commands {
		if strings.HasPrefix(command.Name, prefix) {
			matches = append(matches, command)
		}
	}
	return matches
}
