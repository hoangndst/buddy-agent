package tui

import "testing"

func TestThemeColorAdaptsToBackground(t *testing.T) {
	t.Setenv("COLORFGBG", "0;15")
	light := ThemeColor("#111111", "#eeeeee")
	r, _, _, _ := light.RGBA()
	if r != 0x1111 {
		t.Fatalf("light terminal color red = %#x", r)
	}
	t.Setenv("COLORFGBG", "15;0")
	dark := ThemeColor("#111111", "#eeeeee")
	r, _, _, _ = dark.RGBA()
	if r != 0xeeee {
		t.Fatalf("dark terminal color red = %#x", r)
	}
}
