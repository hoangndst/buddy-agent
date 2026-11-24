package tui

import (
	"image/color"
	"os"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// ThemeColor selects a foreground or accent color for detected light/dark terminals.
func ThemeColor(light, dark string) color.Color {
	if isLightTerminal() {
		return lipgloss.Color(light)
	}
	return lipgloss.Color(dark)
}

func isLightTerminal() bool {
	background := os.Getenv("COLORFGBG")
	if background == "" {
		return false
	}
	value, err := strconv.Atoi(background[strings.LastIndex(background, ";")+1:])
	return err == nil && value > 7
}

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(ThemeColor("#A94721", "#EFA276"))
	mutedStyle   = lipgloss.NewStyle().Foreground(ThemeColor("#62656D", "#9297A3"))
	userStyle    = lipgloss.NewStyle().Bold(true).Foreground(ThemeColor("#27609B", "#78B9EF"))
	buddyStyle   = lipgloss.NewStyle().Bold(true).Foreground(ThemeColor("#A94721", "#EFA276"))
	toolStyle    = lipgloss.NewStyle().Foreground(ThemeColor("#555963", "#B8B8B8"))
	thoughtStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#929292")).Italic(true)
)
