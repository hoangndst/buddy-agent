package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Welcome contains the display metadata shown before a conversation has content.
type Welcome struct {
	Width, Height             int
	Model, Workspace, Version string
	GitBranch                 string
	Status                    string
}

// RenderWelcome lays out Buddy's mascot and first-use guidance when there is room.
func RenderWelcome(w Welcome) []string {
	width := max(1, w.Width)
	workspace := cleanText(strings.TrimSpace(w.Workspace))
	if workspace == "" {
		workspace = "workspace"
	}
	model := cleanText(strings.TrimSpace(w.Model))
	version := cleanText(strings.TrimSpace(w.Version))
	if version == "" {
		version = "dev"
	}
	status := cleanText(strings.TrimSpace(w.Status))
	if status == "" {
		status = "Connected"
	}
	if width < 70 || w.Height < 16 {
		return compactWelcome(width, model, workspace, w.GitBranch, version, status)
	}

	art := RenderMascot(MascotWelcome)
	leftWidth := min(52, max(34, width/3))
	rightWidth := width - leftWidth - 7
	if rightWidth < 24 {
		return compactWelcome(width, model, workspace, w.GitBranch, version, status)
	}

	left := append([]string{}, art...)
	left = append(left, titleStyle.Render("Welcome back!"))
	if model != "" {
		left = append(left, mutedStyle.Render(ansi.Truncate(model, leftWidth, "…")))
	}
	left = append(left,
		mutedStyle.Render(ansi.Truncate(workspace, leftWidth, "…")),
	)
	if branch := cleanText(strings.TrimSpace(w.GitBranch)); branch != "" {
		left = append(left, mutedStyle.Render("on "+ansi.Truncate(branch, leftWidth-3, "…")))
	}
	left = append(left, mutedStyle.Render("Version "+version), runtimeStatus(status))

	right := []string{
		accentStyle().Render("Tips for getting started"),
		"Ask Buddy to explain this project or help with a coding task.",
		accentRule(rightWidth),
		accentStyle().Render("What Buddy can do"),
		"Explore files, edit code, run tests, and review your changes.",
		"",
		accentStyle().Render("Useful commands"),
		"/help       Show available commands",
		"/resume     Continue a previous conversation",
		"/diff       Review working-tree changes",
	}
	var wrappedRight []string
	for _, line := range right {
		wrappedRight = append(wrappedRight, WrapLines(line, rightWidth)...)
	}
	right = wrappedRight

	rows := max(len(left), len(right))
	left = padPane(left, leftWidth, rows)
	right = padPane(right, rightWidth, rows)
	return welcomeFrame(welcomeTitle(version), left, right, rightWidth, width)
}

func compactWelcome(width int, model, workspace, branch, version, status string) []string {
	lines := []string{titleStyle.Render("Buddy Agent"), mutedStyle.Render("Your terminal coding companion")}
	if model != "" {
		lines = append(lines, mutedStyle.Render(ansi.Truncate(model, width, "…")))
	}
	lines = append(lines, mutedStyle.Render(ansi.Truncate(workspace, width, "…")))
	if branch = cleanText(strings.TrimSpace(branch)); branch != "" {
		lines = append(lines, mutedStyle.Render("on "+ansi.Truncate(branch, max(1, width-3), "…")))
	}
	lines = append(lines, mutedStyle.Render("Version "+version), runtimeStatus(status))
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	return lines
}

func welcomeTitle(version string) string {
	return titleStyle.Render("Buddy Agent") + " " + mutedStyle.Render("v"+version)
}

func runtimeStatus(status string) string {
	if status == "Needs attention" {
		return lipgloss.NewStyle().Foreground(ThemeColor("#B42318", "#F28B82")).Render("● " + status)
	}
	return lipgloss.NewStyle().Foreground(ThemeColor("#387E61", "#78B99A")).Render("● " + status)
}

func accentStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(ThemeColor("#A94721", "#EFA276"))
}

func accentRule(width int) string {
	return lipgloss.NewStyle().Foreground(ThemeColor("#A94721", "#EFA276")).Render(strings.Repeat("─", max(1, width)))
}

func padPane(lines []string, width, height int) []string {
	if len(lines) < height {
		lines = append(lines, make([]string, height-len(lines))...)
	}
	for i := range lines {
		lines[i] = lipgloss.NewStyle().Width(width).Render(ansi.Truncate(lines[i], width, "…"))
	}
	return lines
}

func welcomeFrame(title string, left, right []string, rightWidth, width int) []string {
	topText := "╭─ " + title + " "
	fill := max(0, width-ansi.StringWidth(topText)-1)
	rows := []string{accentStyle().Render(topText + strings.Repeat("─", fill) + "╮")}
	for i := range left {
		rows = append(rows,
			"│ "+left[i]+" │ "+right[i]+strings.Repeat(" ", max(0, rightWidth-ansi.StringWidth(right[i])))+" │",
		)
	}
	bottom := "╰" + strings.Repeat("─", max(0, width-2)) + "╯"
	rows = append(rows, accentStyle().Render(bottom))
	for i := range rows {
		rows[i] = ansi.Truncate(rows[i], width, "…")
	}
	return rows
}
