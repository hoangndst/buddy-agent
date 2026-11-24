package tui

import (
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// MascotSize selects the terminal pixel dimensions used by RenderMascot.
type MascotSize uint8

const (
	// MascotCompact renders 24 columns by 12 rows.
	MascotCompact MascotSize = iota
	// MascotLarge renders 48 columns by 24 rows.
	MascotLarge
)

const mascotPixelWidth = 48

// Pixel indices below are generated from assets/buddy-favicon.svg at 48 by 48 pixels.
const mascotPixelGrid = "000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000" +
	"000000000000000000000000000000000001111110000000000000111100000000000000000000000112222211000000" +
	"000001122211000000000000000000001122222221000000000012222222110000000000000000011222222222100000" +
	"000142222222241000000000000000112222222222100000000122222222222100000000000001122222111222210000" +
	"001122224222222210000000000001222221555122210000001222214414222221000000000012222215555442210000" +
	"001222155551122222100000000012222145555512210000001222155555412222210000000122222155555512241000" +
	"001222455555541222211000000122221555555512221000001221455555551122221001111222221555555542221000" +
	"001221455555555122222114555222214555555542221000001221455555555512222222566322241555555542221000" +
	"001224455555555512222222233332222115555512241000001222155555554122222222223335222241555512241000" +
	"001222155555541222222222223333222222145512210000001222155555412222222222223333222222214512210000" +
	"001422445554122222222222223333222222221122210000000122215541222222222222223333522222222122210000" +
	"000122214512222222222222223333322222222412100000000112221122222222211222223333322211112211100000" +
	"000012221122222221111142253333352111351221000000000011221222222211116312233333331111351121000000" +
	"000001221222222211116611233333361111111124100000000000114222222411111111333511154114111122110000" +
	"000000012222222211441111336411155111111433341000000000012225522211111115633341436411114533364100" +
	"000000143333333341111143536631563534455553333100000001463333335555411536515411141566355555333100" +
	"000001333333355555536633641111114633355553334100000001333333355555533333641555515633333333311000" +
	"000001366333335555533333354555513333333333651000000001454333333333333333331455446333333333331000" +
	"000000011333333333333333333144133333333333331000000000014633333333333333333355333333333333651000" +
	"000000004633333333333333333366333333333653510000000000001566536333333333333333333333336511100000" +
	"000000001144115363333333333333333333634100000000000000000011001143366633333333366635411000000000" +
	"000000000000000011144553333333354411100000000000000000000000000000001111111111111000000000000000" +
	"000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000" +
	"000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"

var mascotPalette = [7]string{"", "#100D0D", "#FFAD65", "#FBF0E5", "#8050BF", "#FA9DA8", "#FFFFFF"}

func buildMascotStyles() [7][7]lipgloss.Style {
	var styles [7][7]lipgloss.Style
	palette := mascotPalette
	if !isLightTerminal() {
		palette[1] = "#29252B"
	}
	for top := 1; top < len(mascotPalette); top++ {
		for bottom := 0; bottom < len(mascotPalette); bottom++ {
			style := lipgloss.NewStyle().Foreground(lipgloss.Color(palette[top]))
			if bottom > 0 {
				style = style.Background(lipgloss.Color(palette[bottom]))
			}
			styles[top][bottom] = style
		}
	}
	return styles
}

// RenderMascot converts the checked-in Buddy SVG raster into ANSI half-block pixels.
// Compact uses a 24 by 12 cell canvas; Large uses 48 by 24 cells.
func RenderMascot(size MascotSize) []string {
	pixelSize := mascotPixelWidth
	if size == MascotCompact {
		pixelSize /= 2
	}
	cellRows := (pixelSize + 1) / 2
	rows := make([]string, 0, cellRows)
	colorEnabled := mascotColorEnabled()
	var styles [7][7]lipgloss.Style
	if colorEnabled {
		styles = buildMascotStyles()
	}
	for cellY := 0; cellY < cellRows; cellY++ {
		var row strings.Builder
		for x := 0; x < pixelSize; x++ {
			topY, bottomY := cellY*2, cellY*2+1
			top := mascotPixel(topY, x, pixelSize)
			bottom := uint8(0)
			if cellY*2+1 < pixelSize {
				bottom = mascotPixel(bottomY, x, pixelSize)
			}
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

func mascotPixel(y, x, width int) uint8 {
	if width == mascotPixelWidth {
		return mascotPixelGrid[y*mascotPixelWidth+x] - '0'
	}
	sourceY, sourceX := y*mascotPixelWidth/width, x*mascotPixelWidth/width
	return mascotPixelGrid[sourceY*mascotPixelWidth+sourceX] - '0'
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
