package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// Transcript owns canonical conversation blocks, their rendered line cache,
// and the reading offset used by the fullscreen transcript viewport.
type Transcript struct {
	Blocks           []TranscriptBlock
	ScrollOffset     int
	lineCountCache   map[string]int
	wrapCache        map[string][]string
	streamingText    string
	selectedActivity string
	focused          bool
}

// SetRenderState supplies the transient presentation state used for block rendering.
func (t *Transcript) SetRenderState(streamingText, selectedActivity string, focused bool) {
	t.streamingText, t.selectedActivity, t.focused = streamingText, selectedActivity, focused
}

// ClearCache discards all rendered rows after a transcript reset or theme change.
func (t *Transcript) ClearCache() {
	t.lineCountCache = make(map[string]int)
	t.wrapCache = make(map[string][]string)
}

// Replace installs a recovered or cleared transcript without moving the reading offset.
func (t *Transcript) Replace(blocks []TranscriptBlock) {
	t.Blocks = append(t.Blocks[:0], blocks...)
	t.ClearCache()
}

// Invalidate removes cached output for a block and all measured widths.
func (t *Transcript) Invalidate(blockID string) {
	for key := range t.lineCountCache {
		if strings.HasPrefix(key, blockID+":") {
			delete(t.lineCountCache, key)
		}
	}
	for key := range t.wrapCache {
		if strings.HasPrefix(key, blockID+":") {
			delete(t.wrapCache, key)
		}
	}
}

// Index returns the block position for id, or -1 when it is absent.
func (t *Transcript) Index(id string) int {
	for i := range t.Blocks {
		if t.Blocks[i].ID == id {
			return i
		}
	}
	return -1
}

// Append adds a block while preserving the reader's current viewport anchor.
func (t *Transcript) Append(block TranscriptBlock, width int) {
	if block.ID == "" {
		block.ID = fmt.Sprintf("block-%d", time.Now().UnixNano())
	}
	if block.CreatedAt.IsZero() {
		block.CreatedAt = time.Now()
	}
	parentIndex, groupIndex, before := -1, -1, 0
	if block.ParentID != "" {
		parentIndex = t.Index(block.ParentID)
	}
	groupID := block.GroupID
	if groupID == "" && parentIndex >= 0 {
		groupID = t.Blocks[parentIndex].GroupID
	}
	if groupID != "" {
		groupIndex = t.Index(groupID)
	}
	if t.ScrollOffset > 0 {
		if groupIndex >= 0 {
			before = t.LineCount(t.Blocks[groupIndex], width)
		} else if parentIndex >= 0 {
			before = t.LineCount(t.Blocks[parentIndex], width)
		}
	}
	t.Blocks = append(t.Blocks, block)
	if groupIndex >= 0 {
		t.Invalidate(t.Blocks[groupIndex].ID)
		if t.ScrollOffset > 0 {
			t.ScrollOffset += t.LineCount(t.Blocks[groupIndex], width) - before
		}
	} else if parentIndex >= 0 {
		t.Invalidate(block.ParentID)
		if t.ScrollOffset > 0 {
			t.ScrollOffset += t.LineCount(t.Blocks[parentIndex], width) - before
		}
	} else if t.ScrollOffset > 0 {
		t.ScrollOffset += t.LineCount(block, width)
	}
}

// Update changes a block and adjusts the scroll anchor for changed row counts.
func (t *Transcript) Update(index int, update func(*TranscriptBlock), width int) {
	block := t.Blocks[index]
	parentIndex := t.Index(block.ParentID)
	groupID := block.GroupID
	if groupID == "" && parentIndex >= 0 {
		groupID = t.Blocks[parentIndex].GroupID
	}
	groupIndex := t.Index(groupID)
	before, parentBefore, groupBefore := 0, 0, 0
	if t.ScrollOffset > 0 {
		before = t.LineCount(block, width)
		if parentIndex >= 0 {
			parentBefore = t.LineCount(t.Blocks[parentIndex], width)
		}
		if groupIndex >= 0 {
			groupBefore = t.LineCount(t.Blocks[groupIndex], width)
		}
	}
	update(&t.Blocks[index])
	t.Invalidate(block.ID)
	if parentIndex >= 0 {
		t.Invalidate(block.ParentID)
	}
	if groupIndex >= 0 {
		t.Invalidate(t.Blocks[groupIndex].ID)
	}
	if t.ScrollOffset > 0 {
		t.ScrollOffset += t.LineCount(t.Blocks[index], width) - before
		if parentIndex >= 0 {
			t.ScrollOffset += t.LineCount(t.Blocks[parentIndex], width) - parentBefore
		}
		if groupIndex >= 0 {
			t.ScrollOffset += t.LineCount(t.Blocks[groupIndex], width) - groupBefore
		}
	}
}

// LineCount returns the rendered row count for a top-level block.
func (t *Transcript) LineCount(block TranscriptBlock, width int) int {
	if block.ParentID != "" || block.GroupID != "" || block.Kind == BlockAssistantMessage && !block.Streaming && strings.TrimSpace(block.Body) == "" {
		return 0
	}
	if width <= 0 {
		width = 80
	}
	if block.Kind == BlockToolActivity && !block.Completed {
		return len(t.WrappedLines(block, width)) + 1
	}
	key := fmt.Sprintf("%s:%d", block.ID, width)
	if count, ok := t.lineCountCache[key]; ok {
		return count
	}
	count := len(t.WrappedLines(block, width)) + 1
	if t.lineCountCache == nil {
		t.lineCountCache = make(map[string]int)
	}
	t.lineCountCache[key] = count
	return count
}

// WrappedLines returns cached terminal rows for a block at the requested width.
func (t *Transcript) WrappedLines(block TranscriptBlock, width int) []string {
	if width <= 0 {
		width = 80
	}
	if block.ParentID != "" || block.GroupID != "" || block.Kind == BlockAssistantMessage && !block.Streaming && strings.TrimSpace(block.Body) == "" {
		return nil
	}
	key := fmt.Sprintf("%s:%d", block.ID, width)
	if lines, ok := t.wrapCache[key]; ok {
		return lines
	}
	rendered, _ := RenderBlock(block, t.Blocks, width, t.streamingText, t.selectedActivity, t.focused)
	wrapped := WrapLines(rendered, width)
	if t.wrapCache == nil {
		t.wrapCache = make(map[string][]string)
	}
	t.wrapCache[key] = wrapped
	return wrapped
}

// WrapLines wraps rendered terminal text by display-cell width.
func WrapLines(rendered string, width int) []string {
	if width <= 0 {
		width = 80
	}
	var wrapped []string
	for line := range strings.SplitSeq(rendered, "\n") {
		if lipgloss.Width(line) <= width {
			wrapped = append(wrapped, line)
		} else {
			wrapped = append(wrapped, strings.Split(lipgloss.NewStyle().Width(width).Render(line), "\n")...)
		}
	}
	return wrapped
}

// VisibleWindow returns the blocks and row bounds intersecting the viewport.
func (t *Transcript) VisibleWindow(height, width int) ([]TranscriptBlock, int, int) {
	if len(t.Blocks) == 0 || height <= 0 {
		return nil, 0, 0
	}
	t.ScrollOffset = max(0, t.ScrollOffset)
	first, last, rows := len(t.Blocks), len(t.Blocks), 0
	for first > 0 && rows-height < t.ScrollOffset {
		first--
		rows += t.LineCount(t.Blocks[first], width)
	}
	t.ScrollOffset = min(t.ScrollOffset, max(0, rows-height))
	bottom := rows - t.ScrollOffset
	top := max(0, bottom-height)
	for last > first {
		count := t.LineCount(t.Blocks[last-1], width)
		if rows-count < bottom {
			break
		}
		rows -= count
		last--
	}
	return t.Blocks[first:last], top, bottom
}

// VisibleBlocks returns only top-level transcript blocks intersecting the viewport.
func (t *Transcript) VisibleBlocks(height, width int) []TranscriptBlock {
	blocks, _, _ := t.VisibleWindow(height, width)
	visible := make([]TranscriptBlock, 0, len(blocks))
	for _, block := range blocks {
		if t.LineCount(block, width) > 0 {
			visible = append(visible, block)
		}
	}
	return visible
}

// VisibleLines returns terminal rows for the current viewport and prunes offscreen renders.
func (t *Transcript) VisibleLines(height, width int) []string {
	blocks, top, bottom := t.VisibleWindow(height, width)
	lines := make([]string, 0, bottom-top)
	visibleIDs := make(map[string]bool)
	row := 0
	for _, block := range blocks {
		count := t.LineCount(block, width)
		if count == 0 {
			continue
		}
		visibleIDs[block.ID] = true
		wrapped := t.WrappedLines(block, width)
		start, end := max(0, top-row), min(count, bottom-row)
		lines = append(lines, wrapped[start:min(len(wrapped), end)]...)
		if end == count {
			lines = append(lines, "")
		}
		row += count
	}
	for key := range t.wrapCache {
		id, _, _ := strings.Cut(key, ":")
		if !visibleIDs[id] {
			delete(t.wrapCache, key)
		}
	}
	return lines
}

// HyperlinkAt returns a Markdown link at a row within the currently visible transcript.
func (t *Transcript) HyperlinkAt(row, column, height, width int) string {
	if row < 0 || column < 0 {
		return ""
	}
	lines := t.VisibleLines(height, width)
	if row >= len(lines) {
		return ""
	}
	return hyperlinkAt(lines[row], column)
}
