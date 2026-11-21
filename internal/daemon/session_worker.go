package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
)

// ClientConn abstracts an active client connection.
type ClientConn interface {
	ID() string
	Send(msg *protocol.JSONRPCMessage) error
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
}

// NewSessionWorker constructs a new SessionWorker bound to a ReAct engine.
func NewSessionWorker(
	parentCtx context.Context,
	sessionID string,
	task string,
	engine *agent.Engine,
	controller ClientConn,
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
		w.EmitEvent(agent.Event{
			Type:      finalType,
			Content:   finalContent,
			Timestamp: time.Now(),
		})
	}()
}

// EmitEvent appends an event to the ring buffer and broadcasts it to all subscribers.
func (w *SessionWorker) EmitEvent(evt agent.Event) {
	eventID := w.ringBuffer.Push(evt)

	data := map[string]any{
		"step":         evt.Step,
		"tool_name":    evt.ToolName,
		"tool_call_id": evt.ToolCallID,
		"exit_code":    evt.ToolExitCode,
	}
	notify, err := protocol.NewNotification(protocol.NotifySessionEvent, protocol.SessionEventNotification{
		SessionID: w.sessionID,
		EventID:   eventID,
		Type:      string(evt.Type),
		Content:   evt.Content,
		Data:      data,
	})
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
		go func(c ClientConn) {
			if sendErr := c.Send(notify); sendErr != nil {
				w.Detach(c)
			}
		}(conn)
	}
}

// Attach registers a client connection, replays missed events, and returns assigned role.
func (w *SessionWorker) Attach(conn ClientConn, lastEventID uint64, takeControl bool) (string, error) {
	w.mu.Lock()
	role := "observer"
	if takeControl || w.controller == nil {
		w.controller = conn
		role = "controller"
	}
	w.subscribers[conn] = struct{}{}
	w.mu.Unlock()

	// Replay buffered events after lastEventID
	missed := w.ringBuffer.ReplayAfter(lastEventID)
	for _, item := range missed {
		data := map[string]any{
			"step":         item.Event.Step,
			"tool_name":    item.Event.ToolName,
			"tool_call_id": item.Event.ToolCallID,
			"exit_code":    item.Event.ToolExitCode,
		}
		notify, _ := protocol.NewNotification(protocol.NotifySessionEvent, protocol.SessionEventNotification{
			SessionID: w.sessionID,
			EventID:   item.EventID,
			Type:      string(item.Event.Type),
			Content:   item.Event.Content,
			Data:      data,
		})
		if err := conn.Send(notify); err != nil {
			w.Detach(conn)
			return role, err
		}
	}

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
