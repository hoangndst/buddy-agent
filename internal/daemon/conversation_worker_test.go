package daemon

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/hoangndst/buddy-agent/internal/tools/subprocess"
)

type permissionTestConn struct {
	id       string
	messages chan *protocol.JSONRPCMessage
}

type approvalBashRunner struct {
	mu       sync.Mutex
	commands []string
}

func (r *approvalBashRunner) Run(_ context.Context, opts subprocess.ExecutionOptions) (*subprocess.ExecutionResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, opts.Command)
	return &subprocess.ExecutionResult{Combined: "ok"}, nil
}

type childApprovalProvider struct {
	calls int
	mu    sync.Mutex
}

func (*childApprovalProvider) Protocol() string { return "test" }

func (p *childApprovalProvider) Generate(context.Context, provider.GenerateRequest) (*provider.GenerateResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		return &provider.GenerateResponse{Protocol: "test", ToolCalls: []provider.ToolCall{{
			ID: "child-bash", Name: "bash", Arguments: `{"command":"touch child-marker"}`,
		}}}, nil
	}
	return &provider.GenerateResponse{Protocol: "test", Content: "finished"}, nil
}

func TestSubagentToolsUseParentApprovalInterceptor(t *testing.T) {
	controller := &permissionTestConn{id: "controller", messages: make(chan *protocol.JSONRPCMessage, 2)}
	worker, cancel := newApprovalLifecycleWorker(controller, time.Second, time.Second)
	defer cancel()
	workspace := t.TempDir()
	runner := &approvalBashRunner{}
	registry := tools.NewRegistry(1000)
	registry.Register(tools.NewBashTool(workspace, time.Second, tools.WithRunner(runner)))
	registry.Register(agent.NewInvokeSubagentTool(agent.SubagentToolConfig{
		ParentRegistry: registry, Provider: &childApprovalProvider{}, Model: "test", WorkspaceDir: workspace,
	}))
	agent.NewEngine(nil, "test", 2, registry, nil, nil, agent.WithToolInterceptor(worker.interceptTool))
	subagent, ok := registry.Get("invoke_subagent")
	if !ok {
		t.Fatal("invoke_subagent was not registered")
	}

	result := make(chan error, 1)
	go func() {
		_, err := subagent.Execute(worker.currentTurnCtx, json.RawMessage(`{"task":"make a marker","role":"worker"}`))
		result <- err
	}()
	var request protocol.PermissionRequestNotification
	select {
	case message := <-controller.messages:
		if err := json.Unmarshal(message.Params, &request); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("child bash call bypassed the parent approval interceptor")
	}
	if request.TurnID != worker.currentTurn {
		t.Fatalf("child approval turn ID = %q, want parent turn %q", request.TurnID, worker.currentTurn)
	}
	runner.mu.Lock()
	commandsBeforeDecision := len(runner.commands)
	runner.mu.Unlock()
	if commandsBeforeDecision != 0 {
		t.Fatalf("child command executed before approval: %v", runner.commands)
	}
	if err := worker.RespondPermission(controller, protocol.PermissionRespondParams{
		RequestID: request.RequestID, TurnID: request.TurnID, Decision: protocol.DecisionDeny,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("child did not stop after approval was denied")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 0 {
		t.Fatalf("denied child command executed: %v", runner.commands)
	}
}

func TestConversationWorkerBashApprovalParsesAndCachesExactCommand(t *testing.T) {
	controller := &permissionTestConn{id: "controller", messages: make(chan *protocol.JSONRPCMessage, 4)}
	worker, cancel := newApprovalLifecycleWorker(controller, time.Second, time.Second)
	defer cancel()
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"command":"git status","Command":"touch /tmp/unapproved"}`),
		json.RawMessage(`{"command":"git status","command":"touch /tmp/unapproved"}`),
	} {
		if _, _, err := worker.interceptTool(worker.currentTurnCtx, "bash", raw); err == nil {
			t.Fatalf("ambiguous bash args %s were approved", raw)
		}
	}
	if len(controller.messages) != 0 || len(worker.sessionBashAllow) != 0 {
		t.Fatalf("ambiguous args reached approval/cache: messages=%d cache=%v", len(controller.messages), worker.sessionBashAllow)
	}

	command := "git status"
	raw := json.RawMessage(`{"command":"git status"}`)
	type result struct {
		allowed bool
		args    json.RawMessage
		err     error
	}
	intercept := func() <-chan result {
		resultCh := make(chan result, 1)
		go func() {
			allowed, args, err := worker.interceptTool(worker.currentTurnCtx, "bash", raw)
			resultCh <- result{allowed: allowed, args: args, err: err}
		}()
		return resultCh
	}
	resultCh := intercept()
	var request protocol.PermissionRequestNotification
	select {
	case message := <-controller.messages:
		if err := json.Unmarshal(message.Params, &request); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("bash approval was not requested")
	}
	if got := request.Arguments["command"]; got != command {
		t.Fatalf("approval prompt command = %v, want %q", got, command)
	}
	if err := worker.RespondPermission(controller, protocol.PermissionRespondParams{
		RequestID: request.RequestID, TurnID: request.TurnID, Decision: protocol.DecisionAllowSession,
		SessionWhitelist: true, CommandPattern: command,
	}); err != nil {
		t.Fatal(err)
	}
	first := <-resultCh
	if first.err != nil || !first.allowed || string(first.args) != `{"command":"git status"}` {
		t.Fatalf("first approved call = allowed:%t args:%s err:%v", first.allowed, first.args, first.err)
	}
	runner := &approvalBashRunner{}
	bash := tools.NewBashTool(t.TempDir(), time.Second, tools.WithRunner(runner))
	if _, err := bash.Execute(context.Background(), first.args); err != nil {
		t.Fatal(err)
	}
	if _, _, err := worker.interceptTool(worker.currentTurnCtx, "bash", json.RawMessage(`{"command":"git status","Command":"touch /tmp/unapproved"}`)); err == nil {
		t.Fatal("case-variant command bypassed the cached approval")
	}
	if len(runner.commands) != 1 || runner.commands[0] != command || len(controller.messages) != 0 {
		t.Fatalf("ambiguous cached call reached execution or prompted: commands=%v messages=%d", runner.commands, len(controller.messages))
	}

	second := <-intercept()
	if second.err != nil || !second.allowed || string(second.args) != string(first.args) {
		t.Fatalf("cached approved call = allowed:%t args:%s err:%v", second.allowed, second.args, second.err)
	}
	if len(controller.messages) != 0 {
		t.Fatal("identical session-approved command prompted again")
	}
	if _, err := bash.Execute(context.Background(), second.args); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 2 || runner.commands[0] != command || runner.commands[1] != command {
		t.Fatalf("approved command execution = %v", runner.commands)
	}
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

func TestConversationWorkerSyncRestoresBoundedRedactedActivityWithoutReasoning(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fingerprint := persistence.SubmissionFingerprint("inspect", persistence.SubmissionQueue)
	started, err := store.CreateConversation(ctx, &persistence.ConversationStartRequest{
		Session: persistence.SessionRecord{ID: "activity-history-test", WorkingDir: t.TempDir(), Model: "test-model"},
		StartID: "activity-history-start", PayloadFingerprint: fingerprint,
		FirstSubmission: persistence.SubmissionRecord{SubmissionID: "activity-history-submission", Prompt: "inspect", Mode: persistence.SubmissionQueue, PayloadFingerprint: fingerprint, TurnID: ptrString("activity-history-turn")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitTurn(ctx, &persistence.TurnRecord{
		SessionID: started.ConversationID, ConversationTurnID: "activity-history-turn", StepNumber: 1,
		Role: "assistant", Content: "Finished.", ReasoningContent: "private reasoning must stay private",
		ToolCalls: []persistence.ToolCallRecord{{
			ID: "call-1", Name: "bash", Args: `{"command":"git status --short","api_key":"sk-test-secret"}`,
			Result: "ghp_abcdefghijklmnopqrstuvwxyz1234567890",
		}},
	}, 1, persistence.TokenUsage{}); err != nil {
		t.Fatal(err)
	}
	worker := &ConversationWorker{conversationID: started.ConversationID, store: store, ring: NewEventRingBuffer(DefaultRingBufferCapacity), workerEpoch: "activity-history-epoch"}
	result, _, err := worker.Sync(ctx, protocol.ConversationSyncParams{ConversationID: started.ConversationID}, nil)
	if err != nil {
		t.Fatalf("sync activity history: %v", err)
	}
	if len(result.Transcript) < 2 || len(result.Transcript[1].Activities) != 1 {
		t.Fatalf("tool activity missing from durable transcript: %+v", result.Transcript)
	}
	activity := result.Transcript[1].Activities[0]
	if activity.Summary != "git status --short" || strings.Contains(activity.Summary, "secret") || strings.Contains(activity.Output, "abcdefghijklmnopqrstuvwxyz") || !strings.Contains(activity.Output, "[REDACTED]") {
		t.Fatalf("activity preview is missing or unredacted: %+v", activity)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private reasoning") || strings.Contains(string(encoded), "sk-test-secret") {
		t.Fatalf("sync exposed reasoning or unredacted tool arguments: %s", encoded)
	}
}

func TestConversationActivitiesBoundBatchAndOutputPreview(t *testing.T) {
	calls := make([]persistence.ToolCallRecord, conversationActivityLimit+3)
	for i := range calls {
		calls[i] = persistence.ToolCallRecord{ID: fmt.Sprintf("call-%d", i), Name: "read_file", Result: strings.Repeat("x", conversationActivityOutputSize+100)}
	}
	activities, omitted := conversationActivities(calls)
	if len(activities) != conversationActivityLimit || omitted != 3 || !activities[0].OutputTruncated || len(activities[0].Output) > conversationActivityOutputSize+64 {
		t.Fatalf("activity preview bounds = count %d omitted %d output %d truncated %v", len(activities), omitted, len(activities[0].Output), activities[0].OutputTruncated)
	}
}

func TestConversationWorkerSyncBoundsTranscriptPagesByEncodedFrameSize(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	largePrompt := strings.Repeat("x", 1200*1024)
	fingerprint := persistence.SubmissionFingerprint(largePrompt, persistence.SubmissionQueue)
	started, err := store.CreateConversation(ctx, &persistence.ConversationStartRequest{
		Session: persistence.SessionRecord{ID: "large-page-test", WorkingDir: t.TempDir(), Model: "test-model"},
		StartID: "large-page-start", PayloadFingerprint: fingerprint,
		FirstSubmission: persistence.SubmissionRecord{SubmissionID: "large-submission", Prompt: largePrompt, Mode: persistence.SubmissionQueue, PayloadFingerprint: fingerprint, TurnID: ptrString("large-turn")},
	})
	if err != nil {
		t.Fatal(err)
	}
	secondPrompt := strings.Repeat("y", 700*1024)
	if _, err := store.AdmitSubmission(ctx, &persistence.SubmissionRecord{
		ConversationID: started.ConversationID, SubmissionID: "second-submission", Prompt: secondPrompt, Mode: persistence.SubmissionQueue,
	}); err != nil {
		t.Fatal(err)
	}
	worker := &ConversationWorker{conversationID: started.ConversationID, store: store, ring: NewEventRingBuffer(DefaultRingBufferCapacity), workerEpoch: "large-page-epoch"}
	first, _, err := worker.Sync(ctx, protocol.ConversationSyncParams{ConversationID: started.ConversationID, PageSize: 100}, nil)
	if err != nil {
		t.Fatalf("sync oversized transcript entry: %v", err)
	}
	if len(first.Transcript) != 1 || !first.Transcript[0].ContentTruncated || first.NextPageToken == "" {
		t.Fatalf("oversized entry was not bounded and marked: entries=%+v token=%q", first.Transcript, first.NextPageToken)
	}
	frame, err := protocol.NewResponse(strings.Repeat("x", 128), first)
	if err != nil {
		t.Fatalf("encode sync response: %v", err)
	}
	encoded, err := protocol.EncodeMessage(frame)
	if err != nil || len(encoded) > protocol.DefaultMaxMessageSize {
		t.Fatalf("sync frame size=%d limit=%d err=%v", len(encoded), protocol.DefaultMaxMessageSize, err)
	}
	second, _, err := worker.Sync(ctx, protocol.ConversationSyncParams{
		ConversationID: started.ConversationID, HistoryRevision: first.HistoryRevision, PageToken: first.NextPageToken, PageSize: 100,
	}, nil)
	if err != nil {
		t.Fatalf("sync next transcript page: %v", err)
	}
	if len(second.Transcript) != 1 || second.Transcript[0].Content != secondPrompt || second.NextPageToken != "" {
		t.Fatalf("large page did not advance to the remaining prompt: %+v", second)
	}
}

func TestConversationRestoreBlocksUnsafeEndpointWithoutForwardingCredentials(t *testing.T) {
	record := &persistence.SessionRecord{ID: "unsafe-endpoint-conversation", Task: "prompt", Model: "test-model", MaxSteps: 3}
	secret := "must-not-be-forwarded"
	params, profile, blocked := conversationRestoreParams("/workspace", "", record, nil, protocol.ConversationResumeParams{
		WorkspaceDir: "/workspace", Model: "test-model", BaseURL: "https://user:pass@example.com/v1?token=secret", APIType: "chat", APIKey: &secret,
	})
	if blocked == "" || !strings.Contains(blocked, "secret URL") {
		t.Fatalf("unsafe endpoint did not fail closed: %q", blocked)
	}
	if params.BaseURL != "blocked://invalid" || params.APIKey != nil || strings.Contains(profile.BaseURL, "example.com") {
		t.Fatalf("unsafe endpoint could fall back to a live provider: params=%+v profile=%+v", params, profile)
	}
}

func TestConversationRestoreRequiresExplicitMCPAndEnvironmentReSupply(t *testing.T) {
	record := &persistence.SessionRecord{ID: "settings-conversation", Task: "prompt", Model: "test-model", MaxSteps: 3}
	baseProfile := persistence.ConversationProviderProfile{
		BaseURL: "https://provider.example/v1", APIType: "chat", Model: "test-model", MaxSteps: 3,
		CredentialSource: persistence.ConversationCredentialDaemonDefault,
	}
	for _, test := range []struct {
		name    string
		profile persistence.ConversationProviderProfile
	}{
		{name: "MCP config", profile: func() persistence.ConversationProviderProfile { p := baseProfile; p.RequiresMCPConfig = true; return p }()},
		{name: "environment", profile: func() persistence.ConversationProviderProfile { p := baseProfile; p.RequiresEnv = true; return p }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, blocked := conversationRestoreParams("/workspace", "", record, &test.profile, protocol.ConversationResumeParams{
				WorkspaceDir: "/workspace", Model: test.profile.Model, BaseURL: test.profile.BaseURL, APIType: test.profile.APIType,
			})
			if blocked == "" {
				t.Fatalf("restore silently discarded required %s", test.name)
			}
		})
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
		controller: controller, approvalTimeout: time.Minute, detachedTimeout: time.Minute,
		subscribers: map[ClientConn]struct{}{controller: {}}, replaying: make(map[ClientConn][]sequencedEvent),
		ring: NewEventRingBuffer(DefaultRingBufferCapacity), workerEpoch: "permission-epoch",
		sessionToolAllow: make(map[string]struct{}), sessionBashAllow: make(map[string]struct{}),
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
	if err := worker.RespondPermission(newController, protocol.PermissionRespondParams{TurnID: "old-turn", RequestID: request.RequestID, Decision: protocol.DecisionAllowOnce}); err == nil {
		t.Fatal("accepted stale-turn permission response")
	}
	if err := worker.RespondPermission(observer, protocol.PermissionRespondParams{TurnID: request.TurnID, RequestID: request.RequestID, Decision: protocol.DecisionAllowOnce}); err == nil {
		t.Fatal("accepted permission response from observer")
	}
	if err := worker.RespondPermission(newController, protocol.PermissionRespondParams{TurnID: request.TurnID, RequestID: request.RequestID, Decision: protocol.DecisionAllowOnce}); err != nil {
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
	if err := worker.RespondPermission(newController, protocol.PermissionRespondParams{TurnID: request.TurnID, RequestID: request.RequestID, Decision: protocol.DecisionAllowOnce}); err == nil {
		t.Fatal("accepted stale permission request after resolution")
	}
}

func newApprovalLifecycleWorker(controller ClientConn, attachedTimeout, detachedTimeout time.Duration) (*ConversationWorker, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return &ConversationWorker{
		conversationID: "approval-lifecycle", active: true, currentTurn: "turn-1", currentTurnCtx: ctx,
		controller: controller, approvalTimeout: attachedTimeout, detachedTimeout: detachedTimeout,
		sessionToolAllow: make(map[string]struct{}), sessionBashAllow: make(map[string]struct{}),
	}, cancel
}

func TestConversationWorkerApprovalTimeoutAndStickyDisconnectDeadline(t *testing.T) {
	controller := &permissionTestConn{id: "controller-1", messages: make(chan *protocol.JSONRPCMessage, 4)}
	worker, cancel := newApprovalLifecycleWorker(controller, time.Second, 60*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := worker.requestPermission(worker.currentTurnCtx, "bash", map[string]any{"command": "echo safe"})
		result <- err
	}()
	select {
	case <-controller.messages:
	case <-time.After(time.Second):
		t.Fatal("approval request was not sent")
	}
	worker.Detach(controller)
	worker.mu.Lock()
	pending := worker.pendingApproval
	firstDeadline := pending.deadline
	worker.controller = &permissionTestConn{id: "controller-2", messages: make(chan *protocol.JSONRPCMessage, 4)}
	controller2 := worker.controller
	worker.mu.Unlock()
	worker.Detach(controller2)
	worker.mu.Lock()
	if !worker.pendingApproval.deadline.Equal(firstDeadline) {
		t.Fatalf("second detach extended deadline: got %v want %v", worker.pendingApproval.deadline, firstDeadline)
	}
	worker.mu.Unlock()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("approval timeout error = %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("detached approval did not time out")
	}
}

func TestConversationWorkerBashSessionApprovalMatchesExactCommand(t *testing.T) {
	controller := &permissionTestConn{id: "controller", messages: make(chan *protocol.JSONRPCMessage, 8)}
	worker, cancel := newApprovalLifecycleWorker(controller, time.Second, time.Second)
	defer cancel()
	command := "git status"
	result := make(chan protocol.PermissionRespondParams, 1)
	go func() {
		response, err := worker.requestPermission(context.Background(), "bash", map[string]any{"command": command})
		if err != nil {
			result <- protocol.PermissionRespondParams{DenialReason: err.Error()}
			return
		}
		result <- response
	}()
	var request protocol.PermissionRequestNotification
	select {
	case message := <-controller.messages:
		if err := json.Unmarshal(message.Params, &request); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("approval request was not sent")
	}
	bad := protocol.PermissionRespondParams{RequestID: request.RequestID, TurnID: request.TurnID, Decision: protocol.DecisionAllowSession, CommandPattern: "git"}
	if err := worker.RespondPermission(controller, bad); err == nil {
		t.Fatal("accepted a command prefix instead of the exact command")
	}
	good := protocol.PermissionRespondParams{RequestID: request.RequestID, TurnID: request.TurnID, Decision: protocol.DecisionAllowSession, CommandPattern: command}
	if err := worker.RespondPermission(controller, good); err != nil {
		t.Fatalf("respond to exact command: %v", err)
	}
	select {
	case response := <-result:
		if response.Decision != protocol.DecisionAllowSession {
			t.Fatalf("session approval result = %+v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("approval response was not returned")
	}
	response, err := worker.requestPermission(context.Background(), "bash", map[string]any{"command": command})
	if err != nil || response.Decision != protocol.DecisionAllowOnce {
		t.Fatalf("exact command was not cached: response=%+v err=%v", response, err)
	}
	different := "git status; rm -rf ./tmp"
	result = make(chan protocol.PermissionRespondParams, 1)
	go func() {
		response, err := worker.requestPermission(context.Background(), "bash", map[string]any{"command": different})
		if err != nil {
			result <- protocol.PermissionRespondParams{DenialReason: err.Error()}
			return
		}
		result <- response
	}()
	select {
	case message := <-controller.messages:
		if err := json.Unmarshal(message.Params, &request); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("changed command bypassed the session approval")
	}
	if err := worker.RespondPermission(controller, protocol.PermissionRespondParams{
		RequestID: request.RequestID, TurnID: request.TurnID, Decision: protocol.DecisionAllowOnce,
	}); err != nil {
		t.Fatalf("respond to changed command: %v", err)
	}
	if response := <-result; response.Decision != protocol.DecisionAllowOnce {
		t.Fatalf("changed command response = %+v", response)
	}
}

func TestConversationWorkerCancellationWinsAgainstApproval(t *testing.T) {
	controller := &permissionTestConn{id: "controller", messages: make(chan *protocol.JSONRPCMessage, 2)}
	worker, cancel := newApprovalLifecycleWorker(controller, time.Second, time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := worker.requestPermission(worker.currentTurnCtx, "bash", map[string]any{"command": "echo safe"})
		result <- err
	}()
	var request protocol.PermissionRequestNotification
	select {
	case message := <-controller.messages:
		if err := json.Unmarshal(message.Params, &request); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("approval request was not sent")
	}
	cancel()
	if err := worker.RespondPermission(controller, protocol.PermissionRespondParams{
		RequestID: request.RequestID, TurnID: request.TurnID, Decision: protocol.DecisionAllowOnce,
	}); err == nil {
		t.Fatal("accepted approval after turn cancellation")
	}
	select {
	case err := <-result:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("approval wait error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled approval wait did not return")
	}
}

func TestConversationWorkerInterceptPreservesEmptyModifiedArguments(t *testing.T) {
	controller := &permissionTestConn{id: "controller", messages: make(chan *protocol.JSONRPCMessage, 2)}
	worker, cancel := newApprovalLifecycleWorker(controller, time.Second, time.Second)
	defer cancel()
	type result struct {
		allowed bool
		args    json.RawMessage
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		allowed, args, err := worker.interceptTool(worker.currentTurnCtx, "write_file", json.RawMessage(`{"path":"old.go"}`))
		resultCh <- result{allowed: allowed, args: args, err: err}
	}()
	var request protocol.PermissionRequestNotification
	select {
	case message := <-controller.messages:
		if err := json.Unmarshal(message.Params, &request); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("approval request was not sent")
	}
	empty := map[string]any{}
	payload, err := json.Marshal(protocol.PermissionRespondParams{
		RequestID: request.RequestID, TurnID: request.TurnID, Decision: protocol.DecisionAllowOnce, ModifiedArguments: &empty,
	})
	if err != nil {
		t.Fatal(err)
	}
	var response protocol.PermissionRespondParams
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	if err := worker.RespondPermission(controller, response); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got.err != nil || !got.allowed || string(got.args) != "{}" {
			t.Fatalf("intercepted empty replacement = allowed:%t args:%s err:%v", got.allowed, got.args, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("approval interceptor did not return")
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
