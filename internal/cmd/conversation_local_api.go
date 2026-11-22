package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	daemoncmd "github.com/hoangndst/buddy-agent/internal/cmd/daemon"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/daemon"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/persistence"
)

type localConversationAPI struct {
	ctx       context.Context
	manager   *daemon.SessionManager
	conn      *localClientConn
	evictions chan string
}

type localClientConn struct {
	mu          sync.Mutex
	closed      bool
	events      chan protocol.SessionEventNotification
	permissions chan protocol.PermissionRequestNotification
	evictions   chan string
}

func newLocalConversationAPI(ctx context.Context, f *cmdutil.Factory) (*localConversationAPI, error) {
	home, err := f.UserHome()
	if err != nil {
		return nil, err
	}
	catalog, err := persistence.NewWorkspaceCatalog(filepath.Join(home, ".buddy", "workspaces.db"))
	if err != nil {
		return nil, err
	}
	manager := daemon.NewSessionManager(func(sessionID string, params protocol.SessionStartParams, emit agent.EventHandler) (*agent.Engine, func(), error) {
		return daemoncmd.BuildEngineForSession(ctx, f, sessionID, params, emit)
	})
	manager.SetWorkspaceCatalog(catalog)
	conn := &localClientConn{events: make(chan protocol.SessionEventNotification, 2048), permissions: make(chan protocol.PermissionRequestNotification, 32), evictions: make(chan string, 16)}
	return &localConversationAPI{ctx: ctx, manager: manager, conn: conn, evictions: conn.evictions}, nil
}

func (a *localConversationAPI) Capabilities() (*protocol.DaemonCapabilitiesResult, error) {
	return &protocol.DaemonCapabilitiesResult{Methods: []string{
		protocol.MethodConversationStart, protocol.MethodConversationResume, protocol.MethodConversationSubmit,
		protocol.MethodConversationSync, protocol.MethodConversationList, protocol.MethodConversationWorkspaces,
		protocol.MethodTurnInterrupt, protocol.MethodPermissionRespond,
	}}, nil
}

func (a *localConversationAPI) StartConversation(params protocol.ConversationStartParams) (*protocol.ConversationStartResult, error) {
	_, result, err := a.manager.StartConversation(a.ctx, params, a.conn)
	return result, err
}

func (a *localConversationAPI) ResumeConversation(params protocol.ConversationResumeParams) (*protocol.ConversationResumeResult, error) {
	worker, record, err := a.manager.ResumeConversation(a.ctx, params, a.conn)
	if err != nil {
		return nil, err
	}
	syncResult, _, err := worker.Sync(a.ctx, protocol.ConversationSyncParams{ConversationID: worker.ConversationID()}, a.conn)
	if err != nil {
		return nil, err
	}
	return &protocol.ConversationResumeResult{
		ConversationID: worker.ConversationID(), SessionID: worker.ConversationID(), Role: syncResult.Role,
		WorkspaceDir: record.WorkingDir, Task: record.Task, HistoryRevision: syncResult.HistoryRevision, State: worker.State(),
	}, nil
}

func (a *localConversationAPI) SyncConversation(params protocol.ConversationSyncParams) (*protocol.ConversationSyncResult, error) {
	worker, ok := a.manager.GetConversation(params.ConversationID)
	if !ok {
		return nil, errors.New("conversation worker is not resident; resume the conversation first")
	}
	result, _, err := worker.Sync(a.ctx, params, a.conn)
	return result, err
}

func (a *localConversationAPI) ListConversationWorkspaces(limit int) (*protocol.ConversationListWorkspacesResult, error) {
	workspaces, err := a.manager.ListConversationWorkspaces(a.ctx, limit)
	if err != nil {
		return nil, err
	}
	return &protocol.ConversationListWorkspacesResult{Workspaces: workspaces}, nil
}

func (a *localConversationAPI) ListConversations(params protocol.ConversationListParams) (*protocol.ConversationListResult, error) {
	conversations, err := a.manager.ListConversations(a.ctx, params.WorkspaceDir, params.Limit)
	if err != nil {
		return nil, err
	}
	return &protocol.ConversationListResult{Conversations: conversations}, nil
}

func (a *localConversationAPI) SubmitConversation(params protocol.ConversationSubmitParams) (*protocol.ConversationSubmitResult, error) {
	worker, ok := a.manager.GetConversation(params.ConversationID)
	if !ok {
		return nil, persistence.ErrSessionNotFound
	}
	if !worker.IsController(a.conn) {
		return nil, errors.New("conversation is controlled by another client")
	}
	record, existing, err := worker.Submit(a.ctx, &persistence.SubmissionRecord{
		ConversationID: params.ConversationID, SubmissionID: params.SubmissionID,
		Prompt: params.Prompt, Mode: persistence.SubmissionMode(params.Mode),
	})
	if err != nil {
		return nil, err
	}
	turnID := ""
	if record.TurnID != nil {
		turnID = *record.TurnID
	}
	return &protocol.ConversationSubmitResult{
		ConversationID: params.ConversationID, SubmissionID: record.SubmissionID,
		Sequence: record.Sequence, Status: string(record.Status), TurnID: turnID, Existing: existing,
	}, nil
}

func (a *localConversationAPI) InterruptTurn(params protocol.TurnInterruptParams) (*protocol.TurnInterruptResult, error) {
	worker, ok := a.manager.GetConversation(params.ConversationID)
	if !ok {
		return nil, persistence.ErrSessionNotFound
	}
	if !worker.IsController(a.conn) {
		return nil, errors.New("conversation is controlled by another client")
	}
	if err := worker.Interrupt(params.TurnID); err != nil {
		return nil, err
	}
	return &protocol.TurnInterruptResult{ConversationID: params.ConversationID, TurnID: params.TurnID, Status: "interrupting"}, nil
}

func (a *localConversationAPI) RespondConversationPermission(conversationID, turnID, requestID, decision string) error {
	worker, ok := a.manager.GetConversation(conversationID)
	if !ok {
		return persistence.ErrSessionNotFound
	}
	if !worker.IsController(a.conn) {
		return errors.New("only the conversation controller may respond to permission requests")
	}
	return worker.RespondPermission(a.conn, turnID, requestID, decision)
}

func (a *localConversationAPI) ConversationEvents() <-chan protocol.SessionEventNotification {
	return a.conn.events
}
func (a *localConversationAPI) PermissionRequests() <-chan protocol.PermissionRequestNotification {
	return a.conn.permissions
}
func (a *localConversationAPI) ConversationEventGaps() <-chan struct{} { return nil }
func (a *localConversationAPI) ConversationEvictions() <-chan string   { return a.evictions }
func (a *localConversationAPI) ClearConversationEventGap()             {}

func (a *localConversationAPI) Close() error {
	a.conn.close()
	a.manager.Drain(time.Second)
	return nil
}

func (c *localClientConn) ID() string { return "standalone-local-client" }
func (c *localClientConn) Send(msg *protocol.JSONRPCMessage) error {
	return c.SendWithTimeout(msg, time.Second)
}
func (c *localClientConn) SendWithTimeout(msg *protocol.JSONRPCMessage, timeout time.Duration) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errors.New("local conversation client is closed")
	}
	if msg.Method == protocol.NotifyConversationEvicted {
		var notification struct {
			ConversationID string `json:"conversation_id"`
		}
		if err := json.Unmarshal(msg.Params, &notification); err != nil {
			return err
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case c.evictions <- notification.ConversationID:
			return nil
		case <-timer.C:
			return errors.New("local conversation eviction queue is full")
		}
	}
	if msg.Method == protocol.NotifyPermissionRequired {
		var request protocol.PermissionRequestNotification
		if err := json.Unmarshal(msg.Params, &request); err != nil {
			return err
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case c.permissions <- request:
			return nil
		case <-timer.C:
			return errors.New("local conversation permission queue is full")
		}
	}
	if msg.Method != protocol.NotifyConversationEvent {
		return nil
	}
	var event protocol.SessionEventNotification
	if err := json.Unmarshal(msg.Params, &event); err != nil {
		return err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case c.events <- event:
		return nil
	case <-timer.C:
		return errors.New("local conversation event queue is full")
	}
}
func (c *localClientConn) Close() error { c.close(); return nil }
func (c *localClientConn) close()       { c.mu.Lock(); c.closed = true; c.mu.Unlock() }
