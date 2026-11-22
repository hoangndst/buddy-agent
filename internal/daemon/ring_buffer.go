package daemon

import (
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
)

// DefaultRingBufferCapacity defines the default capacity of circular event buffers (1,000 events).
const DefaultRingBufferCapacity = 1000

// BufferedEvent encapsulates an agent event with an assigned sequence ID and timestamp.
type BufferedEvent struct {
	EventID   uint64      `json:"event_id"`
	Timestamp time.Time   `json:"timestamp"`
	Event     agent.Event `json:"event"`
	Terminal  bool        `json:"terminal"`
	TurnID    string      `json:"turn_id,omitempty"`
}

// EventRingBuffer provides thread-safe, memory-bounded circular storage for intra-session events.
type EventRingBuffer struct {
	mu       sync.RWMutex
	capacity int
	buffer   []BufferedEvent
	head     int
	tail     int
	size     int
	nextID   uint64
}

// NewEventRingBuffer creates a new EventRingBuffer with the specified capacity.
func NewEventRingBuffer(capacity int) *EventRingBuffer {
	if capacity <= 0 {
		capacity = DefaultRingBufferCapacity
	}
	return &EventRingBuffer{
		capacity: capacity,
		buffer:   make([]BufferedEvent, capacity),
		nextID:   1,
	}
}

// Push appends an event to the ring buffer, overwriting the oldest element if full.
func (rb *EventRingBuffer) Push(evt agent.Event) uint64 {
	return rb.PushWithTerminal(evt, false)
}

// PushWithTerminal appends an event to the ring buffer with terminal flag.
func (rb *EventRingBuffer) PushWithTerminal(evt agent.Event, terminal bool) uint64 {
	return rb.PushWithIdentity(evt, terminal, "")
}

func (rb *EventRingBuffer) PushWithIdentity(evt agent.Event, terminal bool, turnID string) uint64 {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	id := rb.nextID
	rb.nextID++

	item := BufferedEvent{
		EventID:   id,
		Timestamp: time.Now(),
		Event:     evt,
		Terminal:  terminal,
		TurnID:    turnID,
	}

	rb.buffer[rb.tail] = item
	rb.tail = (rb.tail + 1) % rb.capacity

	if rb.size < rb.capacity {
		rb.size++
	} else {
		rb.head = (rb.head + 1) % rb.capacity // Evict oldest entry
	}
	return id
}

// ReplayAfter returns all buffered events with EventID strictly greater than afterID.
func (rb *EventRingBuffer) ReplayAfter(afterID uint64) []BufferedEvent {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	var result []BufferedEvent
	for i := 0; i < rb.size; i++ {
		idx := (rb.head + i) % rb.capacity
		if rb.buffer[idx].EventID > afterID {
			result = append(result, rb.buffer[idx])
		}
	}
	return result
}

// Len returns the current count of buffered events.
func (rb *EventRingBuffer) Len() int {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.size
}

// Capacity returns the maximum capacity of the buffer.
func (rb *EventRingBuffer) Capacity() int {
	return rb.capacity
}

func (rb *EventRingBuffer) Watermark() uint64 {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	return rb.nextID - 1
}

func (rb *EventRingBuffer) EarliestID() uint64 {
	rb.mu.RLock()
	defer rb.mu.RUnlock()
	if rb.size == 0 {
		return rb.nextID
	}
	return rb.buffer[rb.head].EventID
}
