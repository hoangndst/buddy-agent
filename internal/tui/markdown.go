package tui

import (
	"os"
	"strconv"
	"strings"

	"charm.land/glamour/v2"
	"github.com/charmbracelet/x/ansi"
)

// RenderMarkdown renders source Markdown for the available terminal width.
func RenderMarkdown(source string, width int) (string, error) {
	if width < 1 {
		width = 1
	}
	style := os.Getenv("GLAMOUR_STYLE")
	if style == "" {
		style = "dark"
		if background := os.Getenv("COLORFGBG"); background != "" {
			if value, err := strconv.Atoi(background[strings.LastIndex(background, ";")+1:]); err == nil && value > 7 {
				style = "light"
			}
		}
	}
	renderer, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width))
	if err != nil {
		return "", err
	}
	rendered, err := renderer.Render(source)
	if err != nil {
		return "", err
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		rendered = ansi.Strip(rendered)
		lines := strings.Split(rendered, "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " ")
		}
		rendered = strings.Join(lines, "\n")
	}
	return strings.TrimRight(rendered, "\n"), nil
}
