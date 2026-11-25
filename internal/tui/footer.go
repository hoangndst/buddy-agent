package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Footer contains pre-rendered UI data for the transcript's bottom region.
type Footer struct {
	Width, Height              int
	Emergency, Standard        bool
	PendingApproval            bool
	ApprovalView, ApprovalKeys string
	Queued, Steering           int
	Error                      string
	Notice                     string
	HasError                   bool
	Completion                 []string
	Composer                   string
	State                      string
	Standalone                 bool
	TranscriptFocused          bool
}

// RenderFooter lays out status, approvals, completions, and the composer.
func RenderFooter(f Footer) []string {
	var rows []string
	if f.PendingApproval {
		if f.Emergency {
			if f.Height <= 1 {
				return []string{f.ApprovalKeys}
			}
			return fitFooterRows(strings.Split(f.ApprovalView, "\n"), f.Height)
		}
		rows = append(rows, strings.Split(f.ApprovalView, "\n")...)
	}
	if f.Queued+f.Steering > 0 {
		rows = append(rows, mutedStyle.Render(fmt.Sprintf("%d queued · %d steering", f.Queued, f.Steering)))
	}
	if f.Error != "" {
		rows = append(rows, f.Error)
	}
	if f.Notice != "" {
		rows = append(rows, mutedStyle.Render(f.Notice))
	}
	rows = append(rows, f.Completion...)
	if f.Height <= 0 {
		return rows
	}
	if !f.PendingApproval || !f.Emergency {
		input := f.Composer
		if f.PendingApproval {
			input = mutedStyle.Render(input)
		}
		if f.Standard {
			rows = append(rows, mutedStyle.Render(strings.Repeat("─", max(1, f.Width))))
		}
		rows = append(rows, strings.Split(input, "\n")...)
		if f.Standard {
			rows = append(rows, mutedStyle.Render(strings.Repeat("─", max(1, f.Width))))
		}
	}
	if !f.Emergency {
		connection := "Connected"
		if f.HasError {
			connection = "Needs attention"
		}
		footer := lipgloss.NewStyle().Foreground(ThemeColor("#387E61", "#78B99A")).Render("● "+connection) + "  " + mutedStyle.Render("? shortcuts · / commands")
		if f.State == "running" {
			footer = lipgloss.NewStyle().Foreground(ThemeColor("#387E61", "#78B99A")).Render("● Connected") + "  " + lipgloss.NewStyle().Bold(true).Foreground(ThemeColor("#A94721", "#EFA276")).Render("Working…") + "  " + mutedStyle.Render("/interrupt · Ctrl+C detach")
		} else if f.Standard {
			extra := "Enter send · Shift+Enter newline · Ctrl+C detach"
			if f.Standalone {
				extra = strings.ReplaceAll(extra, "Ctrl+C detach", "Ctrl+C stop")
			}
			footer += mutedStyle.Render("  " + extra)
		}
		if f.PendingApproval {
			footer = "Approval · " + f.ApprovalKeys
		} else if f.Standard && f.TranscriptFocused {
			footer = "Transcript · ↑/↓ select · Enter inspect · PgUp/PgDn scroll · F6 composer"
		}
		rows = append(rows, mutedStyle.Render(ansi.Truncate(footer, max(1, f.Width), "…")))
	}
	return fitFooterRows(rows, max(1, f.Height))
}

func fitFooterRows(rows []string, height int) []string {
	if height <= 0 {
		return nil
	}
	if len(rows) > height {
		return rows[len(rows)-height:]
	}
	return rows
}
