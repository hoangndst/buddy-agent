package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/daemon/security"
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
	id         string
	conn       net.Conn
	codec      *protocol.StreamCodec
	outbound   chan *protocol.JSONRPCMessage
	server     *Server
	activeSess map[string]*SessionWorker
	sessMu     sync.Mutex
	closed     atomic.Bool
}

func (c *clientConnection) ID() string {
	return c.id
}

func (c *clientConnection) Send(msg *protocol.JSONRPCMessage) error {
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

func (c *clientConnection) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		close(c.outbound)
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
		id:         generateClientID(),
		conn:       conn,
		codec:      protocol.NewStreamCodec(conn, conn),
		outbound:   make(chan *protocol.JSONRPCMessage, 256),
		server:     s,
		activeSess: make(map[string]*SessionWorker),
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
			if err == io.EOF {
				return
			}
			continue
		}

		s.dispatchMessage(client, msg)
	}
}

func (s *Server) dispatchMessage(client *clientConnection, msg *protocol.JSONRPCMessage) {
	switch msg.Method {
	case protocol.MethodDaemonPing:
		res, _ := protocol.NewResponse(*msg.ID, map[string]string{"status": "pong"})
		_ = client.Send(res)

	case protocol.MethodDaemonStatus:
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

	case protocol.MethodDaemonStop:
		res, _ := protocol.NewResponse(*msg.ID, map[string]string{"status": "shutting_down"})
		_ = client.Send(res)
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = s.Stop()
		}()

	case protocol.MethodSessionStart:
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

	case protocol.MethodSessionAttach:
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

		if err := s.sessionMgr.CancelSession(params.SessionID); err != nil {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeSessionNotFound, err.Error(), nil)
			_ = client.Send(errRes)
			return
		}

		if msg.ID != nil {
			res, _ := protocol.NewResponse(*msg.ID, map[string]string{"status": "canceled"})
			_ = client.Send(res)
		}

	case protocol.MethodPermissionRespond:
		var params protocol.PermissionRespondParams
		if err := json.Unmarshal(msg.Params, &params); err != nil || params.RequestID == "" {
			errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInvalidParams, "invalid permission.respond params", nil)
			_ = client.Send(errRes)
			return
		}

		client.sessMu.Lock()
		var foundWorker *SessionWorker
		for _, w := range client.activeSess {
			foundWorker = w
			break
		}
		client.sessMu.Unlock()

		if foundWorker != nil {
			if err := foundWorker.RespondPermission(params.RequestID, params.Decision); err != nil {
				errRes := protocol.NewErrorResponse(msg.ID, protocol.ErrCodeInternalError, err.Error(), nil)
				_ = client.Send(errRes)
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

// Stop initiates a disciplined two-phase graceful shutdown:
// Phase 1: Notify clients and close socket listener.
// Phase 2: Drain active sessions up to 5 seconds and release file locks.
func (s *Server) Stop() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

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
