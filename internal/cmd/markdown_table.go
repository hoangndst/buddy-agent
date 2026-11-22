package cmd

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
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

	// Measure content widths in terminal cells
	for _, row := range rawRows {
		for i, cell := range row {
			if i < numCols {
				w := cellWidth(cell)
				if w > colWidths[i] {
					colWidths[i] = w
				}
			}
		}
	}

	// Ensure min width of 3 for each column
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

	// If table exceeds maxWidth, distribute available budget proportionately while reserving minWidth=3
	if totalNeeded > maxWidth && maxWidth > overhead {
		availableForContent := maxWidth - overhead
		minContentNeeded := numCols * 3
		if availableForContent <= minContentNeeded {
			for i := range colWidths {
				colWidths[i] = 3
			}
		} else {
			remainingBudget := availableForContent - minContentNeeded
			excess := make([]int, numCols)
			sumExcess := 0
			for i, w := range colWidths {
				if w > 3 {
					excess[i] = w - 3
					sumExcess += excess[i]
				}
			}
			if sumExcess > 0 {
				allocated := 0
				for i := range colWidths {
					add := (excess[i] * remainingBudget) / sumExcess
					colWidths[i] = 3 + add
					allocated += add
				}
				remainder := remainingBudget - allocated
				for i := 0; i < remainder && i < numCols; i++ {
					colWidths[i]++
				}
			} else {
				for i := range colWidths {
					colWidths[i] = 3
				}
			}
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
				cellText := truncateToCellWidth(cell, width)
				cellW := cellWidth(cellText)
				b.WriteString(cellText)
				if width > cellW {
					b.WriteString(strings.Repeat(" ", width-cellW))
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

func cellWidth(s string) int {
	return lipgloss.Width(s)
}

func truncateToCellWidth(s string, targetWidth int) string {
	if lipgloss.Width(s) <= targetWidth {
		return s
	}
	if targetWidth <= 0 {
		return ""
	}
	if targetWidth >= 3 {
		var b strings.Builder
		cur := 0
		for _, r := range s {
			rw := lipgloss.Width(string(r))
			if cur+rw+3 > targetWidth {
				break
			}
			b.WriteRune(r)
			cur += rw
		}
		b.WriteString("...")
		return b.String()
	}
	var b strings.Builder
	cur := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if cur+rw > targetWidth {
			break
		}
		b.WriteRune(r)
		cur += rw
	}
	return b.String()
}
