package tui

import (
	"strings"
	"testing"
)

func TestRenderChoiceListAndCompletionWindow(t *testing.T) {
	choices := RenderChoiceList(ChoiceList{Label: "Choose", Items: []string{"alpha", "beta", "gamma"}, Cursor: 1, Height: 3})
	if len(choices) != 3 || !strings.Contains(choices[2], "› beta") {
		t.Fatalf("choice list = %#v", choices)
	}
	completions := RenderCompletion(CompletionView{
		Kind: "command", Commands: []SlashCommand{{Name: "/help", Description: "Show help"}, {Name: "/quit"}},
		Cursor: 1, Start: 1, Rows: 1,
	})
	if len(completions) != 1 || !strings.Contains(completions[0], "› /quit") {
		t.Fatalf("completion window = %#v", completions)
	}
}
