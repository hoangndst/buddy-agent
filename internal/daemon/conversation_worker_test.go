package daemon

import (
	"context"
	"encoding/json"
	"fmt"
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

type uncooperativeProvider struct {
	entered chan struct{}
	release <-chan struct{}
}

func (p *uncooperativeProvider) Protocol() string { return "test" }
func (p *uncooperativeProvider) Generate(context.Context, provider.GenerateRequest) (*provider.GenerateResponse, error) {
	close(p.entered)
	<-p.release // Deliberately ignores cancellation to exercise the drain deadline.
	return &provider.GenerateResponse{Protocol: "test", Content: "completed after release"}, nil
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

func TestConversationWorkerSyncTranscriptPaginationAdvances(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fingerprint := persistence.SubmissionFingerprint("prompt-0", persistence.SubmissionQueue)
	started, err := store.CreateConversation(ctx, &persistence.ConversationStartRequest{
		Session: persistence.SessionRecord{ID: "pagination-test", WorkingDir: t.TempDir(), Model: "test-model"},
		StartID: "pagination-start", PayloadFingerprint: fingerprint,
		FirstSubmission: persistence.SubmissionRecord{SubmissionID: "submission-0", Prompt: "prompt-0", Mode: persistence.SubmissionQueue, PayloadFingerprint: fingerprint, TurnID: ptrString("turn-0")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 105; i++ {
		prompt := fmt.Sprintf("prompt-%d", i)
		if _, err := store.AdmitSubmission(ctx, &persistence.SubmissionRecord{
			ConversationID: started.ConversationID, SubmissionID: fmt.Sprintf("submission-%d", i), Prompt: prompt, Mode: persistence.SubmissionQueue,
		}); err != nil {
			t.Fatalf("admit submission %d: %v", i, err)
		}
	}
	worker := &ConversationWorker{conversationID: started.ConversationID, store: store, ring: NewEventRingBuffer(DefaultRingBufferCapacity), workerEpoch: "pagination-epoch"}
	first, _, err := worker.Sync(ctx, protocol.ConversationSyncParams{ConversationID: started.ConversationID, PageSize: 100}, nil)
	if err != nil {
		t.Fatalf("read first transcript page: %v", err)
	}
	if len(first.Transcript) != 100 || first.NextPageToken == "" {
		t.Fatalf("unexpected first page size/token: entries=%d token=%q", len(first.Transcript), first.NextPageToken)
	}
	second, _, err := worker.Sync(ctx, protocol.ConversationSyncParams{ConversationID: started.ConversationID, PageSize: 100, HistoryRevision: first.HistoryRevision, PageToken: first.NextPageToken}, nil)
	if err != nil {
		t.Fatalf("read second transcript page: %v", err)
	}
	if len(second.Transcript) != 5 || second.NextPageToken != "" || second.Transcript[0].Content == first.Transcript[0].Content {
		t.Fatalf("pagination did not advance to the final five entries: %+v", second)
	}
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

func TestConversationWorkerQueuesSteerAcceptedAfterTerminalSequence(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	fingerprint := persistence.SubmissionFingerprint("first prompt", persistence.SubmissionQueue)
	started, err := store.CreateConversation(ctx, &persistence.ConversationStartRequest{
		Session: persistence.SessionRecord{ID: "terminal-steer-test", WorkingDir: workspace, Model: "test-model"},
		StartID: "terminal-steer-start", PayloadFingerprint: fingerprint,
		FirstSubmission: persistence.SubmissionRecord{
			SubmissionID: "first-submission", Prompt: "first prompt", Mode: persistence.SubmissionQueue,
			PayloadFingerprint: fingerprint, TurnID: ptrString("terminal-turn"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &steeringBoundaryProvider{firstCall: make(chan struct{}), continueCh: make(chan struct{}), second: make(chan provider.GenerateRequest, 1)}
	engine := agent.NewEngine(provider, "test-model", 5, tools.NewRegistry(1000), nil, nil,
		agent.WithStore(store), agent.WithWorkspaceDir(workspace), agent.WithSessionIDOverride(started.ConversationID), agent.WithSystemPromptOverride("system"))
	historySession, err := engine.NewHistorySession("first prompt")
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
	t.Cleanup(worker.Stop)
	worker.mu.Lock()
	worker.active = true
	worker.currentTurn = "terminal-turn"
	worker.currentTerminalSent = true
	worker.mu.Unlock()

	steer, _, err := worker.Submit(ctx, &persistence.SubmissionRecord{
		ConversationID: started.ConversationID, SubmissionID: "late-steer", Prompt: "next turn steer", Mode: persistence.SubmissionSteer,
	})
	if err != nil {
		t.Fatalf("submit steer after terminal event: %v", err)
	}
	worker.mu.Lock()
	queued := len(worker.queue) == 1 && worker.queue[0].SubmissionID == steer.SubmissionID
	stranded := len(worker.pendingSteers) != 0
	_, pending := worker.pending[steer.SubmissionID]
	wakeQueued := len(worker.queueWake) > 0
	worker.mu.Unlock()
	if !queued || stranded || !pending || !wakeQueued {
		t.Fatalf("late steer was not made runnable: queued=%v stranded=%v pending=%v wake=%v", queued, stranded, pending, wakeQueued)
	}
}

func TestSessionManagerDrainReturnsAtDeadlineForUncooperativeProvider(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	storeClosed := make(chan struct{})
	pool := NewStorePool()
	pooled, _, err := pool.Get("drain-test", func() (persistence.Store, func(), error) {
		return store, func() {
			close(storeClosed)
			_ = store.Close()
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewSessionManager(nil)
	manager.SetStorePool(pool)
	workspace := t.TempDir()
	fingerprint := persistence.SubmissionFingerprint("run", persistence.SubmissionQueue)
	started, err := store.CreateConversation(ctx, &persistence.ConversationStartRequest{
		Session: persistence.SessionRecord{ID: "drain-uncooperative-test", WorkingDir: workspace, Model: "test-model"},
		StartID: "drain-start", PayloadFingerprint: fingerprint,
		FirstSubmission: persistence.SubmissionRecord{
			SubmissionID: "drain-submission", Prompt: "run", Mode: persistence.SubmissionQueue,
			PayloadFingerprint: fingerprint, TurnID: ptrString("drain-turn"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	provider := &uncooperativeProvider{entered: make(chan struct{}), release: release}
	engine := agent.NewEngine(provider, "test-model", 5, tools.NewRegistry(1000), nil, nil,
		agent.WithStore(pooled), agent.WithWorkspaceDir(workspace), agent.WithSessionIDOverride(started.ConversationID), agent.WithSystemPromptOverride("system"))
	historySession, err := engine.NewHistorySession("run")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.NewConversationRuntime(ctx, engine, pooled, started.ConversationID, started.SubmissionID, historySession)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDone := make(chan struct{})
	worker, err := NewConversationWorker(ctx, started.ConversationID, engine, runtime, nil, func() { close(cleanupDone) })
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(&persistence.SubmissionRecord{ConversationID: started.ConversationID, SubmissionID: started.SubmissionID, TurnID: ptrString("drain-turn")}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.conversations[started.ConversationID] = worker
	manager.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		worker.Stop()
		_ = pool.Close()
	})
	select {
	case <-provider.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("provider call did not start")
	}

	startedAt := time.Now()
	manager.Drain(30 * time.Millisecond)
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Drain exceeded its deadline by too much: %s", elapsed)
	}
	select {
	case <-worker.done:
		t.Fatal("Drain waited for a provider that ignored cancellation")
	default:
	}
	select {
	case <-storeClosed:
		t.Fatal("Drain closed shared stores while a conversation was still running")
	default:
	}
	if _, err := store.GetSession(ctx, started.ConversationID); err != nil {
		t.Fatalf("shared store became unavailable before the worker exited: %v", err)
	}

	close(release)
	select {
	case <-worker.done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not exit after releasing the provider")
	}
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("worker cleanup did not run after exit")
	}
	select {
	case <-storeClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("shared store was not closed after workers exited")
	}
}
