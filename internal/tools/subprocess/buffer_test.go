package subprocess

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStreamingLineTruncator_UnderBudget(t *testing.T) {
	trunc := NewStreamingLineTruncator(500)
	input := "Hello, World!\nLine 2\nLine 3\n"
	n, err := trunc.Write([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(input) {
		t.Fatalf("expected written %d, got %d", len(input), n)
	}

	res := trunc.String()
	if res != input {
		t.Fatalf("expected %q, got %q", input, res)
	}
	if trunc.TotalBytes() != int64(len(input)) {
		t.Fatalf("expected total bytes %d, got %d", len(input), trunc.TotalBytes())
	}
	if trunc.OmittedBytes() != 0 {
		t.Fatalf("expected 0 omitted bytes, got %d", trunc.OmittedBytes())
	}
	if trunc.OmittedLines() != 0 {
		t.Fatalf("expected 0 omitted lines, got %d", trunc.OmittedLines())
	}
}

func TestStreamingLineTruncator_HeadTailPartition(t *testing.T) {
	trunc := NewStreamingLineTruncator(500)

	var sb strings.Builder
	for i := 1; i <= 200; i++ {
		sb.WriteString(fmt.Sprintf("Line %03d: sample log entry\n", i))
	}
	payload := sb.String()

	_, err := trunc.Write([]byte(payload))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	res := trunc.String()
	if len(res) > 500 {
		t.Fatalf("result length %d exceeds maxBytes 500", len(res))
	}

	if !strings.Contains(res, "Line 001:") {
		t.Errorf("expected head to contain initial lines (Line 001)")
	}
	if !strings.Contains(res, "Line 200:") {
		t.Errorf("expected tail to contain final lines (Line 200)")
	}
	if !strings.Contains(res, "[... truncated:") {
		t.Errorf("expected omission marker, got:\n%s", res)
	}

	if trunc.OmittedBytes() <= 0 {
		t.Errorf("expected positive omitted bytes, got %d", trunc.OmittedBytes())
	}
	if trunc.OmittedLines() <= 0 {
		t.Errorf("expected positive omitted lines, got %d", trunc.OmittedLines())
	}
}

func TestStreamingLineTruncator_DelimiterFreeFallback(t *testing.T) {
	trunc := NewStreamingLineTruncator(300)
	// Minified continuous JSON/base64 without any newline characters
	payload := strings.Repeat("ABCDEFGHIJ1234567890", 50) // 1000 bytes

	_, err := trunc.Write([]byte(payload))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	res := trunc.String()
	if len(res) == 0 {
		t.Fatalf("expected non-empty output for delimiter-free input")
	}
	if len(res) > 300 {
		t.Fatalf("result length %d exceeds maxBytes 300", len(res))
	}
	if !strings.Contains(res, "[... truncated:") {
		t.Errorf("expected omission marker, got:\n%s", res)
	}
	if !utf8.ValidString(res) {
		t.Errorf("expected valid UTF-8 output")
	}
}

func TestStreamingLineTruncator_UTF8Integrity(t *testing.T) {
	trunc := NewStreamingLineTruncator(400)
	// Multibyte Unicode runes (3 bytes per Japanese char)
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		sb.WriteString(fmt.Sprintf("ログ行 %03d: 日本語テストデータ\n", i))
	}
	payload := sb.String()

	_, err := trunc.Write([]byte(payload))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}

	res := trunc.String()
	if !utf8.ValidString(res) {
		t.Fatalf("result contains invalid UTF-8 sequences")
	}
	if strings.Contains(res, "\uFFFD") {
		t.Fatalf("result contains replacement character \\uFFFD")
	}
	if len(res) > 400 {
		t.Fatalf("result length %d exceeds maxBytes 400", len(res))
	}
}

func TestStreamingLineTruncator_MinimumCapacityClamp(t *testing.T) {
	// Requesting an ultra-small buffer (e.g. 50 bytes) should be clamped safely to >= 256
	trunc := NewStreamingLineTruncator(50)
	if trunc.maxBytes < 256 {
		t.Fatalf("expected maxBytes clamped to >= 256, got %d", trunc.maxBytes)
	}

	payload := strings.Repeat("A", 1000)
	_, err := trunc.Write([]byte(payload))
	if err != nil {
		t.Fatalf("write failed on clamped truncator: %v", err)
	}

	res := trunc.String()
	if len(res) == 0 {
		t.Fatalf("expected non-empty result")
	}
	if len(res) > trunc.maxBytes {
		t.Fatalf("result length %d exceeds maxBytes %d", len(res), trunc.maxBytes)
	}
}
