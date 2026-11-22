package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tools"
)

type permissionTestConn struct {
	id       string
	messages chan *protocol.JSONRPCMessage
}

func (c *permissionTestConn) ID() string { return c.id }
func (c *permissionTestConn) Send(msg *protocol.JSONRPCMessage) error {
	c.messages <- msg
	return nil
}
func (c *permissionTestConn) SendWithTimeout(msg *protocol.JSONRPCMessage, _ time.Duration) error {
	return c.Send(msg)
}
func (c *permissionTestConn) Close() error { return nil }

type steeringBoundaryProvider struct {
	firstCall      chan struct{}
	continueCh     chan struct{}
	second         chan provider.GenerateRequest
	toolAfterFirst bool
	mu             sync.Mutex
	calls          int
}

func (p *steeringBoundaryProvider) Protocol() string { return "test" }

func (p *steeringBoundaryProvider) Generate(ctx context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	switch call {
	case 1:
		close(p.firstCall)
		select {
		case <-p.continueCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if p.toolAfterFirst {
			return &provider.GenerateResponse{Protocol: "test", ToolCalls: []provider.ToolCall{{ID: "missing-tool-call", Name: "missing_tool", Arguments: `{}`}}}, nil
		}
		return &provider.GenerateResponse{Protocol: "test", Content: "done"}, nil
	case 2:
		p.second <- req
		return &provider.GenerateResponse{Protocol: "test", Content: "done"}, nil
	default:
		return &provider.GenerateResponse{Protocol: "test", Content: "done"}, nil
	}
}

func TestConversationWorkerAppliesSteerAtNextModelBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	firstPrompt, firstTurnID := "first request", "first-turn"
	fingerprint := persistence.SubmissionFingerprint(firstPrompt, persistence.SubmissionQueue)
	started, err := store.CreateConversation(ctx, &persistence.ConversationStartRequest{
		Session: persistence.SessionRecord{ID: "steering-boundary-test", WorkingDir: workspace, Model: "test-model"},
		StartID: "stable-start", PayloadFingerprint: fingerprint,
		FirstSubmission: persistence.SubmissionRecord{
			SubmissionID: "first-submission", Prompt: firstPrompt, Mode: persistence.SubmissionQueue,
			PayloadFingerprint: fingerprint, TurnID: &firstTurnID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &steeringBoundaryProvider{firstCall: make(chan struct{}), continueCh: make(chan struct{}), second: make(chan provider.GenerateRequest, 1), toolAfterFirst: true}
	engine := agent.NewEngine(provider, "test-model", 5, tools.NewRegistry(1000), nil, nil,
		agent.WithStore(store), agent.WithWorkspaceDir(workspace), agent.WithSessionIDOverride(started.ConversationID), agent.WithSystemPromptOverride("system"))
	historySession, err := engine.NewHistorySession(firstPrompt)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewConversationRuntime(ctx, engine, store, started.ConversationID, started.SubmissionID, historySession)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewConversationWorker(ctx, started.ConversationID, engine, runtime, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(&persistence.SubmissionRecord{ConversationID: started.ConversationID, SubmissionID: started.SubmissionID, TurnID: &firstTurnID}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)
	select {
	case <-provider.firstCall:
	case <-time.After(2 * time.Second):
		t.Fatal("first provider request did not start")
	}
	steer, alreadyExists, err := worker.Submit(ctx, &persistence.SubmissionRecord{
		ConversationID: started.ConversationID, SubmissionID: "steer-submission", Prompt: "use the alternate approach", Mode: persistence.SubmissionSteer,
	})
	if err != nil {
		t.Fatalf("submit steer: %v", err)
	}
	if alreadyExists || steer.TurnID != nil || steer.Status != persistence.SubmissionSteerPending {
		t.Fatalf("steer should remain unattached until the model boundary: %+v existing=%v", steer, alreadyExists)
	}
	close(provider.continueCh)
	select {
	case secondRequest := <-provider.second:
		found := false
		for _, message := range secondRequest.Messages {
			if message.OfUser != nil && strings.Contains(message.OfUser.Content.OfString.Value, "use the alternate approach") {
				found = true
			}
		}
		if !found {
			t.Fatalf("next provider request did not include steer prompt: %+v", secondRequest.Messages)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second provider request did not arrive")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		applied, err := store.GetSubmission(ctx, started.ConversationID, steer.SubmissionID)
		if err == nil && applied.Status == persistence.SubmissionConsumed {
			if applied.TurnID == nil || *applied.TurnID != firstTurnID || applied.AppliedAfterStep == nil || *applied.AppliedAfterStep != 1 || applied.AppliedHistoryRevision <= applied.HistoryRevision {
				t.Fatalf("steer has incorrect persisted boundary metadata: %+v", applied)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	steer, err = store.GetSubmission(ctx, started.ConversationID, steer.SubmissionID)
	t.Fatalf("steer was not consumed with the active turn: %+v err=%v", steer, err)
}

func TestConversationWorkerPromotesLateSteerAheadOfQueuedPrompts(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	firstPrompt, firstTurnID := "first request", "first-turn"
	fingerprint := persistence.SubmissionFingerprint(firstPrompt, persistence.SubmissionQueue)
	started, err := store.CreateConversation(ctx, &persistence.ConversationStartRequest{
		Session: persistence.SessionRecord{ID: "late-steering-test", WorkingDir: workspace, Model: "test-model"},
		StartID: "stable-start", PayloadFingerprint: fingerprint,
		FirstSubmission: persistence.SubmissionRecord{
			SubmissionID: "first-submission", Prompt: firstPrompt, Mode: persistence.SubmissionQueue,
			PayloadFingerprint: fingerprint, TurnID: &firstTurnID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &steeringBoundaryProvider{firstCall: make(chan struct{}), continueCh: make(chan struct{}), second: make(chan provider.GenerateRequest, 1)}
	engine := agent.NewEngine(provider, "test-model", 5, tools.NewRegistry(1000), nil, nil,
		agent.WithStore(store), agent.WithWorkspaceDir(workspace), agent.WithSessionIDOverride(started.ConversationID), agent.WithSystemPromptOverride("system"))
	historySession, err := engine.NewHistorySession(firstPrompt)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewConversationRuntime(ctx, engine, store, started.ConversationID, started.SubmissionID, historySession)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewConversationWorker(ctx, started.ConversationID, engine, runtime, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(&persistence.SubmissionRecord{ConversationID: started.ConversationID, SubmissionID: started.SubmissionID, TurnID: &firstTurnID}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)
	select {
	case <-provider.firstCall:
	case <-time.After(2 * time.Second):
		t.Fatal("first provider request did not start")
	}
	if _, _, err := worker.Submit(ctx, &persistence.SubmissionRecord{
		ConversationID: started.ConversationID, SubmissionID: "queued-submission", Prompt: "ordinary queued prompt", Mode: persistence.SubmissionQueue,
	}); err != nil {
		t.Fatalf("submit queued prompt: %v", err)
	}
	steer, _, err := worker.Submit(ctx, &persistence.SubmissionRecord{
		ConversationID: started.ConversationID, SubmissionID: "steer-submission", Prompt: "urgent steering prompt", Mode: persistence.SubmissionSteer,
	})
	if err != nil {
		t.Fatalf("submit steer: %v", err)
	}
	close(provider.continueCh)
	select {
	case secondRequest := <-provider.second:
		var content strings.Builder
		for _, message := range secondRequest.Messages {
			if message.OfUser != nil {
				content.WriteString(message.OfUser.Content.OfString.Value)
				content.WriteByte('\n')
			}
		}
		if !strings.Contains(content.String(), "urgent steering prompt") || strings.Contains(content.String(), "ordinary queued prompt") {
			t.Fatalf("late steer was not promoted ahead of ordinary queued work: %s", content.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("promoted steer did not start before the queued prompt")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		applied, err := store.GetSubmission(ctx, started.ConversationID, steer.SubmissionID)
		if err == nil && applied.Status == persistence.SubmissionConsumed {
			if applied.TurnID == nil || *applied.TurnID == firstTurnID {
				t.Fatalf("late steer should belong to the next completed turn: %+v", applied)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	steer, err = store.GetSubmission(ctx, started.ConversationID, steer.SubmissionID)
	t.Fatalf("promoted steer was not applied: %+v err=%v", steer, err)
}

func TestConversationPermissionSyncReconnectAndStaleRequest(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fingerprint := persistence.SubmissionFingerprint("prompt", persistence.SubmissionQueue)
	started, err := store.CreateConversation(ctx, &persistence.ConversationStartRequest{
		Session: persistence.SessionRecord{ID: "permission-test", WorkingDir: t.TempDir(), Model: "test-model"},
		StartID: "permission-start", PayloadFingerprint: fingerprint,
		FirstSubmission: persistence.SubmissionRecord{SubmissionID: "submission", Prompt: "prompt", Mode: persistence.SubmissionQueue, PayloadFingerprint: fingerprint, TurnID: ptrString("permission-turn")},
	})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	turnCtx, cancelTurn := context.WithCancel(workerCtx)
	defer cancelTurn()
	controller := &permissionTestConn{id: "controller-1", messages: make(chan *protocol.JSONRPCMessage, 8)}
	worker := &ConversationWorker{
		conversationID: started.ConversationID, store: store, ctx: workerCtx, cancel: cancelWorker,
		active: true, currentTurn: "permission-turn", currentTurnCtx: turnCtx,
		approvalChan: make(chan string, 1), controller: controller,
		subscribers: map[ClientConn]struct{}{controller: {}}, replaying: make(map[ClientConn][]sequencedEvent),
		ring: NewEventRingBuffer(DefaultRingBufferCapacity), workerEpoch: "permission-epoch",
	}
	decision := make(chan bool, 1)
	requestErr := make(chan error, 1)
	go func() {
		allowed, reqErr := worker.RequestPermission("bash", map[string]any{"command": "echo safe"})
		decision <- allowed
		requestErr <- reqErr
	}()
	var notification *protocol.JSONRPCMessage
	select {
	case notification = <-controller.messages:
	case <-time.After(2 * time.Second):
		t.Fatal("permission notification was not sent")
	}
	var request protocol.PermissionRequestNotification
	if notification.Method != protocol.NotifyPermissionRequired || json.Unmarshal(notification.Params, &request) != nil {
		t.Fatalf("invalid permission notification: %+v", notification)
	}
	if request.ConversationID != started.ConversationID || request.TurnID != "permission-turn" || request.RequestID == "" {
		t.Fatalf("permission request lacks conversation/turn identity: %+v", request)
	}

	snapshot, _, err := worker.Sync(ctx, protocol.ConversationSyncParams{ConversationID: started.ConversationID}, controller)
	if err != nil || snapshot.State != "awaiting_approval" || len(snapshot.PendingApprovals) != 1 {
		t.Fatalf("sync did not recover pending approval: snapshot=%+v err=%v", snapshot, err)
	}
	observer := &permissionTestConn{id: "observer", messages: make(chan *protocol.JSONRPCMessage, 8)}
	if role, _, err := worker.Attach(observer, "permission-epoch", 0, false); err != nil || role != "observer" {
		t.Fatalf("attach observer: role=%q err=%v", role, err)
	}
	observerSnapshot, _, err := worker.Sync(ctx, protocol.ConversationSyncParams{ConversationID: started.ConversationID}, observer)
	if err != nil || len(observerSnapshot.PendingApprovals) != 0 {
		t.Fatalf("observer received pending approval: snapshot=%+v err=%v", observerSnapshot, err)
	}
	worker.Detach(controller)
	newController := &permissionTestConn{id: "controller-2", messages: make(chan *protocol.JSONRPCMessage, 8)}
	if role, _, err := worker.Attach(newController, "permission-epoch", 0, false); err != nil || role != "controller" {
		t.Fatalf("reattach controller: role=%q err=%v", role, err)
	}
	select {
	case replay := <-newController.messages:
		var replayed protocol.PermissionRequestNotification
		if replay.Method != protocol.NotifyPermissionRequired || json.Unmarshal(replay.Params, &replayed) != nil || replayed.RequestID != request.RequestID {
			t.Fatalf("reattach replay mismatch: %+v / %+v", replay, replayed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending approval was not replayed on controller attach")
	}
	if err := worker.RespondPermission(newController, "old-turn", request.RequestID, "allow"); err == nil {
		t.Fatal("accepted stale-turn permission response")
	}
	if err := worker.RespondPermission(observer, request.TurnID, request.RequestID, "allow"); err == nil {
		t.Fatal("accepted permission response from observer")
	}
	if err := worker.RespondPermission(newController, request.TurnID, request.RequestID, "allow"); err != nil {
		t.Fatalf("respond to current request: %v", err)
	}
	select {
	case allowed := <-decision:
		if !allowed {
			t.Fatal("allow decision was not returned to request caller")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("permission request did not receive its decision")
	}
	if err := <-requestErr; err != nil {
		t.Fatalf("permission request returned error: %v", err)
	}
	if err := worker.RespondPermission(newController, request.TurnID, request.RequestID, "allow"); err == nil {
		t.Fatal("accepted stale permission request after resolution")
	}
}

func ptrString(s string) *string { return &s }
