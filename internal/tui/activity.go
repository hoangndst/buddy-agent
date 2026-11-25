package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// RenderBlock formats one transcript block and its child activity rows.
func RenderBlock(block TranscriptBlock, blocks []TranscriptBlock, width int, streamingText, selectedActivity string, focused bool) (string, int) {
	if width < 1 {
		width = 80
	}
	var content string
	childOffset := -1
	switch block.Kind {
	case BlockUserMessage:
		header := block.Header
		if header == "" {
			header = "You"
		}
		content = userStyle.Render(cleanText(header)) + "\n" + cleanText(block.Body)
	case BlockAssistantMessage:
		header := block.Header
		if header == "" {
			header = "Buddy"
		}
		body := block.Body
		if block.Streaming && streamingText != "" && len(streamingText) > len(body) {
			body = streamingText
		}
		if block.Role == "reasoning" {
			content = thoughtStyle.Render(cleanText(body))
		} else if strings.TrimSpace(body) == "" {
			if !block.Streaming {
				return "", -1
			}
			content = buddyStyle.Render(cleanText(header)) + "\n" + mutedStyle.Render("Working…")
		} else {
			body = cleanText(body)
			if rendered, err := RenderMarkdown(body, max(1, width-4)); err == nil {
				body = rendered
			}
			content = buddyStyle.Render(cleanText(header)) + "\n" + body
		}
	case BlockToolActivity:
		icon, elapsed := activityPresentation(block)
		header := activitySummary(block.Header)
		if block.Collapsed {
			content = toolStyle.Render(icon + " " + header + elapsed + "  ·  Enter to expand")
		} else if block.Body == "" {
			content = toolStyle.Render(icon + " " + header + elapsed)
		} else {
			content = toolStyle.Render(icon+" "+header+elapsed) + "\n" + mutedStyle.Render(cleanText(block.Body))
		}
		children := childActivities(block.ID, blocks)
		if len(children) > 0 {
			completed := 0
			for _, child := range children {
				if child.Completed {
					completed++
				}
			}
			count := fmt.Sprintf("%d steps", len(children))
			if len(children) == 1 {
				count = "1 step"
			}
			if completed == len(children) {
				count += " completed"
			}
			content = toolStyle.Render(fmt.Sprintf("[Subagent: %s (%s)]", subagentName(block), count))
			if !block.Collapsed {
				for _, child := range children {
					childLine := "  ↳ " + cleanText(child.Header)
					if child.Activity != nil && child.Activity.OutputRef != "" {
						childLine += "  ·  Enter to inspect"
					}
					if child.ID == selectedActivity {
						childOffset = len(content)
						if focused {
							childLine = lipgloss.NewStyle().Background(lipgloss.Color("#30343b")).Width(max(1, width)).Render("› " + strings.TrimSpace(childLine))
						}
					}
					content += "\n" + childLine
					if child.Body != "" && !child.Collapsed {
						content += "\n    " + cleanText(child.Body)
					}
				}
				if block.Body != "" {
					content += "\n" + mutedStyle.Render(cleanText(block.Body))
				}
			}
		}
	case BlockSubagentActivity:
		header := block.Header
		if header == "" {
			header = "Subagent"
		}
		content = titleStyle.Render("↳ "+cleanText(header)) + "\n"
		if !block.Collapsed {
			content += cleanText(block.Body)
		}
	case BlockSystemStatus:
		content = mutedStyle.Render(cleanText(block.Body))
	default:
		content = cleanText(block.Body)
	}
	if focused && block.ID == selectedActivity {
		content = lipgloss.NewStyle().Background(lipgloss.Color("#30343b")).Width(max(1, width)).Render("› " + content)
	}
	return content, childOffset
}

func activityPresentation(block TranscriptBlock) (string, string) {
	icon := "●"
	if block.Completed {
		icon = "✓"
		if block.Activity != nil && block.Activity.ExitCode != nil && *block.Activity.ExitCode != 0 {
			icon = "!"
		}
	}
	elapsed := ""
	if block.Activity != nil && block.Activity.Elapsed > 0 {
		elapsed = "  ·  " + block.Activity.Elapsed.Round(100*time.Millisecond).String()
	} else if !block.Completed && !block.CreatedAt.IsZero() {
		elapsed = "  ·  " + time.Since(block.CreatedAt).Round(time.Second).String()
	}
	return icon, elapsed
}

func activitySummary(header string) string {
	for _, prefix := range []string{"Running ", "Done ", "Failed "} {
		header = strings.TrimPrefix(header, prefix)
	}
	return header
}

func childActivities(parentID string, blocks []TranscriptBlock) []TranscriptBlock {
	var children []TranscriptBlock
	for _, block := range blocks {
		if block.ParentID == parentID {
			children = append(children, block)
		}
	}
	return children
}

func subagentName(block TranscriptBlock) string {
	if block.Activity != nil && block.Activity.Tool == "invoke_subagent" {
		if block.Activity.AgentName != "" {
			return cleanText(block.Activity.AgentName)
		}
		return "worker"
	}
	return cleanText(block.Header)
}

func cleanText(value string) string {
	value = ansi.Strip(value)
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
}
