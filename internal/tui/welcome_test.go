package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderWelcomeSnapshots(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, tc := range []struct {
		name string
		view Welcome
	}{
		{name: "large", view: Welcome{Width: 160, Height: 40, Model: "GPT-5.4", Workspace: "~/Developer/buddy-agent", Version: "dev", Status: "Connected"}},
		{name: "compact", view: Welcome{Width: 100, Height: 24, Model: "GPT-5.4", Workspace: "~/Developer/buddy-agent", Version: "dev", Status: "Connected"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ansi.Strip(strings.Join(RenderWelcome(tc.view), "\n")) + "\n"
			path := filepath.Join("testdata", "welcome_"+tc.name+".golden")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("welcome snapshot mismatch\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestMascotVariantsAndNoColorFallback(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	for _, tc := range []struct {
		size MascotSize
		rows int
		cols int
	}{{MascotWelcome, 15, 32}, {MascotCompact, 11, 24}, {MascotLarge, 19, 40}} {
		art := RenderMascot(tc.size)
		if len(art) != tc.rows {
			t.Fatalf("mascot rows = %d, want %d", len(art), tc.rows)
		}
		for _, line := range art {
			if width := ansi.StringWidth(line); width > tc.cols {
				t.Fatalf("mascot line width %d exceeds %d: %q", width, tc.cols, line)
			}
		}
	}

	t.Setenv("NO_COLOR", "1")
	for _, line := range RenderMascot(MascotCompact) {
		if strings.Contains(line, "\x1b") {
			t.Fatalf("NO_COLOR mascot contains terminal styling: %q", line)
		}
	}
}

func TestMascotSpriteRowsAreRectangular(t *testing.T) {
	for _, sprite := range []string{mascotSprite24, mascotSprite32, mascotSprite40} {
		rows := strings.Split(sprite, "\n")
		if len(rows)%2 != 0 {
			t.Errorf("sprite has odd pixel height %d", len(rows))
		}
		for row, pixels := range rows {
			if len(pixels) != len(rows[0]) {
				t.Errorf("sprite row %d width = %d, want %d", row, len(pixels), len(rows[0]))
			}
		}
	}
}

func TestWelcomeResizeAndNarrowFallback(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, tc := range []struct {
		width, height int
		wantMascot    bool
	}{{160, 40, true}, {100, 24, true}, {80, 18, false}, {60, 18, false}, {40, 12, false}} {
		rows := RenderWelcome(Welcome{Width: tc.width, Height: tc.height, Workspace: "/workspace/buddy", Version: "dev"})
		joined := strings.Join(rows, "\n")
		if strings.Contains(joined, "▀") != tc.wantMascot {
			t.Errorf("%dx%d mascot presence mismatch: %q", tc.width, tc.height, joined)
		}
		if len(rows) > tc.height {
			t.Errorf("%dx%d welcome has %d rows", tc.width, tc.height, len(rows))
		}
		for _, row := range rows {
			if width := ansi.StringWidth(row); width > tc.width {
				t.Errorf("%dx%d welcome row width %d: %q", tc.width, tc.height, width, row)
			}
		}
	}
}
