package tui

// ChoiceList is a bounded picker view over UI-owned labels.
type ChoiceList struct {
	Label  string
	Items  []string
	Cursor int
	Height int
}

// RenderChoiceList renders a scrollable picker list.
func RenderChoiceList(list ChoiceList) []string {
	lines := []string{titleStyle.Render(list.Label)}
	if list.Height <= 1 {
		return lines
	}
	start := max(0, min(list.Cursor-list.Height+2, len(list.Items)-list.Height+1))
	for i := start; i < min(len(list.Items), start+list.Height-1); i++ {
		marker := "  "
		if i == list.Cursor {
			marker = "› "
		}
		lines = append(lines, marker+cleanText(list.Items[i]))
	}
	return lines
}

// CompletionView contains the already-filtered items and current window.
type CompletionView struct {
	Kind     string
	Commands []SlashCommand
	Files    []string
	Cursor   int
	Start    int
	Rows     int
}

// RenderCompletion draws the visible completion window.
func RenderCompletion(view CompletionView) []string {
	var rows []string
	if view.Kind == "command" {
		for i := view.Start; i < min(view.Start+view.Rows, len(view.Commands)); i++ {
			marker := "  "
			if i == view.Cursor {
				marker = "› "
			}
			command := view.Commands[i]
			rows = append(rows, marker+cleanText(command.Name)+"  "+mutedStyle.Render(cleanText(command.Description+" "+command.ArgsHint)))
		}
		return rows
	}
	for i := view.Start; i < min(view.Start+view.Rows, len(view.Files)); i++ {
		marker := "  "
		if i == view.Cursor {
			marker = "› "
		}
		rows = append(rows, marker+cleanText(view.Files[i]))
	}
	return rows
}
