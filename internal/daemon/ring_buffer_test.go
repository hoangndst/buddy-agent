package daemon

import (
	"testing"

	"github.com/hoangndst/buddy-agent/internal/agent"
)

func TestEventRingBuffer_PushAndEviction(t *testing.T) {
	rb := NewEventRingBuffer(3)
	if rb.Capacity() != 3 {
		t.Fatalf("expected capacity 3, got %d", rb.Capacity())
	}

	// 1. Push 3 items (fill capacity)
	id1 := rb.Push(agent.Event{Type: agent.EventReasoning, Content: "item 1"})
	id2 := rb.Push(agent.Event{Type: agent.EventReasoning, Content: "item 2"})
	id3 := rb.Push(agent.Event{Type: agent.EventReasoning, Content: "item 3"})

	if id1 != 1 || id2 != 2 || id3 != 3 {
		t.Fatalf("expected IDs 1, 2, 3, got %d, %d, %d", id1, id2, id3)
	}
	if rb.Len() != 3 {
		t.Fatalf("expected len 3, got %d", rb.Len())
	}

	// 2. Replay all from afterID 0
	all := rb.ReplayAfter(0)
	if len(all) != 3 {
		t.Fatalf("expected 3 items, got %d", len(all))
	}
	if all[0].Event.Content != "item 1" || all[2].Event.Content != "item 3" {
		t.Fatalf("unexpected items in replay: %+v", all)
	}

	// 3. Push 4th item (triggers eviction of item 1)
	id4 := rb.Push(agent.Event{Type: agent.EventReasoning, Content: "item 4"})
	if id4 != 4 {
		t.Fatalf("expected ID 4, got %d", id4)
	}
	if rb.Len() != 3 {
		t.Fatalf("expected len to remain 3 after eviction, got %d", rb.Len())
	}

	afterEvict := rb.ReplayAfter(0)
	if len(afterEvict) != 3 {
		t.Fatalf("expected 3 items after eviction, got %d", len(afterEvict))
	}
	if afterEvict[0].Event.Content != "item 2" || afterEvict[2].Event.Content != "item 4" {
		t.Fatalf("expected item 1 evicted; got %+v", afterEvict)
	}

	// 4. Replay from cursor afterID 2
	cursorReplay := rb.ReplayAfter(2)
	if len(cursorReplay) != 2 {
		t.Fatalf("expected 2 items after cursor 2, got %d", len(cursorReplay))
	}
	if cursorReplay[0].EventID != 3 || cursorReplay[1].EventID != 4 {
		t.Fatalf("unexpected replay after cursor: %+v", cursorReplay)
	}
}
