package tui

import (
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// MascotSize selects the terminal pixel dimensions used by RenderMascot.
type MascotSize uint8

const (
	// MascotWelcome renders 32 columns by 15 rows for the welcome header.
	MascotWelcome MascotSize = iota
	// MascotCompact renders 24 columns by 11 rows.
	MascotCompact
	// MascotLarge renders 40 columns by 19 rows.
	MascotLarge
)

var mascotPalette = [7]string{"", "#100D0D", "#FFAD65", "#FBF0E5", "#D97745", "#FA9DA8", "#FFFFFF"}

func buildMascotStyles() [7][7]lipgloss.Style {
	var styles [7][7]lipgloss.Style
	palette := mascotPalette
	if !isLightTerminal() {
		palette[1] = "#29252B"
	}
	for top := 0; top < len(mascotPalette); top++ {
		for bottom := 0; bottom < len(mascotPalette); bottom++ {
			style := lipgloss.NewStyle()
			if top > 0 {
				style = style.Foreground(lipgloss.Color(palette[top]))
			}
			if bottom > 0 {
				style = style.Background(lipgloss.Color(palette[bottom]))
			}
			styles[top][bottom] = style
		}
	}
	return styles
}

// RenderMascot converts the Buddy SVG palette into ANSI half-block pixels.
// Welcome uses a 32 by 15 cell sprite; Compact 24 by 11, and Large 40 by 19.
func RenderMascot(size MascotSize) []string {
	sprite := strings.Split(mascotSprite32, "\n")
	switch size {
	case MascotCompact:
		sprite = strings.Split(mascotSprite24, "\n")
	case MascotLarge:
		sprite = strings.Split(mascotSprite40, "\n")
	}
	pixelWidth := len(sprite[0])
	cellRows := len(sprite) / 2
	rows := make([]string, 0, cellRows)
	colorEnabled := mascotColorEnabled()
	var styles [7][7]lipgloss.Style
	if colorEnabled {
		styles = buildMascotStyles()
	}
	for cellY := 0; cellY < cellRows; cellY++ {
		var row strings.Builder
		for x := 0; x < pixelWidth; x++ {
			top := mascotPixel(sprite[cellY*2][x])
			bottom := mascotPixel(sprite[cellY*2+1][x])
			switch {
			case top == 0 && bottom == 0:
				row.WriteByte(' ')
			case !colorEnabled:
				if top == 0 {
					row.WriteRune(monochromePixel(bottom))
				} else if bottom == 0 {
					row.WriteRune('▀')
				} else {
					row.WriteRune(monochromePixel((top + bottom) / 2))
				}
			case top == bottom:
				row.WriteString(styles[0][top].Render(" "))
			case top == 0:
				row.WriteString(styles[bottom][0].Render("▄"))
			case bottom == 0:
				row.WriteString(styles[top][0].Render("▀"))
			default:
				row.WriteString(styles[top][bottom].Render("▀"))
			}
		}
		rows = append(rows, strings.TrimRight(row.String(), " "))
	}
	return rows
}

func mascotPixel(pixel byte) uint8 {
	switch pixel {
	case 'K':
		return 1
	case 'O':
		return 2
	case 'C':
		return 3
	case 'P':
		return 5
	case 'W':
		return 6
	default:
		return 0
	}
}

func monochromePixel(index uint8) rune {
	switch index {
	case 1:
		return '█'
	case 2, 4:
		return '▓'
	case 3, 5:
		return '▒'
	case 6:
		return '░'
	default:
		return ' '
	}
}

func mascotColorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CLICOLOR") == "0" || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	if os.Getenv("TERM") == "" && os.Getenv("COLORTERM") == "" {
		return false
	}
	return true
}
