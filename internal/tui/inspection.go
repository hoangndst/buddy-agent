package tui

import (
	"fmt"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type inspectionState struct {
	Kind                         InspectionKind
	Generation                   uint64
	Ref                          string
	Output                       *OutputPage
	Diff                         *DiffView
	Loading                      bool
	Error                        string
	SelectedFile, Scroll, Column int
	FileOpen                     bool
}

var inspectionErrorStyle = lipgloss.NewStyle().Foreground(ThemeColor("#B42318", "#F07B72"))

func (m *Model) updateInspectionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	i := &m.inspection
	switch msg.String() {
	case "esc":
		if i.Kind == InspectionDiff && i.FileOpen {
			i.FileOpen, i.Scroll, i.Column = false, 0, 0
			return m, nil
		}
		generation := i.Generation
		m.closeInspection()
		return m, EmitAction(Action{Kind: ActionCloseInspection, InspectionGeneration: generation})
	case "up", "k":
		if i.Kind == InspectionOutput {
			i.Scroll = max(0, i.Scroll-1)
		} else if i.FileOpen {
			i.Scroll = max(0, i.Scroll-1)
		} else if i.SelectedFile > 0 {
			i.SelectedFile--
		}
	case "down", "j":
		if i.Kind == InspectionOutput {
			if i.Output != nil {
				i.Scroll = min(max(0, strings.Count(i.Output.Data, "\n")), i.Scroll+1)
			}
		} else if i.FileOpen {
			if f := i.selectedDiff(); f != nil {
				i.Scroll = min(max(0, strings.Count(f.Diff, "\n")), i.Scroll+1)
			}
		} else if i.Diff != nil && i.SelectedFile+1 < len(i.Diff.Files) {
			i.SelectedFile++
		}
	case "enter":
		if i.Kind == InspectionDiff && !i.FileOpen && i.selectedDiff() != nil {
			i.FileOpen, i.Scroll, i.Column = true, 0, 0
		}
	case "left", "right":
		if i.Kind == InspectionOutput && i.Output != nil {
			i.pan(i.Output.Data, m.Width, msg.String() == "right")
		}
		if i.Kind == InspectionDiff && i.FileOpen {
			if f := i.selectedDiff(); f != nil {
				i.pan(f.Diff, m.Width, msg.String() == "right")
			}
		}
	case "pgup":
		if i.Kind == InspectionOutput {
			return m, EmitAction(Action{Kind: ActionChangeOutputPage, InspectionGeneration: i.Generation, PageDelta: -1})
		}
		if i.Kind == InspectionDiff && i.FileOpen {
			i.moveHunk(-1, m.Height)
		}
	case "pgdown":
		if i.Kind == InspectionOutput {
			return m, EmitAction(Action{Kind: ActionChangeOutputPage, InspectionGeneration: i.Generation, PageDelta: 1})
		}
		if i.Kind == InspectionDiff && i.FileOpen {
			i.moveHunk(1, m.Height)
		}
	}
	return m, nil
}

func (m *Model) updateInspectionWheel(msg tea.MouseWheelMsg) tea.Cmd {
	i := &m.inspection
	if i.Kind == InspectionDiff && !i.FileOpen {
		step := 3
		if msg.Button == tea.MouseWheelUp {
			i.SelectedFile = max(0, i.SelectedFile-step)
		} else if msg.Button == tea.MouseWheelDown && i.Diff != nil {
			i.SelectedFile = min(max(0, len(i.Diff.Files)-1), i.SelectedFile+step)
		}
		return nil
	}
	if i.Kind == InspectionOutput || i.FileOpen {
		lines := 0
		if i.Kind == InspectionOutput && i.Output != nil {
			lines = strings.Count(i.Output.Data, "\n") + 1
		}
		if i.Kind == InspectionDiff {
			if f := i.selectedDiff(); f != nil {
				lines = strings.Count(f.Diff, "\n") + 1
			}
		}
		switch msg.Button {
		case tea.MouseWheelUp:
			i.Scroll = max(0, i.Scroll-3)
		case tea.MouseWheelDown:
			i.Scroll = min(max(0, lines-1), i.Scroll+3)
		}
	}
	return nil
}

func (i *inspectionState) selectedDiff() *DiffFile {
	if i.Diff == nil || i.SelectedFile < 0 || i.SelectedFile >= len(i.Diff.Files) {
		return nil
	}
	return &i.Diff.Files[i.SelectedFile]
}

func (i *inspectionState) view(width, height int) []string {
	if i.Kind == InspectionNone || height <= 0 {
		return nil
	}
	width = max(1, width)
	var header, notices, content []string
	hint := "↑/↓ scroll · ←/→ pan · PageUp/PageDown pages · Esc returns to activity"
	if i.Kind == InspectionOutput {
		header = append(header, inspectionTitle("Tool output")+"  ·  "+inspectionMuted(i.Ref))
		if i.Loading {
			header = append(header, "Loading output page…")
		} else if i.Output != nil {
			p := i.Output
			end := p.Offset + int64(len(p.Data))
			byteRange := "No output bytes"
			if end > p.Offset {
				byteRange = fmt.Sprintf("Bytes %d–%d of %d", p.Offset+1, end, p.TotalBytes)
			}
			header = append(header, inspectionMuted(byteRange), "")
			content = strings.Split(p.Data, "\n")
			if p.Truncated {
				notices = append(notices, inspectionErrorStyle.Render("Output capped at 1 MiB; rerun the command with narrower output."))
			}
			if !p.HasMore {
				notices = append(notices, inspectionMuted("End of available output"))
			}
		}
	} else {
		header = append(header, inspectionTitle("Working tree diff")+"  ·  "+inspectionMuted("/diff"))
		files := []DiffFile(nil)
		if i.Diff != nil && !i.Loading {
			files = i.Diff.Files
		}
		if i.Loading {
			header = append(header, "Inspecting Git changes…")
		} else if len(files) == 0 {
			header = append(header, "No changed files found.")
		} else if !i.FileOpen {
			stats := DiffStats{}
			if i.Diff != nil {
				stats = i.Diff.Stats
			}
			header = append(header, fmt.Sprintf("%d files  ·  +%d  −%d", stats.FilesChanged, stats.Additions, stats.Deletions), "")
		} else if f := i.selectedDiff(); f != nil {
			header = append(header, inspectionText(f.Path)+"  ·  "+diffAttribution(*f))
			if notice := diffNotice(*f); notice != "" {
				header = append(header, inspectionMuted(notice))
			}
		}
		if i.Diff != nil && !i.Loading {
			if i.Diff.Warning != "" {
				notices = append(notices, inspectionErrorStyle.Render(inspectionText(i.Diff.Warning)))
			}
			if i.Diff.OmittedPaths > 0 {
				notices = append(notices, inspectionMuted(fmt.Sprintf("%d paths omitted because they could not be represented or read safely", i.Diff.OmittedPaths)))
			}
			if i.Diff.Truncated {
				notices = append(notices, inspectionErrorStyle.Render("Diff output was capped to fit the safe response limit."))
			}
		}
		hint = "↑/↓ files · Enter open · Esc close"
		if i.Loading {
			hint = "Esc close"
		} else if i.FileOpen {
			hint = "↑/↓ scroll · ←/→ pan · PageUp/PageDown hunks · Esc file list"
		}
		if i.FileOpen {
			if f := i.selectedDiff(); f != nil {
				content = strings.Split(f.Diff, "\n")
			}
		} else {
			for n, f := range files {
				marker := "  "
				if n == i.SelectedFile {
					marker = "› "
				}
				content = append(content, fmt.Sprintf("%s%s %-18s %s", marker, inspectionText(strings.ReplaceAll(f.Path, "\n", " ")), diffAttribution(f), diffNotice(f)))
			}
		}
	}
	if i.Error != "" {
		notices = append(notices, inspectionErrorStyle.Render(inspectionText(i.Error)))
	}
	controls := WrapLines(inspectionMuted(hint), width)
	headerRows, noticeRows := WrapLines(strings.Join(header, "\n"), width), WrapLines(strings.Join(notices, "\n"), width)
	controls = controls[:min(len(controls), height)]
	headerRows = headerRows[:min(len(headerRows), max(0, height-len(controls)))]
	noticeRows = noticeRows[:min(len(noticeRows), max(0, height-len(controls)-len(headerRows)))]
	rows := max(0, height-len(headerRows)-len(noticeRows)-len(controls))
	if i.Kind == InspectionDiff && !i.FileOpen && len(content) > rows {
		start := max(0, min(i.SelectedFile-rows+1, len(content)-rows))
		content = content[start : start+rows]
	} else if i.Kind == InspectionOutput || i.FileOpen {
		i.Scroll = min(i.Scroll, max(0, len(content)-rows))
	}
	result := append(append(append([]string{}, headerRows...), noticeRows...), nil...)
	start := min(i.Scroll, max(0, len(content)-1))
	for _, line := range content[start:min(len(content), start+rows)] {
		result = append(result, ansi.Cut(inspectionText(line), i.Column, i.Column+width))
	}
	result = append(result, controls...)
	return result
}

func inspectionText(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ReplaceAll(s, "\t", "    "))
}
func inspectionTitle(s string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(ThemeColor("#A94721", "#EFA276")).Render(s)
}
func inspectionMuted(s string) string {
	return lipgloss.NewStyle().Foreground(ThemeColor("#62656D", "#9297A3")).Render(s)
}
func diffAttribution(f DiffFile) string {
	if f.AttributionUnknown {
		return "Unknown"
	}
	if f.IsBuddyEdit && f.WasDirtyAtStart {
		return "Buddy + pre-existing"
	}
	if f.IsBuddyEdit {
		return "Buddy edit"
	}
	if f.WasDirtyAtStart {
		return "Pre-existing"
	}
	return "Unknown"
}
func diffNotice(f DiffFile) string {
	var values []string
	if f.IsBinary {
		values = append(values, "binary omitted")
	}
	if f.Truncated {
		values = append(values, "truncated")
	}
	return strings.Join(values, ", ")
}

func (i *inspectionState) pan(content string, width int, right bool) {
	step := max(1, width/2)
	if !right {
		i.Column = max(0, i.Column-step)
		return
	}
	longest := 0
	for line := range strings.SplitSeq(content, "\n") {
		longest = max(longest, ansi.StringWidth(inspectionText(line)))
	}
	i.Column = min(max(0, longest-max(1, width)), i.Column+step)
}
func (i *inspectionState) moveHunk(direction, height int) {
	f := i.selectedDiff()
	if f == nil {
		return
	}
	lines := strings.Split(f.Diff, "\n")
	if direction > 0 {
		for n := i.Scroll + 1; n < len(lines); n++ {
			if strings.HasPrefix(lines[n], "@@") {
				i.Scroll = n
				return
			}
		}
		i.Scroll = min(len(lines)-1, i.Scroll+max(1, height/2))
		return
	}
	for n := i.Scroll - 1; n >= 0; n-- {
		if strings.HasPrefix(lines[n], "@@") {
			i.Scroll = n
			return
		}
	}
	i.Scroll = max(0, i.Scroll-max(1, height/2))
}

func (m *Model) closeInspection() { m.inspection = inspectionState{} }
