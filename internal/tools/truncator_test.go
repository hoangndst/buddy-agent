package tools

import (
	"strings"
	"testing"
)

func TestTruncateOutput(t *testing.T) {
	short := "Hello world"
	if res := TruncateOutput(short, 100); res != short {
		t.Errorf("expected unchanged short text, got %q", res)
	}

	long := strings.Repeat("A", 1000) + strings.Repeat("B", 1000)
	res := TruncateOutput(long, 200)

	if len(res) >= len(long) {
		t.Errorf("expected truncated length to be shorter than original")
	}
	if !strings.HasPrefix(res, strings.Repeat("A", 100)) {
		t.Errorf("expected prefix of A's preserved")
	}
	if !strings.HasSuffix(res, strings.Repeat("B", 100)) {
		t.Errorf("expected suffix of B's preserved")
	}
	if !strings.Contains(res, "TRUNCATED") {
		t.Errorf("expected truncation marker in text")
	}
}
