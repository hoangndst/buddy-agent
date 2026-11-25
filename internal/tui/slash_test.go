package tui

import "testing"

func TestSlashCommandRegistryMatchAndReplace(t *testing.T) {
	var registry SlashCommandRegistry
	registry.Register(SlashCommand{Name: "/clear"})
	registry.Register(SlashCommand{Name: "/copy"})
	registry.Register(SlashCommand{Name: "/clear", Description: "updated"})
	got := registry.Match("/cl")
	if len(got) != 1 || got[0].Description != "updated" {
		t.Fatalf("unexpected prefix matches: %+v", got)
	}
}
