package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestMascotColorPixelsUseHalfBlockOrBackgroundSpace(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "")

	rows := strings.Split(mascotSprite32, "\n")
	got := RenderMascot(MascotWelcome)
	want := make([]string, len(rows)/2)
	foundSolid, foundTransparent := false, false
	var solidColors [7]bool
	for y := 0; y < len(rows); y += 2 {
		var line strings.Builder
		lastPixel := -1
		for x := range rows[y] {
			if mascotPixel(rows[y][x]) != 0 || mascotPixel(rows[y+1][x]) != 0 {
				lastPixel = x
			}
		}
		for x := 0; x <= lastPixel; x++ {
			top, bottom := mascotPixel(rows[y][x]), mascotPixel(rows[y+1][x])
			switch {
			case top == 0 && bottom == 0:
				line.WriteByte(' ')
				foundTransparent = true
			case top == bottom:
				line.WriteByte(' ')
				foundSolid = true
				solidColors[top] = true
			case top == 0:
				line.WriteRune('▄')
			default:
				line.WriteRune('▀')
			}
		}
		want[y/2] = line.String()
	}
	if gotPlain := ansi.Strip(strings.Join(got, "\n")); gotPlain != strings.Join(want, "\n") {
		t.Fatalf("mascot glyphs do not match pixel pairs\n got: %q\nwant: %q", gotPlain, strings.Join(want, "\n"))
	}
	if !foundSolid || !foundTransparent {
		t.Fatal("sprite must exercise both solid-color and transparent cells")
	}
	if !strings.HasPrefix(got[0], " ") {
		t.Fatal("leading transparent pixels should remain unstyled spaces")
	}

	styles := buildMascotStyles()
	foundMixed, foundUpperOnly, foundLowerOnly := false, false, false
	for y := 0; y < len(rows); y += 2 {
		for x := range rows[y] {
			top, bottom := mascotPixel(rows[y][x]), mascotPixel(rows[y+1][x])
			var style lipgloss.Style
			var glyph string
			switch {
			case top > 0 && bottom > 0 && top != bottom:
				style, glyph, foundMixed = styles[top][bottom], "▀", true
			case top > 0 && bottom == 0:
				style, glyph, foundUpperOnly = styles[top][0], "▀", true
			case top == 0 && bottom > 0:
				style, glyph, foundLowerOnly = styles[bottom][0], "▄", true
			default:
				continue
			}
			if !strings.Contains(got[y/2], style.Render(glyph)) {
				t.Errorf("pixel pair %q/%q lacks expected ANSI color mapping", rows[y][x], rows[y+1][x])
			}
		}
	}
	if !foundMixed || !foundUpperOnly || !foundLowerOnly {
		t.Fatalf("sprite must exercise mixed and transparent half-block pairs: mixed=%v upper=%v lower=%v", foundMixed, foundUpperOnly, foundLowerOnly)
	}
	for _, pixel := range []byte{'K', 'O', 'C', 'P', 'W'} {
		colorIndex := mascotPixel(pixel)
		if !solidColors[colorIndex] {
			continue
		}
		wantSolid := styles[0][colorIndex].Render(" ")
		if !strings.Contains(wantSolid, "\x1b[") {
			t.Fatal("color test did not enable ANSI rendering")
		}
		if strings.Contains(wantSolid, "\x1b[38;") || !strings.Contains(wantSolid, "\x1b[48;") {
			t.Errorf("same-color %q pixels must use background color only: %q", pixel, wantSolid)
		}
		if !strings.Contains(strings.Join(got, "\n"), wantSolid) {
			t.Errorf("same-color %q pixels lack a background-only space", pixel)
		}
		if strings.Contains(wantSolid, lipgloss.NewStyle().Foreground(lipgloss.Color(mascotPalette[colorIndex])).Render("▀")) {
			t.Errorf("same-color %q pixels use a half-block glyph", pixel)
		}
	}
}
