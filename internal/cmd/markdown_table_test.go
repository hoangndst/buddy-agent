package cmd

import (
	"fmt"
	"strings"
	"testing"
)

func TestFormatMarkdownTableBasic(t *testing.T) {
	lines := []string{
		"| Name | Role | Status |",
		"| --- | --- | --- |",
		"| Alice | Developer | Active |",
		"| Bob | Designer | Pending |",
	}

	res := FormatMarkdownTable(lines, 80, 200)
	if res.OmittedRows != 0 {
		t.Fatalf("expected 0 omitted rows, got %d", res.OmittedRows)
	}
	if !strings.Contains(res.Rendered, "Alice") || !strings.Contains(res.Rendered, "Bob") {
		t.Fatalf("rendered table missing rows: %s", res.Rendered)
	}
}

func TestFormatMarkdownTableCapping(t *testing.T) {
	var lines []string
	lines = append(lines, "| ID | Value |")
	lines = append(lines, "| --- | --- |")
	for i := 1; i <= 250; i++ {
		lines = append(lines, fmt.Sprintf("| row_%d | value_%d |", i, i))
	}

	res := FormatMarkdownTable(lines, 80, 200)
	if res.TotalRows != 250 {
		t.Fatalf("expected 250 total rows, got %d", res.TotalRows)
	}
	if res.OmittedRows != 50 {
		t.Fatalf("expected 50 omitted rows, got %d", res.OmittedRows)
	}
	if !strings.Contains(res.Rendered, "[... 50 rows omitted; use /copy to export full table]") {
		t.Fatalf("missing omitted badge in output: %s", res.Rendered)
	}
	if strings.Contains(res.Rendered, "row_201") {
		t.Fatalf("row_201 should have been truncated")
	}
	if !strings.Contains(res.Rendered, "row_200") {
		t.Fatalf("row_200 should be visible")
	}
}

func TestFormatMarkdownTableWidthTruncation(t *testing.T) {
	lines := []string{
		"| Short | Very Long Column Text That Exceeds Terminal Width Extensively |",
		"| --- | --- |",
		"| Key | A extraordinarily long explanation that definitely needs column budgeting and cell truncation |",
	}

	res := FormatMarkdownTable(lines, 40, 200)
	if !strings.Contains(res.Rendered, "...") {
		t.Fatalf("expected cell truncation with ellipsis in narrow width: %s", res.Rendered)
	}
}

func TestProcessMarkdownTablesInText(t *testing.T) {
	content := "Here is some introductory text.\n\n| Col A | Col B |\n| --- | --- |\n| 1 | 2 |\n\nAnd some trailing text."
	processed := ProcessMarkdownTables(content, 80, 200)

	if !strings.Contains(processed, "Here is some introductory text.") {
		t.Fatalf("intro text missing: %s", processed)
	}
	if !strings.Contains(processed, "And some trailing text.") {
		t.Fatalf("trailing text missing: %s", processed)
	}
	if !strings.Contains(processed, "| Col A") {
		t.Fatalf("table missing: %s", processed)
	}
}
