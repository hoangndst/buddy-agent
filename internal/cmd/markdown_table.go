package cmd

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// DefaultMaxTableRows is the maximum number of markdown table rows rendered visually.
const DefaultMaxTableRows = 200

// FormattedTable holds the visually formatted table and its raw source.
type FormattedTable struct {
	Rendered    string
	OmittedRows int
	TotalRows   int
}

// ProcessMarkdownTables detects markdown table blocks in text and renders them with
// width-aware column allocation and row truncation capping.
func ProcessMarkdownTables(text string, maxWidth int, maxRows int) string {
	if maxWidth <= 0 {
		maxWidth = 80
	}
	if maxRows <= 0 {
		maxRows = DefaultMaxTableRows
	}

	lines := strings.Split(text, "\n")
	var out []string
	var tableLines []string
	inTable := false

	flushTable := func() {
		if len(tableLines) == 0 {
			return
		}
		formatted := FormatMarkdownTable(tableLines, maxWidth, maxRows)
		out = append(out, formatted.Rendered)
		tableLines = tableLines[:0]
		inTable = false
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isTableLine(trimmed) {
			inTable = true
			tableLines = append(tableLines, line)
		} else {
			if inTable {
				flushTable()
			}
			out = append(out, line)
		}
	}
	if inTable {
		flushTable()
	}

	return strings.Join(out, "\n")
}

func isTableLine(line string) bool {
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return false
	}
	return strings.Count(line, "|") >= 2
}

func isDelimiterRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		trimmed := strings.TrimSpace(cell)
		if trimmed == "" {
			return false
		}
		for _, r := range trimmed {
			if r != '-' && r != ':' {
				return false
			}
		}
	}
	return true
}

func splitRowCells(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	parts := strings.Split(trimmed, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// FormatMarkdownTable formats markdown table lines with width constraints and row caps.
func FormatMarkdownTable(lines []string, maxWidth int, maxRows int) FormattedTable {
	if len(lines) == 0 {
		return FormattedTable{}
	}

	var rawRows [][]string
	for _, line := range lines {
		rawRows = append(rawRows, splitRowCells(line))
	}

	if len(rawRows) < 2 || !isDelimiterRow(rawRows[1]) {
		// Not a standard markdown table; return as-is
		return FormattedTable{
			Rendered:    strings.Join(lines, "\n"),
			TotalRows:   len(lines),
			OmittedRows: 0,
		}
	}

	numCols := len(rawRows[0])
	colWidths := make([]int, numCols)

	// Measure content widths
	for _, row := range rawRows {
		for i, cell := range row {
			if i < numCols {
				w := utf8.RuneCountInString(cell)
				if w > colWidths[i] {
					colWidths[i] = w
				}
			}
		}
	}

	// Ensure min width for delimiters
	for i := range colWidths {
		if colWidths[i] < 3 {
			colWidths[i] = 3
		}
	}

	// Calculate total width including borders: "| cell | cell |" -> numCols*3 + 1
	overhead := (numCols * 3) + 1
	totalNeeded := overhead
	for _, w := range colWidths {
		totalNeeded += w
	}

	// If table exceeds maxWidth, distribute available budget proportionately
	if totalNeeded > maxWidth && maxWidth > overhead {
		availableForContent := maxWidth - overhead
		if availableForContent < numCols*3 {
			availableForContent = numCols * 3
		}
		sumOriginal := 0
		for _, w := range colWidths {
			sumOriginal += w
		}
		for i := range colWidths {
			scaled := (colWidths[i] * availableForContent) / sumOriginal
			if scaled < 3 {
				scaled = 3
			}
			colWidths[i] = scaled
		}
	}

	// Truncate rows if exceeding maxRows (excluding header and delimiter)
	headerRow := rawRows[0]
	delimRow := rawRows[1]
	dataRows := rawRows[2:]

	omitted := 0
	visibleRows := dataRows
	if len(dataRows) > maxRows {
		omitted = len(dataRows) - maxRows
		visibleRows = dataRows[:maxRows]
	}

	renderRow := func(cells []string, isDelim bool) string {
		var b strings.Builder
		b.WriteString("|")
		for i := 0; i < numCols; i++ {
			b.WriteString(" ")
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			width := colWidths[i]
			if isDelim {
				b.WriteString(strings.Repeat("-", width))
			} else {
				cellLen := utf8.RuneCountInString(cell)
				if cellLen > width {
					if width > 3 {
						runes := []rune(cell)
						cell = string(runes[:width-3]) + "..."
					} else {
						runes := []rune(cell)
						cell = string(runes[:width])
					}
					cellLen = utf8.RuneCountInString(cell)
				}
				b.WriteString(cell)
				if width > cellLen {
					b.WriteString(strings.Repeat(" ", width-cellLen))
				}
			}
			b.WriteString(" |")
		}
		return b.String()
	}

	var renderedLines []string
	renderedLines = append(renderedLines, renderRow(headerRow, false))
	renderedLines = append(renderedLines, renderRow(delimRow, true))
	for _, row := range visibleRows {
		renderedLines = append(renderedLines, renderRow(row, false))
	}

	if omitted > 0 {
		badge := fmt.Sprintf("\n[... %d rows omitted; use /copy to export full table]", omitted)
		renderedLines = append(renderedLines, badge)
	}

	return FormattedTable{
		Rendered:    strings.Join(renderedLines, "\n"),
		TotalRows:   len(dataRows),
		OmittedRows: omitted,
	}
}
