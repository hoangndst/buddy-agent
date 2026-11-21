package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
)

// ClientConn abstracts an active client connection.
type ClientConn interface {
	ID() string
	Send(msg *protocol.JSONRPCMessage) error
	SendWithTimeout(msg *protocol.JSONRPCMessage, timeout time.Duration) error
	Close() error
}

// SessionWorker coordinates headless execution of an agent session, event broadcasting,
// in-memory replay buffers, and out-of-band control signaling.
type SessionWorker struct {
	mu               sync.RWMutex
	sessionID        string
	task             string
	engine           *agent.Engine
	ctx              context.Context
	cancelFunc       context.CancelFunc
	ringBuffer       *EventRingBuffer
	controller       ClientConn
	subscribers      map[ClientConn]struct{}
	active           bool
	err              error
	result           string
	approvalChan     chan string
	pendingRequestID string
	pendingMu        sync.Mutex
	cleanup          func()
	onComplete       func()
}

// NewSessionWorker constructs a new SessionWorker bound to a ReAct engine.
func NewSessionWorker(
	parentCtx context.Context,
	sessionID string,
	task string,
	engine *agent.Engine,
	controller ClientConn,
	cleanup func(),
) *SessionWorker {
	ctx, cancel := context.WithCancel(parentCtx)
	w := &SessionWorker{
		sessionID:    sessionID,
		task:         task,
		engine:       engine,
		ctx:          ctx,
		cancelFunc:   cancel,
		ringBuffer:   NewEventRingBuffer(DefaultRingBufferCapacity),
		controller:   controller,
		subscribers:  make(map[ClientConn]struct{}),
		approvalChan: make(chan string, 1),
		cleanup:      cleanup,
	}
	if controller != nil {
		w.subscribers[controller] = struct{}{}
	}
	return w
}

// SessionID returns the identifier of the session.
func (w *SessionWorker) SessionID() string {
	return w.sessionID
}

// IsActive returns whether the session is currently executing.
func (w *SessionWorker) IsActive() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.active
}

// SetOnComplete configures a completion hook invoked after worker execution finishes.
func (w *SessionWorker) SetOnComplete(fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onComplete = fn
}

// Result returns the execution output and error once completed.
func (w *SessionWorker) Result() (string, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.result, w.err
}

// Start launches the headless ReAct loop in a dedicated goroutine.
func (w *SessionWorker) Start() {
	w.mu.Lock()
	w.active = true
	w.mu.Unlock()

	go func() {
		defer func() {
			if w.cleanup != nil {
				w.cleanup()
			}
			w.mu.Lock()
			w.engine = nil
			w.mu.Unlock()
			if w.onComplete != nil {
				w.onComplete()
			}
		}()

		res, err := w.engine.Run(w.ctx, w.task)

		w.mu.Lock()
		w.active = false
		w.result = res
		w.err = err
		w.mu.Unlock()

		// Emit final event
		finalType := agent.EventFinished
		finalContent := res
		if err != nil {
			finalType = agent.EventError
			finalContent = err.Error()
		}
		w.EmitTerminalEvent(agent.Event{
			Type:      finalType,
			Content:   finalContent,
			Timestamp: time.Now(),
		})
	}()
}

// maxNotificationFieldBytes defines the maximum byte size for individual large string fields
// in event notifications to ensure the total JSON-RPC frame stays well within DefaultMaxMessageSize.
const maxNotificationFieldBytes = 256 * 1024

func truncateForEvent(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	const suffix = "\n... [output truncated for streaming]"
	if maxBytes <= len(suffix) {
		return s[:utf8SafePrefix(s, maxBytes)]
	}
	return s[:utf8SafePrefix(s, maxBytes-len(suffix))] + suffix
}

func utf8SafePrefix(s string, maxBytes int) int {
	if maxBytes > len(s) {
		maxBytes = len(s)
	}
	for maxBytes > 0 && maxBytes < len(s) && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return maxBytes
}

func createEventNotification(sessionID string, eventID uint64, evt agent.Event, terminal bool) (*protocol.JSONRPCMessage, error) {
	content := evt.Content
	if content == "" && evt.ToolResult != "" {
		if len(evt.ToolResult) > 1024 {
			content = evt.ToolResult[:utf8SafePrefix(evt.ToolResult, 1024)] + "..."
		} else {
			content = evt.ToolResult
		}
	}
	if content == "" && evt.Error != nil {
		content = evt.Error.Error()
	}
	if len(content) > maxNotificationFieldBytes {
		content = truncateForEvent(content, maxNotificationFieldBytes)
	}

	toolResult := evt.ToolResult
	if len(toolResult) > maxNotificationFieldBytes {
		toolResult = truncateForEvent(toolResult, maxNotificationFieldBytes)
	}

	data := map[string]any{
		"step":         evt.Step,
		"tool_name":    evt.ToolName,
		"tool_call_id": evt.ToolCallID,
		"exit_code":    evt.ToolExitCode,
		"tool_result":  toolResult,
		"terminal":     terminal,
	}
	if evt.Error != nil {
		data["error"] = truncateForEvent(evt.Error.Error(), maxNotificationFieldBytes)
	}

	return protocol.NewNotification(protocol.NotifySessionEvent, protocol.SessionEventNotification{
		SessionID: sessionID,
		EventID:   eventID,
		Type:      string(evt.Type),
		Content:   content,
		Data:      data,
	})
}

// EmitEvent appends an event to the ring buffer and broadcasts it to all subscribers.
func (w *SessionWorker) EmitEvent(evt agent.Event) {
	w.emitEvent(evt, false)
}

// EmitTerminalEvent appends a terminal completion or error event to the ring buffer and broadcasts it.
func (w *SessionWorker) EmitTerminalEvent(evt agent.Event) {
	w.emitEvent(evt, true)
}

func (w *SessionWorker) emitEvent(evt agent.Event, terminal bool) {
	eventID := w.ringBuffer.PushWithTerminal(evt, terminal)

	notify, err := createEventNotification(w.sessionID, eventID, evt, terminal)
	if err != nil {
		return
	}

	w.mu.RLock()
	subs := make([]ClientConn, 0, len(w.subscribers))
	for conn := range w.subscribers {
		subs = append(subs, conn)
	}
	w.mu.RUnlock()

	for _, conn := range subs {
		if sendErr := conn.Send(notify); sendErr != nil {
			w.Detach(conn)
			_ = conn.Close()
		}
	}
}

// Attach registers a client connection, replays missed events, and returns assigned role.
func (w *SessionWorker) Attach(conn ClientConn, lastEventID uint64, takeControl bool) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	role := "observer"
	if takeControl || w.controller == nil {
		w.controller = conn
		role = "controller"
	}

	// Replay buffered events before registering to live subscribers,
	// holding mutex so no new live events can overtake replay history
	missed := w.ringBuffer.ReplayAfter(lastEventID)
	for _, item := range missed {
		notify, err := createEventNotification(w.sessionID, item.EventID, item.Event, item.Terminal)
		if err != nil {
			continue
		}
		if err := conn.SendWithTimeout(notify, 1*time.Second); err != nil {
			_ = conn.Close()
			return role, err
		}
	}

	// Register to live subscribers only AFTER all historical events have been enqueued
	w.subscribers[conn] = struct{}{}

	return role, nil
}

// Detach removes a client connection from active subscribers without stopping execution.
func (w *SessionWorker) Detach(conn ClientConn) {
	w.mu.Lock()
	defer w.mu.Unlock()

	delete(w.subscribers, conn)
	if w.controller == conn {
		w.controller = nil
	}
}

// Cancel immediately triggers context cancellation for the running engine.
func (w *SessionWorker) Cancel() {
	w.cancelFunc()
}

// IsController reports whether the given client is the current primary controller of the session.
func (w *SessionWorker) IsController(conn ClientConn) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.controller == conn
}

// RequestPermission requests human-in-the-loop approval from the primary controller.
func (w *SessionWorker) RequestPermission(tool string, args map[string]any) (bool, error) {
	reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())

	w.pendingMu.Lock()
	w.pendingRequestID = reqID
	w.pendingMu.Unlock()

	w.mu.RLock()
	ctrl := w.controller
	w.mu.RUnlock()

	if ctrl != nil {
		notify, err := protocol.NewNotification(protocol.NotifyPermissionRequired, protocol.PermissionRequestNotification{
			RequestID: reqID,
			SessionID: w.sessionID,
			Tool:      tool,
			Arguments: args,
		})
		if err == nil {
			_ = ctrl.Send(notify)
		}
	}

	select {
	case decision := <-w.approvalChan:
		return decision == "allow", nil
	case <-w.ctx.Done():
		return false, w.ctx.Err()
	}
}

// RespondPermission fulfills a pending human-in-the-loop permission request.
func (w *SessionWorker) RespondPermission(requestID, decision string) error {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()

	if w.pendingRequestID == "" || w.pendingRequestID != requestID {
		return fmt.Errorf("no pending permission request matching %s", requestID)
	}
	w.pendingRequestID = ""

	select {
	case w.approvalChan <- decision:
		return nil
	default:
		return fmt.Errorf("permission approval channel busy")
	}
}
