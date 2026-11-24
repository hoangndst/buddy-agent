// Package client provides IPC client bindings for interacting with the buddy-agent daemon.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hoangndst/buddy-agent/internal/daemon"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
)

// Client coordinates communication with a local buddy-agent background daemon over Unix domain socket.
type Client struct {
	sockPath              string
	conn                  net.Conn
	codec                 *protocol.StreamCodec
	reqCounter            atomic.Uint64
	mu                    sync.Mutex
	pendingReqs           map[string]chan *protocol.JSONRPCMessage
	eventsCh              chan protocol.SessionEventNotification
	conversationEventsCh  chan protocol.SessionEventNotification
	conversationEventGap  atomic.Bool
	conversationEventGaps chan struct{}
	conversationEvictions chan string
	permissionCh          chan protocol.PermissionRequestNotification
	closed                atomic.Bool
}

// Connect attempts to establish a connection to an active daemon at sockPath.
func Connect(sockPath string) (*Client, error) {
	if sockPath == "" {
		sockPath = daemon.GetDefaultSocketPath()
	}

	conn, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to daemon at %s: %w", sockPath, err)
	}

	c := &Client{
		sockPath:              sockPath,
		conn:                  conn,
		codec:                 protocol.NewStreamCodec(conn, conn),
		pendingReqs:           make(map[string]chan *protocol.JSONRPCMessage),
		eventsCh:              make(chan protocol.SessionEventNotification, 2048),
		conversationEventsCh:  make(chan protocol.SessionEventNotification, 2048),
		conversationEventGaps: make(chan struct{}, 1),
		conversationEvictions: make(chan string, 16),
		permissionCh:          make(chan protocol.PermissionRequestNotification, 128),
	}

	go c.readPump()
	return c, nil
}

// ConnectOrSpawn attempts to connect to an existing daemon, or auto-spawns one in the background.
func ConnectOrSpawn(sockPath string) (*Client, error) {
	if sockPath == "" {
		sockPath = daemon.GetDefaultSocketPath()
	}

	// 1. Try immediate connect
	c, err := Connect(sockPath)
	if err == nil {
		return c, nil
	}

	// 2. Spawn daemon process in detached background mode
	execPath, err := os.Executable()
	if err != nil {
		execPath = os.Args[0]
	}

	cmdArgs := []string{"daemon", "start", "--detach"}
	if sockPath != "" {
		cmdArgs = append(cmdArgs, "--socket", sockPath)
	}
	cmd := exec.Command(execPath, cmdArgs...)
	SetDetachedProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to auto-spawn daemon: %w", err)
	}

	// 3. Poll socket with backoff up to 2.5 seconds
	deadline := time.Now().Add(2500 * time.Millisecond)
	sleepInterval := 10 * time.Millisecond

	for time.Now().Before(deadline) {
		time.Sleep(sleepInterval)
		c, err := Connect(sockPath)
		if err == nil {
			return c, nil
		}
		if sleepInterval < 100*time.Millisecond {
			sleepInterval *= 2
		}
	}

	return nil, fmt.Errorf("timeout waiting for auto-spawned daemon on %s", sockPath)
}

func (c *Client) readPump() {
	defer func() {
		c.closed.Store(true)
		c.mu.Lock()
		for id, ch := range c.pendingReqs {
			close(ch)
			delete(c.pendingReqs, id)
		}
		c.mu.Unlock()
		close(c.eventsCh)
		close(c.conversationEventsCh)
		close(c.conversationEventGaps)
		close(c.permissionCh)
	}()

	for {
		msg, err := c.codec.ReadMessage()
		if err != nil {
			return
		}

		if msg.ID != nil {
			// Response to pending request
			c.mu.Lock()
			ch, ok := c.pendingReqs[*msg.ID]
			if ok {
				delete(c.pendingReqs, *msg.ID)
			}
			c.mu.Unlock()

			if ok {
				ch <- msg
			}
			continue
		}

		// Server notification
		switch msg.Method {
		case protocol.NotifySessionEvent:
			var notify protocol.SessionEventNotification
			if err := json.Unmarshal(msg.Params, &notify); err == nil {
				select {
				case c.eventsCh <- notify:
				default:
					// Evict oldest unconsumed event to make room for newest events (especially terminal completion)
					select {
					case <-c.eventsCh:
					default:
					}
					select {
					case c.eventsCh <- notify:
					default:
					}
				}
			}
		case protocol.NotifyConversationEvent:
			var notify protocol.SessionEventNotification
			if err := json.Unmarshal(msg.Params, &notify); err == nil {
				select {
				case c.conversationEventsCh <- notify:
				default:
					// Signal loss explicitly so the consumer can call conversation.sync.
					c.conversationEventGap.Store(true)
					select {
					case c.conversationEventGaps <- struct{}{}:
					default:
					}
					select {
					case <-c.conversationEventsCh:
					default:
					}
					select {
					case c.conversationEventsCh <- notify:
					default:
					}
				}
			}
		case protocol.NotifyConversationEvicted:
			var notify struct {
				ConversationID string `json:"conversation_id"`
			}
			if json.Unmarshal(msg.Params, &notify) == nil && notify.ConversationID != "" {
				select {
				case c.conversationEvictions <- notify.ConversationID:
				default:
				}
			}
		case protocol.NotifyPermissionRequired:
			var req protocol.PermissionRequestNotification
			if err := json.Unmarshal(msg.Params, &req); err == nil {
				select {
				case c.permissionCh <- req:
				default:
				}
			}
		}
	}
}

func (c *Client) cleanup() {
	if c.closed.CompareAndSwap(false, true) {
		_ = c.conn.Close()
	}
}

func (c *Client) call(method string, params any) (*protocol.JSONRPCMessage, error) {
	if c.closed.Load() {
		return nil, errors.New("client connection closed")
	}

	id := fmt.Sprintf("c_%d", c.reqCounter.Add(1))
	req, err := protocol.NewRequest(id, method, params)
	if err != nil {
		return nil, err
	}

	respCh := make(chan *protocol.JSONRPCMessage, 1)
	c.mu.Lock()
	c.pendingReqs[id] = respCh
	c.mu.Unlock()

	if err := c.codec.WriteMessage(req); err != nil {
		c.mu.Lock()
		delete(c.pendingReqs, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("failed to send request %s: %w", method, err)
	}

	select {
	case resp, ok := <-respCh:
		if !ok {
			return nil, errors.New("connection closed while awaiting response")
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp, nil
	case <-time.After(30 * time.Second):
		c.mu.Lock()
		delete(c.pendingReqs, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("timeout waiting for response to %s", method)
	}
}

// Ping checks daemon liveness.
func (c *Client) Ping() error {
	_, err := c.call(protocol.MethodDaemonPing, nil)
	return err
}

// Status returns current daemon status metrics.
func (c *Client) Status() (*protocol.DaemonStatusResult, error) {
	resp, err := c.call(protocol.MethodDaemonStatus, nil)
	if err != nil {
		return nil, err
	}
	var res protocol.DaemonStatusResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("failed to unmarshal status result: %w", err)
	}
	return &res, nil
}

// StopDaemon instructs the daemon to initiate graceful shutdown.
func (c *Client) StopDaemon() error {
	_, err := c.call(protocol.MethodDaemonStop, nil)
	return err
}

// StartSession initiates a new agent task.
func (c *Client) StartSession(params protocol.SessionStartParams) (string, error) {
	resp, err := c.call(protocol.MethodSessionStart, params)
	if err != nil {
		return "", err
	}
	var res protocol.SessionStartResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return "", fmt.Errorf("failed to unmarshal session.start result: %w", err)
	}
	return res.SessionID, nil
}

// StartConversation creates a durable interactive conversation.
func (c *Client) StartConversation(params protocol.ConversationStartParams) (*protocol.ConversationStartResult, error) {
	resp, err := c.call(protocol.MethodConversationStart, params)
	if err != nil {
		return nil, err
	}
	var result protocol.ConversationStartResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation.start result: %w", err)
	}
	return &result, nil
}

// ResumeConversation attaches to a conversation and returns its current state.
func (c *Client) ResumeConversation(params protocol.ConversationResumeParams) (*protocol.ConversationResumeResult, error) {
	resp, err := c.call(protocol.MethodConversationResume, params)
	if err != nil {
		return nil, err
	}
	var result protocol.ConversationResumeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation.resume result: %w", err)
	}
	return &result, nil
}

// SyncConversation reads a durable transcript page and synchronization watermark.
func (c *Client) SyncConversation(params protocol.ConversationSyncParams) (*protocol.ConversationSyncResult, error) {
	resp, err := c.call(protocol.MethodConversationSync, params)
	if err != nil {
		return nil, err
	}
	var result protocol.ConversationSyncResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation.sync result: %w", err)
	}
	return &result, nil
}

// Capabilities returns the daemon protocol version and supported methods.
func (c *Client) Capabilities() (*protocol.DaemonCapabilitiesResult, error) {
	resp, err := c.call(protocol.MethodDaemonCapabilities, nil)
	if err != nil {
		return nil, err
	}
	var result protocol.DaemonCapabilitiesResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal daemon capabilities: %w", err)
	}
	return &result, nil
}

// ListConversationWorkspaces returns workspaces with stored conversations.
func (c *Client) ListConversationWorkspaces(limit int) (*protocol.ConversationListWorkspacesResult, error) {
	resp, err := c.call(protocol.MethodConversationWorkspaces, protocol.ConversationListWorkspacesParams{Limit: limit})
	if err != nil {
		return nil, err
	}
	var result protocol.ConversationListWorkspacesResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal workspace list: %w", err)
	}
	return &result, nil
}

// ListConversations returns conversations stored for a workspace.
func (c *Client) ListConversations(params protocol.ConversationListParams) (*protocol.ConversationListResult, error) {
	resp, err := c.call(protocol.MethodConversationList, params)
	if err != nil {
		return nil, err
	}
	var result protocol.ConversationListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation list: %w", err)
	}
	return &result, nil
}

// SubmitConversation durably queues a prompt in an existing conversation.
func (c *Client) SubmitConversation(params protocol.ConversationSubmitParams) (*protocol.ConversationSubmitResult, error) {
	resp, err := c.call(protocol.MethodConversationSubmit, params)
	if err != nil {
		return nil, err
	}
	var result protocol.ConversationSubmitResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation.submit result: %w", err)
	}
	return &result, nil
}

// InterruptTurn cancels only the specified active conversation turn.
func (c *Client) InterruptTurn(params protocol.TurnInterruptParams) (*protocol.TurnInterruptResult, error) {
	resp, err := c.call(protocol.MethodTurnInterrupt, params)
	if err != nil {
		return nil, err
	}
	var result protocol.TurnInterruptResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal turn.interrupt result: %w", err)
	}
	return &result, nil
}

// AttachSession attaches to an existing session and replays missed events.
func (c *Client) AttachSession(params protocol.SessionAttachParams) (*protocol.SessionAttachResult, error) {
	resp, err := c.call(protocol.MethodSessionAttach, params)
	if err != nil {
		return nil, err
	}
	var res protocol.SessionAttachResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session.attach result: %w", err)
	}
	return &res, nil
}

// DetachSession detaches client from session without terminating task.
func (c *Client) DetachSession(sessionID string) error {
	_, err := c.call(protocol.MethodSessionDetach, protocol.SessionCancelParams{SessionID: sessionID})
	return err
}

// CancelSession requests immediate cancellation of an active session.
func (c *Client) CancelSession(sessionID string) error {
	_, err := c.call(protocol.MethodSessionCancel, protocol.SessionCancelParams{SessionID: sessionID})
	return err
}

// RespondPermission fulfills a human-in-the-loop tool approval request.
func (c *Client) RespondPermission(requestID string, decision string) error {
	_, err := c.call(protocol.MethodPermissionRespond, protocol.PermissionRespondParams{
		RequestID: requestID,
		Decision:  protocol.PermissionDecision(decision),
	})
	return err
}

// RespondConversationPermission answers a request bound to one conversation and turn.
func (c *Client) RespondConversationPermission(conversationID, turnID, requestID, decision string) error {
	return c.RespondConversationPermissionParams(protocol.PermissionRespondParams{
		ConversationID: conversationID, TurnID: turnID, RequestID: requestID, Decision: protocol.PermissionDecision(decision),
	})
}

// RespondConversationPermissionParams sends a structured decision for one conversation approval.
func (c *Client) RespondConversationPermissionParams(params protocol.PermissionRespondParams) error {
	_, err := c.call(protocol.MethodPermissionRespond, protocol.PermissionRespondParams{
		ConversationID: params.ConversationID, TurnID: params.TurnID, RequestID: params.RequestID, Decision: params.Decision,
		SessionWhitelist: params.SessionWhitelist, CommandPattern: params.CommandPattern,
		ModifiedArguments: params.ModifiedArguments, DenialReason: params.DenialReason,
	})
	return err
}

// Events returns the channel receiving live session event notifications.
func (c *Client) Events() <-chan protocol.SessionEventNotification {
	return c.eventsCh
}

// ConversationEvents returns the channel receiving conversation turn events.
func (c *Client) ConversationEvents() <-chan protocol.SessionEventNotification {
	return c.conversationEventsCh
}

// ConversationEventGaps signals when streamed conversation events have a gap.
func (c *Client) ConversationEventGaps() <-chan struct{} { return c.conversationEventGaps }

// HasConversationEventGap reports whether a streamed event gap is pending.
func (c *Client) HasConversationEventGap() bool { return c.conversationEventGap.Load() }

// ClearConversationEventGap clears the pending event gap marker.
func (c *Client) ClearConversationEventGap() { c.conversationEventGap.Store(false) }

// ConversationEvictions signals that a resident conversation worker was evicted.
func (c *Client) ConversationEvictions() <-chan string { return c.conversationEvictions }

// Permissions returns the channel receiving interactive tool permission requests.
func (c *Client) Permissions() <-chan protocol.PermissionRequestNotification {
	return c.permissionCh
}

// PermissionRequests is an alias used by the conversation UI API.
func (c *Client) PermissionRequests() <-chan protocol.PermissionRequestNotification {
	return c.permissionCh
}

// Close closes the client connection.
func (c *Client) Close() error {
	c.cleanup()
	return nil
}
