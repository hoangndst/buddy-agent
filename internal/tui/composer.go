package tui

import (
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Composer owns prompt editing, history navigation, and inline completion.
type Composer struct {
	Editor   textarea.Model
	History  *InputHistoryRing
	Commands SlashCommandRegistry

	completionKind   string
	commandMatches   []SlashCommand
	fileMatches      []string
	completionCursor int
	completionStart  int
	completionEnd    int
}

// NewComposer creates a focused composer with the supplied history store.
func NewComposer(history *InputHistoryRing) *Composer {
	input := textarea.New()
	input.ShowLineNumbers = false
	input.Placeholder = "Ask Buddy anything…"
	input.SetHeight(1)
	input.SetStyles(textarea.DefaultStyles(true))
	input.Prompt = "› "
	input.SetVirtualCursor(true)
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter"), key.WithHelp("shift+enter", "new line"))
	input.Focus()
	c := &Composer{Editor: input, History: history}
	c.Resize(80, 24)
	return c
}

// Resize updates the editor width and bounds its height to the terminal.
func (c *Composer) Resize(width, terminalHeight int) {
	c.Editor.SetWidth(max(1, width-4))
	maxRows := 5
	if width < 80 || terminalHeight < 24 {
		maxRows = 3
	}
	if width < 40 || terminalHeight < 12 {
		maxRows = 1
	}
	rows := 1
	if strings.TrimSpace(c.Editor.Value()) != "" {
		editorWidth := max(1, c.Editor.Width())
		rows = 0
		for _, line := range strings.Split(c.Editor.Value(), "\n") {
			rows += max(1, (ansi.StringWidth(line)+editorWidth-1)/editorWidth)
		}
		rows = min(maxRows, max(1, rows))
	}
	c.Editor.SetHeight(rows)
}

// SetValue replaces the draft, moves the cursor to its end, and resizes.
func (c *Composer) SetValue(value string, width, terminalHeight int) {
	c.Editor.SetValue(value)
	c.Editor.CursorEnd()
	c.Resize(width, terminalHeight)
}

// Reset clears the draft and returns the editor to its compact height.
func (c *Composer) Reset(width, terminalHeight int) {
	c.Editor.Reset()
	c.Resize(width, terminalHeight)
}

// Update processes an editor message and refreshes completion after edits.
func (c *Composer) Update(msg tea.Msg, width, terminalHeight int, workspace string) tea.Cmd {
	beforeValue, beforeLine, beforeLineInfo := c.Editor.Value(), c.Editor.Line(), c.Editor.LineInfo()
	var cmd tea.Cmd
	c.Editor, cmd = c.Editor.Update(msg)
	c.Resize(width, terminalHeight)
	if beforeValue != c.Editor.Value() || beforeLine != c.Editor.Line() || beforeLineInfo != c.Editor.LineInfo() {
		c.UpdateCompletion(workspace)
	}
	return cmd
}

// UpdateCompletion matches the current word against commands or workspace files.
func (c *Composer) UpdateCompletion(workspace string) {
	text := c.Editor.Value()
	line := c.Editor.Line()
	lines := strings.Split(text, "\n")
	if line >= len(lines) {
		c.completionKind = ""
		return
	}
	current := lines[line]
	runes := []rune(current)
	lineInfo := c.Editor.LineInfo()
	cursor := min(len(runes), lineInfo.StartColumn+lineInfo.ColumnOffset)
	start, end := cursor, cursor
	for start > 0 && runes[start-1] != ' ' && runes[start-1] != '\t' {
		start--
	}
	for end < len(runes) && runes[end] != ' ' && runes[end] != '\t' {
		end++
	}
	word := string(runes[start:cursor])
	lineStart := 0
	for i := 0; i < line; i++ {
		lineStart += len(lines[i]) + 1
	}
	c.completionStart = lineStart + len(string(runes[:start]))
	c.completionEnd = lineStart + len(string(runes[:end]))
	c.completionCursor = 0
	if strings.HasPrefix(word, "/") {
		c.commandMatches = c.Commands.Match(word)
		c.fileMatches = nil
		c.completionKind = "command"
		if len(c.commandMatches) == 0 {
			c.completionKind = ""
		}
		return
	}
	if strings.HasPrefix(word, "@") {
		c.fileMatches = nil
		entries, err := os.ReadDir(workspace)
		if err != nil {
			c.completionKind = ""
			return
		}
		prefix := strings.TrimPrefix(word, "@")
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), prefix) && len(c.fileMatches) < 50 {
				c.fileMatches = append(c.fileMatches, entry.Name())
			}
		}
		c.commandMatches = nil
		c.completionKind = "file"
		if len(c.fileMatches) == 0 {
			c.completionKind = ""
		}
		return
	}
	c.completionKind = ""
}

// ClearCompletion dismisses suggestions and resets their selection state.
func (c *Composer) ClearCompletion() {
	c.completionKind = ""
	c.commandMatches = nil
	c.fileMatches = nil
	c.completionCursor = 0
	c.completionStart = 0
	c.completionEnd = 0
}

// CompletionKind reports whether commands or files are currently suggested.
func (c *Composer) CompletionKind() string { return c.completionKind }

// CompletionCommands returns the current slash-command matches.
func (c *Composer) CompletionCommands() []SlashCommand { return c.commandMatches }

// CompletionFiles returns the current workspace file matches.
func (c *Composer) CompletionFiles() []string { return c.fileMatches }

// CompletionCursor reports the selected suggestion index.
func (c *Composer) CompletionCursor() int { return c.completionCursor }

// CompletionCount reports the number of current suggestions.
func (c *Composer) CompletionCount() int {
	if c.completionKind == "file" {
		return len(c.fileMatches)
	}
	return len(c.commandMatches)
}

// MoveCompletion advances or reverses the selected suggestion.
func (c *Composer) MoveCompletion(step int) {
	items := c.CompletionCount()
	if items > 0 {
		c.completionCursor = (c.completionCursor + step + items) % items
	}
}

// AcceptCompletion inserts the selected match and reports whether the key was consumed.
func (c *Composer) AcceptCompletion(enter bool) bool {
	if c.CompletionCount() == 0 {
		return false
	}
	if c.completionKind == "file" {
		c.replaceCompletion(c.fileMatches[c.completionCursor])
		c.ClearCompletion()
		return true
	}
	command := c.commandMatches[c.completionCursor]
	suffix := ""
	if command.ArgsHint != "" {
		suffix = " "
	}
	c.replaceCompletion(command.Name + suffix)
	c.ClearCompletion()
	return !enter || suffix != ""
}

// Previous navigates to an earlier prompt while retaining the current draft.
func (c *Composer) Previous(draft string) (string, bool) {
	if c.History == nil {
		return "", false
	}
	return c.History.Previous(draft)
}

// HasHistory reports whether prompt history is available.
func (c *Composer) HasHistory() bool { return c.History != nil }

// Next navigates forward through prompt history and restores the saved draft.
func (c *Composer) Next() (string, bool) {
	if c.History == nil {
		return "", false
	}
	return c.History.Next()
}

// AddHistory persists a submitted prompt and resets history navigation.
func (c *Composer) AddHistory(prompt string) error {
	if c.History == nil {
		return nil
	}
	if err := c.History.Add(prompt); err != nil {
		return err
	}
	c.History.Reset()
	return nil
}

// CompletionView renders the selected window of current suggestions.
func (c *Composer) CompletionView(rows int) []string {
	count := c.CompletionCount()
	start := min(max(0, c.completionCursor-rows+1), max(0, count-rows))
	return RenderCompletion(CompletionView{
		Kind: c.completionKind, Commands: c.commandMatches, Files: c.fileMatches,
		Cursor: c.completionCursor, Start: start, Rows: rows,
	})
}

func (c *Composer) replaceCompletion(value string) {
	text := c.Editor.Value()
	if c.completionStart <= c.completionEnd && c.completionEnd <= len(text) {
		c.Editor.SetValue(text[:c.completionStart] + value + text[c.completionEnd:])
		c.Editor.CursorEnd()
	}
}
