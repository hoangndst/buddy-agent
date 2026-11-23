package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/daemon/security"
	"github.com/hoangndst/buddy-agent/internal/persistence"
)

// Server coordinates the Unix Domain Socket listener, client connection pumps,
// JSON-RPC dispatch, and graceful shutdown draining.
type Server struct {
	socketMgr  *SocketManager
	sessionMgr *SessionManager
	startTime  time.Time
	clientsMu  sync.RWMutex
	clients    map[string]*clientConnection
	shutdownCh chan struct{}
	closed     atomic.Bool
	ctx        context.Context
	cancel     context.CancelFunc
}

type clientConnection struct {
	id                  string
	conn                net.Conn
	codec               *protocol.StreamCodec
	outbound            chan *protocol.JSONRPCMessage
	outboundMu          sync.RWMutex
	server              *Server
	activeSess          map[string]*SessionWorker
	activeConversations map[string]*ConversationWorker
	sessMu              sync.Mutex
	closed              atomic.Bool
}

func (c *clientConnection) ID() string {
	return c.id
}

func (c *clientConnection) Send(msg *protocol.JSONRPCMessage) error {
	c.outboundMu.RLock()
	defer c.outboundMu.RUnlock()

	if c.closed.Load() {
		return fmt.Errorf("connection closed")
	}
	select {
	case c.outbound <- msg:
		return nil
	default:
		return fmt.Errorf("client outbound buffer full; dropping frame")
	}
}

func (c *clientConnection) SendWithTimeout(msg *protocol.JSONRPCMessage, timeout time.Duration) error {
	c.outboundMu.RLock()
	defer c.outboundMu.RUnlock()

	if c.closed.Load() {
		return fmt.Errorf("connection closed")
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case c.outbound <- msg:
		return nil
	case <-timer.C:
		return fmt.Errorf("client outbound buffer timed out after %v", timeout)
	}
}

func (c *clientConnection) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.outboundMu.Lock()
		close(c.outbound)
		c.outboundMu.Unlock()
		return c.conn.Close()
	}
	return nil
}

// NewServer creates a new Server instance.
func NewServer(sockMgr *SocketManager, sessionMgr *SessionManager) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		socketMgr:  sockMgr,
		sessionMgr: sessionMgr,
		startTime:  time.Now(),
		clients:    make(map[string]*clientConnection),
		shutdownCh: make(chan struct{}),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Start acquires the socket lock, binds the listener, and starts the accept loop.
func (s *Server) Start() error {
	listener, err := s.socketMgr.AcquireLockAndBind()
	if err != nil {
		return err
	}

	go s.acceptLoop(listener)
	return nil
}

func (s *Server) acceptLoop(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if s.closed.Load() {
				return
			}
			continue
		}

		// Verify peer credentials at kernel socket boundary
		if credErr := security.VerifyPeerCredentials(conn); credErr != nil {
			_ = conn.Close()
			continue
		}

		go s.handleConnection(conn)
	}
}

func generateClientID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleConnection(conn net.Conn) {
	client := &clientConnection{
		id:                  generateClientID(),
		conn:                conn,
		codec:               protocol.NewStreamCodec(conn, conn),
		outbound:            make(chan *protocol.JSONRPCMessage, 1024),
		server:              s,
		activeSess:          make(map[string]*SessionWorker),
		activeConversations: make(map[string]*ConversationWorker),
	}

	s.clientsMu.Lock()
	s.clients[client.id] = client
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, client.id)
		s.clientsMu.Unlock()

		// Detach from all active sessions
		client.sessMu.Lock()
		for _, worker := range client.activeSess {
			worker.Detach(client)
		}
		for _, worker := range client.activeConversations {
			worker.Detach(client)
		}
		client.sessMu.Unlock()

		_ = client.Close()
	}()

	// Dedicated outbound writer goroutine
	go func() {
		for msg := range client.outbound {
			if writeErr := client.codec.WriteMessage(msg); writeErr != nil {
				_ = client.Close()
				return
			}
		}
	}()

	// Inbound message pump
	for {
		msg, err := client.codec.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			var streamErr *protocol.StreamReadError
			if errors.As(err, &streamErr) {
				return
			}
			// Malformed JSON on an otherwise intact scanned line: skip and continue
			continue
		}

		s.dispatchMessage(client, msg)
	}
}

func (s *Server) dispatchMessage(client *clientConnection, msg *protocol.JSONRPCMessage) {
	switch msg.Method {
	case protocol.MethodDaemonPing:
		if msg.ID != nil {
			res, _ := protocol.NewResponse(*msg.ID, map[string]string{"status": "pong"})
			_ = client.Send(res)
		}

	case protocol.MethodDaemonStatus:
		if msg.ID != nil {
			active := s.sessionMgr.ListActiveSessions()
			uptime := int64(time.Since(s.startTime).Seconds())
			statusRes := protocol.DaemonStatusResult{
				PID:            os.Getpid(),
				UptimeSeconds:  uptime,
				SocketPath:     s.socketMgr.SocketPath(),
				ActiveSessions: active,
			}
			res, _ := protocol.NewResponse(*msg.ID, statusRes)
			_ = client.Send(res)
		}

	case protocol.MethodDaemonCapabilities:
		if msg.ID != nil {
			res, _ := protocol.NewResponse(*msg.ID, protocol.DaemonCapabilitiesResult{ProtocolVersion: 1, Methods: []string{
				protocol.MethodSessionStart, protocol.MethodSessionAttach, protocol.MethodSessionDetach,
				protocol.MethodSessionCancel, protocol.MethodPermissionRespond, protocol.MethodDaemonPing,
				protocol.MethodDaemonStatus, protocol.MethodConversationStart, protocol.MethodConversationResume,
				protocol.MethodConversationSubmit, protocol.MethodConversationSync, protocol.MethodConversationWorkspaces,
				protocol.MethodConversationList, protocol.MethodTurnInterrupt,
			}})
			_ = client.Send(res)
		}

	case protocol.MethodDaemonStop:
		if msg.ID != nil {
			res, _ := protocol.NewResponse(*msg.ID, map[string]string{"status": "shutting_down"})
			_ = client.Send(res)
		}
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = s.Stop()
		}()

	case protocol.MethodSessionStart:
		if msg.ID == nil {
			return
		}
		var params protocol.SessionStartParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid session.start params", err.Error())
			_ = client.Send(errRes)
			return
		}

		worker, err := s.sessionMgr.StartSession(s.ctx, params, client)
		if err != nil {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInternalError, "failed to start session", err.Error())
			_ = client.Send(errRes)
			return
		}

		client.sessMu.Lock()
		client.activeSess[worker.SessionID()] = worker
		client.sessMu.Unlock()

		res, _ := protocol.NewResponse(*msg.ID, protocol.SessionStartResult{
			SessionID: worker.SessionID(),
		})
		_ = client.Send(res)

	case protocol.MethodConversationStart:
		if msg.ID == nil {
			return
		}
		var params protocol.ConversationStartParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid conversation.start params", err.Error()))
			return
		}
		worker, result, err := s.sessionMgr.StartConversation(s.ctx, params, client)
		if err != nil {
			code := protocol.ErrCodeInternalError
			if errors.Is(err, persistence.ErrSubmissionConflict) {
				code = protocol.ErrCodeConflict
			}
			if params.StartID == "" || params.SubmissionID == "" || params.Prompt == "" {
				code = protocol.ErrCodeInvalidParams
			}
			_ = client.Send(protocol.NewErrorResponse(msg.ID, code, "failed to start conversation", err.Error()))
			return
		}
		client.sessMu.Lock()
		client.activeConversations[worker.ConversationID()] = worker
		client.sessMu.Unlock()
		res, _ := protocol.NewResponse(*msg.ID, result)
		_ = client.Send(res)

	case protocol.MethodConversationResume:
		if msg.ID == nil {
			return
		}
		var params protocol.ConversationResumeParams
		if err := json.Unmarshal(msg.Params, &params); err != nil || params.WorkspaceDir == "" {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid conversation.resume params", nil))
			return
		}
		worker, record, err := s.sessionMgr.ResumeConversation(s.ctx, params, client)
		if err != nil {
			code := protocol.ErrCodeInternalError
			if errors.Is(err, persistence.ErrSessionNotFound) {
				code = protocol.ErrCodeSessionNotFound
			}
			_ = client.Send(protocol.NewErrorResponse(msg.ID, code, "failed to resume conversation", err.Error()))
			return
		}
		client.sessMu.Lock()
		client.activeConversations[worker.ConversationID()] = worker
		client.sessMu.Unlock()
		syncResult, _, syncErr := worker.Sync(s.ctx, protocol.ConversationSyncParams{ConversationID: worker.ConversationID()}, client)
		if syncErr != nil {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInternalError, "failed to sync resumed conversation", syncErr.Error()))
			return
		}
		result := protocol.ConversationResumeResult{
			ConversationID: worker.ConversationID(), SessionID: worker.ConversationID(),
			WorkspaceDir: record.WorkingDir, Task: record.Task, HistoryRevision: syncResult.HistoryRevision,
			State: worker.State(), Role: syncResult.Role,
		}
		res, _ := protocol.NewResponse(*msg.ID, result)
		_ = client.Send(res)

	case protocol.MethodConversationSubmit:
		if msg.ID == nil {
			return
		}
		var params protocol.ConversationSubmitParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid conversation.submit params", err.Error()))
			return
		}
		worker, ok := s.sessionMgr.GetConversation(params.ConversationID)
		if !ok {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeSessionNotFound, fmt.Sprintf("conversation %s not found", params.ConversationID), nil))
			return
		}
		if !worker.IsController(client) {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodePermissionDenied, "only the conversation controller may submit prompts", nil))
			return
		}
		record, existing, err := worker.Submit(s.ctx, &persistence.SubmissionRecord{
			ConversationID: params.ConversationID, SubmissionID: params.SubmissionID,
			Prompt: params.Prompt, Mode: persistence.SubmissionMode(params.Mode),
		})
		if err != nil {
			code := protocol.ErrCodeInvalidParams
			if errors.Is(err, persistence.ErrSubmissionConflict) {
				code = protocol.ErrCodeConflict
			}
			_ = client.Send(protocol.NewErrorResponse(msg.ID, code, "failed to submit conversation prompt", err.Error()))
			return
		}
		turnID := ""
		if record.TurnID != nil {
			turnID = *record.TurnID
		}
		res, _ := protocol.NewResponse(*msg.ID, protocol.ConversationSubmitResult{
			ConversationID: params.ConversationID, SubmissionID: record.SubmissionID,
			Sequence: record.Sequence, Status: string(record.Status), TurnID: turnID, Existing: existing,
		})
		_ = client.Send(res)

	case protocol.MethodConversationSync:
		if msg.ID == nil {
			return
		}
		var params protocol.ConversationSyncParams
		if err := json.Unmarshal(msg.Params, &params); err != nil || params.ConversationID == "" {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid conversation.sync params", nil))
			return
		}
		worker, ok := s.sessionMgr.GetConversation(params.ConversationID)
		if !ok {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeSessionNotFound, "conversation worker is not resident; resume the conversation first", nil))
			return
		}
		client.sessMu.Lock()
		client.activeConversations[params.ConversationID] = worker
		client.sessMu.Unlock()
		result, _, err := worker.Sync(s.ctx, params, client)
		if err != nil {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInternalError, "conversation sync failed", err.Error()))
			return
		}
		res, _ := protocol.NewResponse(*msg.ID, result)
		_ = client.Send(res)

	case protocol.MethodConversationWorkspaces:
		if msg.ID == nil {
			return
		}
		var params protocol.ConversationListWorkspacesParams
		if len(msg.Params) > 0 {
			if err := json.Unmarshal(msg.Params, &params); err != nil {
				_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid workspace list params", err.Error()))
				return
			}
		}
		workspaces, err := s.sessionMgr.ListConversationWorkspaces(s.ctx, params.Limit)
		if err != nil {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInternalError, "failed to list conversation workspaces", err.Error()))
			return
		}
		res, _ := protocol.NewResponse(*msg.ID, protocol.ConversationListWorkspacesResult{Workspaces: workspaces})
		_ = client.Send(res)

	case protocol.MethodConversationList:
		if msg.ID == nil {
			return
		}
		var params protocol.ConversationListParams
		if err := json.Unmarshal(msg.Params, &params); err != nil || params.WorkspaceDir == "" {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid conversation.list params", nil))
			return
		}
		conversations, err := s.sessionMgr.ListConversations(s.ctx, params.WorkspaceDir, params.Limit)
		if err != nil {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInternalError, "failed to list conversations", err.Error()))
			return
		}
		res, _ := protocol.NewResponse(*msg.ID, protocol.ConversationListResult{Conversations: conversations})
		_ = client.Send(res)

	case protocol.MethodTurnInterrupt:
		if msg.ID == nil {
			return
		}
		var params protocol.TurnInterruptParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid turn.interrupt params", err.Error()))
			return
		}
		worker, ok := s.sessionMgr.GetConversation(params.ConversationID)
		if !ok {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeSessionNotFound, "conversation not found", nil))
			return
		}
		if !worker.IsController(client) {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodePermissionDenied, "only the conversation controller may interrupt a turn", nil))
			return
		}
		if err := worker.Interrupt(params.TurnID); err != nil {
			_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "turn interrupt failed", err.Error()))
			return
		}
		res, _ := protocol.NewResponse(*msg.ID, protocol.TurnInterruptResult{ConversationID: params.ConversationID, TurnID: params.TurnID, Status: "interrupting"})
		_ = client.Send(res)

	case protocol.MethodSessionAttach:
		if msg.ID == nil {
			return
		}
		var params protocol.SessionAttachParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid session.attach params", err.Error())
			_ = client.Send(errRes)
			return
		}

		worker, ok := s.sessionMgr.GetSession(params.SessionID)
		if !ok {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeSessionNotFound, fmt.Sprintf("session %s not found", params.SessionID), nil)
			_ = client.Send(errRes)
			return
		}

		role, err := worker.Attach(client, params.LastEventID, params.TakeControl)
		if err != nil {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInternalError, "attach failed", err.Error())
			_ = client.Send(errRes)
			return
		}

		client.sessMu.Lock()
		client.activeSess[worker.SessionID()] = worker
		client.sessMu.Unlock()

		res, _ := protocol.NewResponse(*msg.ID, protocol.SessionAttachResult{
			SessionID: worker.SessionID(),
			Role:      role,
			Active:    worker.IsActive(),
		})
		_ = client.Send(res)

	case protocol.MethodSessionDetach:
		var params protocol.SessionCancelParams
		if err := json.Unmarshal(msg.Params, &params); err == nil && params.SessionID != "" {
			if worker, ok := s.sessionMgr.GetSession(params.SessionID); ok {
				worker.Detach(client)
				client.sessMu.Lock()
				delete(client.activeSess, params.SessionID)
				client.sessMu.Unlock()
			}
		}
		if msg.ID != nil {
			res, _ := protocol.NewResponse(*msg.ID, map[string]string{"status": "detached"})
			_ = client.Send(res)
		}

	case protocol.MethodSessionCancel:
		var params protocol.SessionCancelParams
		if err := json.Unmarshal(msg.Params, &params); err != nil || params.SessionID == "" {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid session.cancel params", nil)
			_ = client.Send(errRes)
			return
		}

		worker, ok := s.sessionMgr.GetSession(params.SessionID)
		if !ok {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeSessionNotFound, fmt.Sprintf("session %s not found", params.SessionID), nil)
			_ = client.Send(errRes)
			return
		}

		if !worker.IsController(client) {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodePermissionDenied, "permission denied: only the primary controller can cancel this session", nil)
			_ = client.Send(errRes)
			return
		}

		worker.Cancel()

		if msg.ID != nil {
			res, _ := protocol.NewResponse(*msg.ID, map[string]string{"status": "canceled"})
			_ = client.Send(res)
		}

	case protocol.MethodPermissionRespond:
		var params protocol.PermissionRespondParams
		if err := json.Unmarshal(msg.Params, &params); err != nil || params.RequestID == "" ||
			(params.Decision != protocol.DecisionAllowOnce && params.Decision != protocol.DecisionAllowSession && params.Decision != protocol.DecisionDeny) {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid permission.respond params", nil)
			_ = client.Send(errRes)
			return
		}

		if params.ConversationID != "" {
			client.sessMu.Lock()
			worker := client.activeConversations[params.ConversationID]
			client.sessMu.Unlock()
			if worker == nil {
				_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeSessionNotFound, "conversation is not attached to this client", nil))
				return
			}
			if !worker.IsController(client) {
				_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodePermissionDenied, "only the conversation controller may respond to permission requests", nil))
				return
			}
			if err := worker.RespondPermission(client, params); err != nil {
				_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeConflict, err.Error(), nil))
				return
			}
		} else {
			if params.Decision == protocol.DecisionAllowSession || params.SessionWhitelist || params.ModifiedArguments != nil || params.CommandPattern != "" {
				_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "structured approvals require a conversation session", nil))
				return
			}
			client.sessMu.Lock()
			workers := make([]*SessionWorker, 0, len(client.activeSess))
			for _, worker := range client.activeSess {
				workers = append(workers, worker)
			}
			client.sessMu.Unlock()
			responded := false
			for _, worker := range workers {
				if params.SessionID != "" && worker.sessionID != params.SessionID {
					continue
				}
				if !worker.IsController(client) {
					continue
				}
				decision := "deny"
				if params.Decision == protocol.DecisionAllowOnce {
					decision = "allow"
				}
				if err := worker.RespondPermission(params.RequestID, decision); err == nil {
					responded = true
					break
				}
			}
			if !responded {
				_ = client.Send(protocol.NewErrorResponse(msg.ID, protocol.ErrCodeConflict, "no controller-owned session has that pending permission request", nil))
				return
			}
		}

		if msg.ID != nil {
			res, _ := protocol.NewResponse(*msg.ID, map[string]string{"status": "responded"})
			_ = client.Send(res)
		}

	default:
		if msg.ID != nil {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeMethodNotFound, fmt.Sprintf("method %s not found", msg.Method), nil)
			_ = client.Send(errRes)
		}
	}
}

// Done returns a channel that is closed when the daemon server terminates.
func (s *Server) Done() <-chan struct{} {
	return s.shutdownCh
}

// Stop initiates a disciplined two-phase graceful shutdown:
// Phase 1: Notify clients and close socket listener.
// Phase 2: Drain active sessions up to 5 seconds and release file locks.
func (s *Server) Stop() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	defer close(s.shutdownCh)

	s.cancel()

	// Broadcast shutdown notice to all active clients
	shuttingDownNotify, _ := protocol.NewNotification(protocol.NotifyDaemonShuttingDown, map[string]string{
		"message": "daemon is shutting down",
	})
	s.clientsMu.Lock()
	for _, client := range s.clients {
		_ = client.Send(shuttingDownNotify)
		_ = client.Close()
	}
	s.clientsMu.Unlock()

	// Phase 1: Stop accepting connections and unlink socket
	_ = s.socketMgr.Close()

	// Phase 2: Drain active sessions (up to 5 seconds)
	s.sessionMgr.Drain(5 * time.Second)

	return nil
}
