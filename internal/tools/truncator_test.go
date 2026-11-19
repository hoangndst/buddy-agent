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
	maxLimit := 200
	res := TruncateOutput(long, maxLimit)

	if len(res) > maxLimit {
		t.Fatalf("expected truncated length <= %d, got %d", maxLimit, len(res))
	}
	if !strings.HasPrefix(res, "AAA") {
		t.Errorf("expected prefix of A's preserved")
	}
	if !strings.HasSuffix(res, "BBB") {
		t.Errorf("expected suffix of B's preserved")
	}
	if !strings.Contains(res, "TRUNCATED") {
		t.Errorf("expected truncation marker in text")
	}

	// Boundary limits
	for _, limit := range []int{5, 20, 50, 100, 500} {
		out := TruncateOutput(long, limit)
		if len(out) > limit {
			t.Errorf("limit %d violated: got len %d", limit, len(out))
		}
	}
}
