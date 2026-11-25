package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/client"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tui"
	"golang.org/x/term"
)

type conversationTUI struct {
	ctx                    context.Context
	client                 conversationAPI
	workspace              string
	gitBranch              string
	startParams            protocol.ConversationStartParams
	conversation           string
	startPending           bool
	pendingStart           []pendingStartSubmission
	pendingStartInFlight   bool
	databasePath           string
	standalone             bool
	state                  string
	workerEpoch            string
	lastSequence           uint64
	ui                     *tui.Model
	pendingApprovals       []pendingApprovalRequest
	approvalGeneration     uint64
	pickerGeneration       uint64
	resumeGeneration       uint64
	queued                 []string
	steers                 []string
	errorText              string
	workspaces             []protocol.ConversationWorkspace
	conversations          []protocol.ConversationSummary
	selectedWorkspace      string
	loadWorkspaceMCPConfig func(string) (*mcp.Config, error)
	outputPageStarts       []int64
	outputPageIndex        int
	inspectionGeneration   uint64
	outputLoading          bool
}

type pendingApprovalRequest struct {
	request    protocol.PermissionRequestNotification
	generation uint64
}

// BlockKind is kept as an alias for the command adapter's transcript tests.
type BlockKind = tui.BlockKind

const (
	// BlockUserMessage identifies a submitted prompt.
	BlockUserMessage = tui.BlockUserMessage
	// BlockAssistantMessage identifies an assistant response.
	BlockAssistantMessage = tui.BlockAssistantMessage
	// BlockToolActivity identifies a tool execution.
	BlockToolActivity = tui.BlockToolActivity
	// BlockSubagentActivity identifies delegated tool activity.
	BlockSubagentActivity = tui.BlockSubagentActivity
	// BlockActivityGroup summarizes tool calls from one model step.
	BlockActivityGroup = tui.BlockActivityGroup
	// BlockSystemStatus identifies a system notice.
	BlockSystemStatus = tui.BlockSystemStatus
)

// TranscriptBlock is the canonical rendered conversation entry.
type TranscriptBlock = tui.TranscriptBlock

type conversationAPI interface {
	Capabilities() (*protocol.DaemonCapabilitiesResult, error)
	StartConversation(protocol.ConversationStartParams) (*protocol.ConversationStartResult, error)
	ResumeConversation(protocol.ConversationResumeParams) (*protocol.ConversationResumeResult, error)
	SyncConversation(protocol.ConversationSyncParams) (*protocol.ConversationSyncResult, error)
	ListConversationWorkspaces(int) (*protocol.ConversationListWorkspacesResult, error)
	ListConversations(protocol.ConversationListParams) (*protocol.ConversationListResult, error)
	SubmitConversation(protocol.ConversationSubmitParams) (*protocol.ConversationSubmitResult, error)
	InterruptTurn(protocol.TurnInterruptParams) (*protocol.TurnInterruptResult, error)
	ReadOutput(protocol.OutputReadParams) (*protocol.OutputReadResult, error)
	InspectDiff(protocol.DiffInspectParams) (*protocol.DiffInspectResult, error)
	RespondConversationPermissionParams(protocol.PermissionRespondParams) error
	ConversationEvents() <-chan protocol.SessionEventNotification
	PermissionRequests() <-chan protocol.PermissionRequestNotification
	ConversationEventGaps() <-chan struct{}
	ConversationEvictions() <-chan string
	ClearConversationEventGap()
	Close() error
}

type conversationStartedMsg struct {
	result *protocol.ConversationStartResult
	err    error
}
type conversationSubmittedMsg struct {
	result       *protocol.ConversationSubmitResult
	prompt       string
	mode         persistence.SubmissionMode
	pendingStart bool
	err          error
}
type pendingStartSubmission struct {
	prompt string
	mode   persistence.SubmissionMode
}
type conversationEventMsg protocol.SessionEventNotification
type permissionRequestMsg protocol.PermissionRequestNotification
type permissionRespondedMsg struct {
	requestID  string
	generation uint64
	err        error
}
type conversationResumeListMsg struct {
	generation uint64
	workspaces []protocol.ConversationWorkspace
	err        error
}
type conversationListMsg struct {
	generation    uint64
	conversations []protocol.ConversationSummary
	err           error
}
type conversationResumedMsg struct {
	pickerGeneration     uint64
	resumeGeneration     uint64
	sourceConversationID string
	sourceWorkerEpoch    string
	result               *protocol.ConversationSyncResult
	workspace            string
	databasePath         string
	mcpConfig            *mcp.Config
	updateMCPConfig      bool
	err                  error
}
type conversationSyncMsg struct {
	result          *protocol.ConversationSyncResult
	fromWorkerEpoch string
	err             error
}
type outputPageMsg struct {
	generation uint64
	ref        string
	start      int64
	result     *protocol.OutputReadResult
	err        error
}
type diffInspectMsg struct {
	generation uint64
	result     *protocol.DiffInspectResult
	err        error
}
type conversationGapMsg struct{}
type conversationEvictedMsg string
type gitBranchMsg struct {
	workspace string
	branch    string
}

var (
	uiUserStyle  = lipgloss.NewStyle().Bold(true).Foreground(tui.ThemeColor("#27609B", "#78B9EF"))
	uiBuddyStyle = lipgloss.NewStyle().Bold(true).Foreground(tui.ThemeColor("#A94721", "#EFA276"))
)

func runConversationTUI(ctx context.Context, f *cmdutil.Factory, socketPath string, continueLatest, standalone bool) error {
	params, workspace, err := buildConversationStartParams(f)
	if err != nil {
		return err
	}
	var api conversationAPI
	if standalone {
		api, err = newLocalConversationAPI(ctx, f)
	} else {
		var cl *client.Client
		cl, err = client.ConnectOrSpawn(socketPath)
		api = cl
	}
	if err != nil {
		return fmt.Errorf("connect to conversation runtime: %w", err)
	}
	defer func() { _ = api.Close() }()
	return runConversationTUIWithAPI(ctx, f, api, params, workspace, continueLatest, standalone)
}

func runConversationTUIWithAPI(ctx context.Context, f *cmdutil.Factory, api conversationAPI, params protocol.ConversationStartParams, workspace string, continueLatest, standalone bool) error {
	capabilities, err := api.Capabilities()
	if err != nil {
		return fmt.Errorf("check daemon capabilities: %w", err)
	}
	if !containsConversationMethods(capabilities.Methods) {
		return fmt.Errorf("this daemon does not support M9 conversations; restart it when its current work is safe to stop, then retry")
	}
	historyPath := os.Getenv("BUDDY_HISTORY_FILE")
	if historyPath == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return homeErr
		}
		historyPath = filepath.Join(home, ".buddy", "history.jsonl")
	}
	history, historyErr := tui.NewInputHistoryRing(historyPath)
	uiModel := tui.NewModel(tui.Header{Model: params.Model, Workspace: workspace})
	uiModel.Composer.History = history
	m := &conversationTUI{
		ctx:          ctx,
		client:       api,
		workspace:    workspace,
		startParams:  params,
		databasePath: params.DBPath,
		standalone:   standalone,
		state:        "ready",
		ui:           uiModel,
	}
	for _, command := range []tui.SlashCommand{
		{Name: "/help", Description: "Show available commands"},
		{Name: "/clear", Description: "Clear the transcript"},
		{Name: "/quit", Description: "Exit the client"},
		{Name: "/resume", Description: "Resume a conversation"},
		{Name: "/steer", Description: "Guide the active turn", ArgsHint: "<instruction>"},
		{Name: "/interrupt", Description: "Stop the active turn"},
		{Name: "/queue", Description: "Queue a prompt", ArgsHint: "<prompt>"},
		{Name: "/copy", Description: "Copy the transcript"},
		{Name: "/diff", Description: "Review working-tree changes"},
	} {
		m.ui.Composer.Commands.Register(command)
	}
	if historyErr != nil {
		m.errorText = fmt.Sprintf("load prompt history: %v", historyErr)
	}
	if f.MCPConfigLoader != nil {
		home := ""
		if f.UserHome != nil {
			home, _ = f.UserHome()
		} else {
			home, _ = os.UserHomeDir()
		}
		m.loadWorkspaceMCPConfig = func(project string) (*mcp.Config, error) {
			return f.MCPConfigLoader(project, home)
		}
	}
	if continueLatest {
		result, resumeErr := api.ResumeConversation(conversationResumeParams(workspace, "", params))
		if resumeErr != nil {
			if isConversationNotFound(resumeErr) {
				return fmt.Errorf("no conversations found in workspace %q; run bare buddy to start one", workspace)
			}
			return fmt.Errorf("resume latest conversation: %w", resumeErr)
		}
		m.conversation = result.ConversationID
		m.state = result.State
		if syncResult, syncErr := readConversationSnapshot(api, result.ConversationID); syncErr == nil {
			m.loadSync(syncResult)
		} else {
			return fmt.Errorf("sync resumed conversation: %w", syncErr)
		}
	}
	return tui.Run(ctx, m, f.IOStreams.In, f.IOStreams.Out)
}

func isInteractive(stream any) bool {
	file, ok := stream.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

func runConversationLines(ctx context.Context, f *cmdutil.Factory, socketPath string, continueLatest bool) error {
	params, workspace, err := buildConversationStartParams(f)
	if err != nil {
		return err
	}
	cl, err := client.ConnectOrSpawn(socketPath)
	if err != nil {
		return fmt.Errorf("connect to conversation daemon: %w", err)
	}
	defer func() { _ = cl.Close() }()
	capabilities, err := cl.Capabilities()
	if err != nil {
		return fmt.Errorf("check daemon capabilities: %w", err)
	}
	if !containsConversationMethods(capabilities.Methods) {
		return fmt.Errorf("this daemon does not support M9 conversations; restart it when its current work is safe to stop, then retry")
	}
	conversationID := ""
	if continueLatest {
		resumed, err := cl.ResumeConversation(conversationResumeParams(workspace, "", params))
		if err != nil {
			if isConversationNotFound(err) {
				return fmt.Errorf("no conversations found in workspace %q; run bare buddy to start one", workspace)
			}
			return fmt.Errorf("resume latest conversation: %w", err)
		}
		conversationID = resumed.ConversationID
		if syncResult, syncErr := readConversationSnapshot(cl, conversationID); syncErr == nil {
			for _, entry := range syncResult.Transcript {
				if entry.Role == "user" {
					_, _ = fmt.Fprintf(f.IOStreams.Out, "\n%s\n", uiUserStyle.Render("You: ")+entry.Content)
				}
				if entry.Role == "assistant" {
					_, _ = fmt.Fprintf(f.IOStreams.Out, "\n%s\n", uiBuddyStyle.Render("Buddy: ")+entry.Content)
				}
			}
		}
	}
	scanner := bufio.NewScanner(f.IOStreams.In)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for {
		_, _ = fmt.Fprint(f.IOStreams.Out, "\nbuddy> ")
		if !scanner.Scan() {
			break
		}
		prompt := strings.TrimSpace(scanner.Text())
		if prompt == "" {
			continue
		}
		if prompt == "exit" || prompt == "quit" || prompt == "/quit" {
			break
		}
		mode := persistence.SubmissionQueue
		if strings.HasPrefix(prompt, "/steer ") {
			mode = persistence.SubmissionSteer
			prompt = strings.TrimSpace(strings.TrimPrefix(prompt, "/steer "))
		}
		if conversationID == "" {
			params.StartID, params.SubmissionID, params.Prompt = newConversationID("start"), newConversationID("submission"), prompt
			started, err := cl.StartConversation(params)
			if err != nil {
				return fmt.Errorf("start conversation: %w", err)
			}
			conversationID = started.ConversationID
			if err := waitConversationSubmissionTerminal(ctx, f, cl, conversationID, started.SubmissionID, started.TurnID, persistence.SubmissionQueue); err != nil {
				return err
			}
		} else {
			submissionID := newConversationID("submission")
			submitted, err := cl.SubmitConversation(protocol.ConversationSubmitParams{ConversationID: conversationID, SubmissionID: submissionID, Prompt: prompt, Mode: string(mode)})
			if err != nil {
				return fmt.Errorf("submit conversation prompt: %w", err)
			}
			if err := waitConversationSubmissionTerminal(ctx, f, cl, conversationID, submitted.SubmissionID, submitted.TurnID, mode); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

func waitConversationSubmissionTerminal(ctx context.Context, f *cmdutil.Factory, cl *client.Client, conversationID, submissionID, turnID string, mode persistence.SubmissionMode) error {
	return waitConversationSubmissionEvents(ctx, f, conversationID, submissionID, turnID, mode,
		cl.ConversationEvents(), cl.ConversationEventGaps(),
		func() (*protocol.ConversationSyncResult, error) {
			return cl.SyncConversation(protocol.ConversationSyncParams{ConversationID: conversationID, PageSize: 1})
		},
		func() (*protocol.ConversationSyncResult, error) { return readConversationSnapshot(cl, conversationID) },
		func() error {
			_, err := cl.SyncConversation(protocol.ConversationSyncParams{ConversationID: conversationID})
			return err
		})
}

func waitConversationSubmissionEvents(ctx context.Context, f *cmdutil.Factory, conversationID, submissionID, turnID string, mode persistence.SubmissionMode,
	events <-chan protocol.SessionEventNotification, gaps <-chan struct{},
	snapshot func() (*protocol.ConversationSyncResult, error), fullSnapshot func() (*protocol.ConversationSyncResult, error), resync func() error,
) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	terminalTurns := make(map[string]bool)
	renderedAssistantSteps := make(map[string]map[int]bool)
	turnStatus, turnError := "", ""
	resolveTurn := func() error {
		if submissionID == "" && turnID == "" {
			return nil
		}
		synced, err := snapshot()
		if err != nil {
			return fmt.Errorf("sync conversation while waiting for submitted turn: %w", err)
		}
		for _, turn := range synced.Turns {
			matchesSubmission := submissionID != "" && turn.SubmissionID == submissionID
			matchesTurn := turnID != "" && turn.TurnID == turnID
			if (matchesSubmission || matchesTurn) && turn.TurnID != "" {
				turnID, turnStatus, turnError = turn.TurnID, turn.Status, turn.Error
				break
			}
		}
		if turnID == "" && mode == persistence.SubmissionSteer && submissionID != "" {
			history, historyErr := fullSnapshot()
			if historyErr != nil {
				return fmt.Errorf("sync conversation while resolving steering submission: %w", historyErr)
			}
			for _, entry := range history.Transcript {
				if entry.SubmissionID == submissionID && entry.TurnID != "" {
					turnID = entry.TurnID
					break
				}
			}
		}
		return nil
	}
	for {
		if err := resolveTurn(); err != nil {
			return err
		}
		if turnID != "" && turnStatus != "" && turnStatus != string(persistence.StatusRunning) {
			if !terminalTurns[turnID] {
				if history, err := fullSnapshot(); err == nil {
					for _, entry := range history.Transcript {
						if entry.Role == "assistant" && entry.TurnID == turnID {
							if renderedAssistantSteps[turnID][entry.StepNumber] {
								continue
							}
							_, _ = fmt.Fprintln(f.IOStreams.Out, entry.Content)
							if entry.StepNumber > 0 {
								if renderedAssistantSteps[turnID] == nil {
									renderedAssistantSteps[turnID] = make(map[int]bool)
								}
								renderedAssistantSteps[turnID][entry.StepNumber] = true
							}
						}
					}
				}
			}
			if turnStatus != string(persistence.StatusCompleted) {
				if turnError == "" {
					turnError = turnStatus
				}
				return fmt.Errorf("conversation turn failed: %s", turnError)
			}
			return nil
		}
		if turnID != "" && terminalTurns[turnID] {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case evt, ok := <-events:
			if !ok {
				return fmt.Errorf("conversation daemon connection closed")
			}
			if evt.ConversationID != conversationID {
				continue
			}
			renderEvent(f, evt)
			if evt.Type == string(agent.EventTextComplete) && evt.TurnID != "" && evt.Content != "" {
				if step := conversationEventStep(evt); step > 0 {
					if renderedAssistantSteps[evt.TurnID] == nil {
						renderedAssistantSteps[evt.TurnID] = make(map[int]bool)
					}
					renderedAssistantSteps[evt.TurnID][step] = true
				}
			}
			terminal := evt.Type == string(agent.EventFinished) || evt.Type == string(agent.EventError) && evt.Data["terminal"] == true
			if terminal {
				terminalTurns[evt.TurnID] = true
				if turnID != "" && evt.TurnID == turnID {
					if evt.Type == string(agent.EventError) {
						return fmt.Errorf("conversation turn failed: %s", evt.Content)
					}
					return nil
				}
			}
		case <-gaps:
			if err := resync(); err != nil {
				return fmt.Errorf("resync conversation after event gap: %w", err)
			}
		case <-ticker.C:
		}
	}
}

func conversationEventStep(event protocol.SessionEventNotification) int {
	if event.Data == nil {
		return 0
	}
	switch step := event.Data["step"].(type) {
	case int:
		return step
	case float64:
		return int(step)
	default:
		return 0
	}
}

func containsConversationMethods(methods []string) bool {
	wanted := map[string]bool{
		protocol.MethodConversationStart: false, protocol.MethodConversationResume: false,
		protocol.MethodConversationSubmit: false, protocol.MethodConversationSync: false,
		protocol.MethodConversationList: false, protocol.MethodConversationWorkspaces: false,
		protocol.MethodTurnInterrupt: false,
	}
	for _, method := range methods {
		if _, ok := wanted[method]; ok {
			wanted[method] = true
		}
	}
	for _, found := range wanted {
		if !found {
			return false
		}
	}
	return true
}

func isConversationNotFound(err error) bool {
	if errors.Is(err, persistence.ErrSessionNotFound) {
		return true
	}
	var rpcErr *protocol.JSONRPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == protocol.ErrCodeSessionNotFound
}

func (m *conversationTUI) Init() tea.Cmd {
	m.ensureUIModel()
	if m.conversation != "" {
		return tea.Batch(textarea.Blink, m.loadGitBranch(), m.watchEvent(), m.watchPermission())
	}
	return tea.Batch(textarea.Blink, m.loadGitBranch())
}

func (m *conversationTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.ensureUIModel()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		_, _ = m.ui.Update(msg)
		m.clearCache()
		if m.ui.ActiveBlockID() != "" {
			for _, b := range m.ui.Transcript.Blocks {
				if b.ID == m.ui.ActiveBlockID() {
					_ = m.getWrappedLines(b, m.ui.Width)
					break
				}
			}
		}
		visible := m.computeVisibleBlocks()
		for _, b := range visible {
			_ = m.getWrappedLines(b, m.ui.Width)
		}
	case tea.MouseWheelMsg:
		_, _ = m.ui.Update(msg)
		return m, nil
	case gitBranchMsg:
		if msg.workspace == m.workspace {
			m.gitBranch = msg.branch
		}
	case tui.FlushMsg:
		_, cmd := m.ui.Update(msg)
		m.syncUIRenderState()
		return m, cmd
	case tui.ActionMsg:
		switch msg.Action.Kind {
		case tui.ActionSubmitPrompt:
			if msg.Action.Mode == "" {
				return m, m.handlePromptInput(msg.Action.Prompt)
			}
			m.pickerGeneration++
			mode := persistence.SubmissionQueue
			if msg.Action.Mode == string(persistence.SubmissionSteer) {
				mode = persistence.SubmissionSteer
			}
			return m, m.submit(msg.Action.Prompt, mode)
		case tui.ActionInterruptTurn:
			return m, m.interruptTurn()
		case tui.ActionOpenOutput:
			m.inspectionGeneration++
			m.outputPageStarts, m.outputPageIndex, m.outputLoading = []int64{0}, 0, true
			_, _ = m.ui.Update(tui.Event{Kind: tui.EventInspectionStart, InspectionKind: tui.InspectionOutput, InspectionGeneration: m.inspectionGeneration, InspectionRef: msg.Action.Ref})
			m.ui.Composer.Editor.Blur()
			return m, m.readOutputPage(msg.Action.Ref, 0, m.inspectionGeneration)
		case tui.ActionChangeOutputPage:
			if m.ui.HasInspection() && msg.Action.InspectionGeneration == m.inspectionGeneration {
				return m, m.changeOutputPage(msg.Action.PageDelta)
			}
		case tui.ActionCloseInspection:
			if msg.Action.InspectionGeneration == m.inspectionGeneration {
				m.inspectionGeneration++
				m.outputPageStarts, m.outputPageIndex, m.outputLoading = nil, 0, false
			}
		case tui.ActionPermissionChoice:
			request := m.approvalForGeneration(msg.Action.ApprovalGeneration)
			if request == nil {
				return m, m.syncConversation()
			}
			params, err := approvalParamsForRequest(msg.Action.ApprovalChoice, request.request)
			if err != nil {
				m.errorText = err.Error()
				return m, nil
			}
			return m, m.respondPermission(request, *params)
		case tui.ActionSelectChoice:
			if m.ui.Picker != msg.Action.PickerKind || msg.Action.PickerGeneration == 0 || msg.Action.PickerGeneration != m.pickerGeneration {
				return m, nil
			}
			generation := m.nextPickerGeneration()
			_, _ = m.ui.Update(tui.Event{Kind: tui.EventPickerGeneration, PickerGeneration: generation})
			switch msg.Action.PickerKind {
			case tui.PickerWorkspace:
				for _, workspace := range m.workspaces {
					if workspace.WorkspaceDir == msg.Action.ChoiceID {
						m.selectedWorkspace = workspace.WorkspaceDir
						return m, m.listConversations(m.selectedWorkspace, generation)
					}
				}
			case tui.PickerConversation:
				for _, conversation := range m.conversations {
					if conversation.ConversationID == msg.Action.ChoiceID {
						return m, m.resumeSelected(conversation, generation, m.nextResumeGeneration())
					}
				}
			}
			return m, nil
		case tui.ActionCancelPicker:
			if msg.Action.PickerKind != tui.PickerNone && msg.Action.PickerGeneration != 0 && msg.Action.PickerGeneration == m.pickerGeneration {
				m.pickerGeneration++
			}
			return m, nil
		}
		return m, nil
	case conversationStartedMsg:
		if msg.err != nil {
			m.startPending = false
			m.errorText = msg.err.Error()
			if len(m.pendingStart) > 0 {
				m.pendingStart = nil
				m.errorText += "; prompts queued while starting were discarded; resubmit them"
			}
			return m, nil
		}
		m.startPending = false
		if msg.result == nil || msg.result.ConversationID == "" {
			m.errorText = "start conversation returned no conversation ID"
			m.pendingStart = nil
			return m, nil
		}
		m.nextResumeGeneration()
		m.conversation = msg.result.ConversationID
		m.state = "running"
		cmds := []tea.Cmd{m.syncConversation(), m.watchPermission()}
		cmds = append(cmds, m.submitNextPendingStart())
		return m, tea.Batch(cmds...)
	case conversationSubmittedMsg:
		if msg.pendingStart {
			m.pendingStartInFlight = false
		}
		if msg.err != nil {
			m.errorText = msg.err.Error()
			m.state = "idle"
			if msg.pendingStart {
				return m, nil
			}
			return m, nil
		}
		if msg.pendingStart && len(m.pendingStart) > 0 {
			m.pendingStart = m.pendingStart[1:]
		}
		if msg.mode == persistence.SubmissionSteer {
			m.steers = append(m.steers, msg.prompt)
		} else {
			m.queued = append(m.queued, msg.prompt)
		}
		m.state = "running"
		if len(m.pendingStart) > 0 {
			return m, tea.Batch(m.syncConversation(), m.submitNextPendingStart())
		}
		return m, m.syncConversation()
	case conversationEventMsg:
		if msg.ConversationID != m.conversation || (msg.WorkerEpoch != "" && m.workerEpoch != "" && msg.WorkerEpoch != m.workerEpoch) {
			return m, m.watchEvent()
		}
		if msg.Sequence > 0 && msg.Sequence <= m.lastSequence {
			return m, m.watchEvent()
		}
		if m.lastSequence > 0 && msg.Sequence > m.lastSequence+1 {
			return m, m.syncConversation()
		}
		if msg.Sequence > 0 {
			m.lastSequence = msg.Sequence
		}
		event, ok := tuiEventFromProtocol(protocol.SessionEventNotification(msg))
		if !ok {
			return m, m.watchEvent()
		}
		if event.Kind == tui.EventTurnFinished || event.Kind == tui.EventFailure {
			m.state = "idle"
		}
		if event.Kind == tui.EventFailure {
			m.errorText = event.Text
		}
		_, renderCmd := m.ui.Update(event)
		m.syncUIRenderState()
		if event.Kind == tui.EventTurnFinished || event.Kind == tui.EventFailure {
			return m, tea.Batch(renderCmd, m.syncConversation())
		}
		if event.Kind == tui.EventAssistantDelta {
			return m, tea.Batch(renderCmd, m.watchEvent())
		}
		return m, m.watchEvent()
	case permissionRequestMsg:
		if msg.ConversationID == m.conversation && msg.TurnID != "" && msg.RequestID != "" {
			request := protocol.PermissionRequestNotification(msg)
			m.addPendingApproval(request)
			m.state = "awaiting_approval"
		}
		return m, m.watchPermission()
	case permissionRespondedMsg:
		if msg.err != nil {
			m.errorText = msg.err.Error()
		} else if m.resolvePendingApproval(msg.requestID, msg.generation) {
			return m, m.syncConversation()
		} else {
			return m, m.syncConversation()
		}
		return m, nil
	case conversationGapMsg:
		m.client.ClearConversationEventGap()
		return m, m.syncConversation()
	case conversationEvictedMsg:
		if string(msg) != m.conversation {
			return m, m.watchEvent()
		}
		m.state = "restoring"
		return m, m.resumeEvictedConversation(m.nextResumeGeneration(), m.conversation, m.workerEpoch)
	case conversationResumeListMsg:
		if msg.generation == 0 || msg.generation != m.pickerGeneration {
			return m, nil
		}
		if msg.err != nil {
			m.errorText = msg.err.Error()
			_, _ = m.ui.Update(tui.Event{Kind: tui.EventPickerClosed})
			return m, nil
		}
		m.workspaces = msg.workspaces
		if len(msg.workspaces) == 0 {
			m.errorText = "No saved conversations are available. Submit a prompt to start one."
			_, _ = m.ui.Update(tui.Event{Kind: tui.EventPickerClosed})
			return m, nil
		}
		choices := make([]tui.Choice, len(msg.workspaces))
		for i, workspace := range msg.workspaces {
			choices[i] = tui.Choice{ID: workspace.WorkspaceDir, Label: workspace.WorkspaceDir}
		}
		_, _ = m.ui.Update(tui.Event{Kind: tui.EventPickerSnapshot, PickerKind: tui.PickerWorkspace, PickerGeneration: msg.generation, Choices: choices})
	case conversationListMsg:
		if msg.generation == 0 || msg.generation != m.pickerGeneration || m.ui.Picker != tui.PickerWorkspace {
			return m, nil
		}
		if msg.err != nil {
			m.errorText = msg.err.Error()
			_, _ = m.ui.Update(tui.Event{Kind: tui.EventPickerClosed})
			return m, nil
		}
		m.conversations = msg.conversations
		if len(msg.conversations) == 0 {
			m.errorText = "No saved conversations in this project."
			_, _ = m.ui.Update(tui.Event{Kind: tui.EventPickerClosed})
			return m, nil
		}
		choices := make([]tui.Choice, len(msg.conversations))
		for i, conversation := range msg.conversations {
			choices[i] = tui.Choice{ID: conversation.ConversationID, Label: conversation.Task + "  ·  " + conversation.ConversationID}
		}
		_, _ = m.ui.Update(tui.Event{Kind: tui.EventPickerSnapshot, PickerKind: tui.PickerConversation, PickerGeneration: msg.generation, Choices: choices})
	case conversationResumedMsg:
		if (msg.resumeGeneration != 0 && msg.resumeGeneration != m.resumeGeneration) ||
			(msg.sourceConversationID != "" && msg.sourceConversationID != m.conversation) ||
			(msg.sourceWorkerEpoch != "" && msg.sourceWorkerEpoch != m.workerEpoch) ||
			(msg.pickerGeneration != 0 && (msg.pickerGeneration != m.pickerGeneration || m.ui.Picker != tui.PickerConversation)) {
			return m, nil
		}
		if msg.err != nil {
			m.errorText = msg.err.Error()
			if msg.pickerGeneration != 0 {
				_, _ = m.ui.Update(tui.Event{Kind: tui.EventPickerClosed})
			}
			return m, nil
		}
		if msg.pickerGeneration != 0 {
			_, _ = m.ui.Update(tui.Event{Kind: tui.EventPickerClosed})
		}
		if msg.workspace != "" {
			m.workspace = msg.workspace
			m.startParams.WorkspaceDir = msg.workspace
		}
		if msg.databasePath != "" {
			m.databasePath = msg.databasePath
		}
		if msg.updateMCPConfig {
			m.startParams.MCPConfig = msg.mcpConfig
		}
		m.loadSync(msg.result)
		return m, tea.Batch(m.loadGitBranch(), m.watchEvent(), m.watchPermission())
	case conversationSyncMsg:
		if msg.fromWorkerEpoch != "" && msg.fromWorkerEpoch != m.workerEpoch {
			return m, nil
		}
		if msg.err != nil {
			m.errorText = msg.err.Error()
		} else {
			if msg.result != nil && msg.result.ConversationID != m.conversation {
				return m, nil
			}
			m.loadSync(msg.result)
			return m, m.watchEvent()
		}
	case outputPageMsg:
		if msg.generation != m.inspectionGeneration || !m.ui.HasInspection() || msg.ref != m.ui.InspectionRef() || m.ui.InspectionKind() != tui.InspectionOutput {
			return m, nil
		}
		m.outputLoading = false
		if msg.err != nil {
			m.errorText = msg.err.Error()
			_, _ = m.ui.Update(tui.Event{Kind: tui.EventInspectionFailure, InspectionGeneration: msg.generation, InspectionError: msg.err.Error()})
			return m, nil
		}
		pageIndex := indexInt64(m.outputPageStarts, msg.start)
		m.outputPageIndex = pageIndex
		var page *tui.OutputPage
		if msg.result != nil {
			page = &tui.OutputPage{Data: msg.result.Data, Offset: msg.start, TotalBytes: msg.result.TotalBytes, NextOffset: msg.result.NextOffset, HasMore: msg.result.HasMore, Truncated: msg.result.Truncated}
		}
		_, _ = m.ui.Update(tui.Event{Kind: tui.EventOutputPage, InspectionGeneration: msg.generation, OutputPage: page})
		return m, nil
	case diffInspectMsg:
		if msg.generation != m.inspectionGeneration || !m.ui.HasInspection() || m.ui.InspectionKind() != tui.InspectionDiff {
			return m, nil
		}
		if msg.err != nil {
			m.errorText = msg.err.Error()
			_, _ = m.ui.Update(tui.Event{Kind: tui.EventInspectionFailure, InspectionGeneration: msg.generation, InspectionError: msg.err.Error()})
			return m, nil
		}
		_, _ = m.ui.Update(tui.Event{Kind: tui.EventDiffResult, InspectionGeneration: msg.generation, DiffView: toTUIDiffView(msg.result)})
		return m, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.hasPendingApproval() {
			_, cmd := m.ui.Update(msg)
			return m, cmd
		}
		if m.ui.HasPicker() {
			_, cmd := m.ui.Update(msg)
			return m, cmd
		}
		if m.ui.HasInspection() {
			_, cmd := m.ui.Update(msg)
			return m, cmd
		}
		_, cmd := m.ui.Update(msg)
		return m, cmd
	case tea.PasteMsg:
		_, cmd := m.ui.Update(msg)
		return m, cmd
	}
	_, cmd := m.ui.Update(msg)
	return m, cmd
}

func (m *conversationTUI) handlePromptInput(input string) tea.Cmd {
	text := strings.TrimSpace(input)
	if text == "" {
		return nil
	}
	m.errorText = ""
	if err := m.ui.Composer.AddHistory(text); err != nil {
		m.errorText = fmt.Sprintf("save prompt history: %v", err)
	}
	if text == "/resume" {
		return m.listWorkspaces(m.nextPickerGeneration())
	}
	m.pickerGeneration++
	if text == "/help" {
		var help strings.Builder
		for _, command := range m.ui.Composer.Commands.Commands() {
			help.WriteString(command.Name + " " + command.ArgsHint + "  " + command.Description + "\n")
		}
		m.appendBlock(TranscriptBlock{ID: newConversationID("block"), Kind: BlockSystemStatus, Role: "system", Header: "Help", Body: strings.TrimSpace(help.String()), CreatedAt: time.Now()})
		return nil
	}
	if text == "/diff" {
		if m.conversation == "" {
			m.errorText = "Start or resume a conversation before opening /diff."
			return nil
		}
		return m.inspectDiff()
	}
	if text == "/interrupt" {
		if m.conversation == "" {
			m.errorText = "No active conversation."
			return nil
		}
		return tui.EmitAction(tui.Action{Kind: tui.ActionInterruptTurn})
	}
	if text == "/copy" {
		var full strings.Builder
		for _, block := range m.ui.Transcript.Blocks {
			if full.Len() > 0 {
				full.WriteString("\n\n")
			}
			full.WriteString(block.Body)
		}
		m.appendBlock(TranscriptBlock{ID: newConversationID("block"), Kind: BlockAssistantMessage, Role: "assistant", Header: "Buddy", Body: "Requested clipboard copy of full transcript.", CreatedAt: time.Now()})
		return tea.SetClipboard(full.String())
	}
	if text == "/quit" || text == "/exit" {
		return tea.Quit
	}
	if text == "/clear" {
		var retained []TranscriptBlock
		if m.ui.ActiveBlockID() != "" {
			for _, block := range m.ui.Transcript.Blocks {
				if block.ID == m.ui.ActiveBlockID() {
					retained = append(retained, block)
					break
				}
			}
			if len(retained) == 0 {
				m.ui.ClearActiveBlock()
			}
		}
		m.ui.Transcript.Replace(retained)
		m.ui.Transcript.ScrollOffset = 0
		return nil
	}
	if strings.HasPrefix(text, "/steer ") {
		return tui.EmitAction(tui.Action{Kind: tui.ActionSubmitPrompt, Prompt: strings.TrimSpace(strings.TrimPrefix(text, "/steer ")), Mode: string(persistence.SubmissionSteer)})
	}
	if strings.HasPrefix(text, "/queue ") {
		return tui.EmitAction(tui.Action{Kind: tui.ActionSubmitPrompt, Prompt: strings.TrimSpace(strings.TrimPrefix(text, "/queue ")), Mode: string(persistence.SubmissionQueue)})
	}
	return tui.EmitAction(tui.Action{Kind: tui.ActionSubmitPrompt, Prompt: text, Mode: string(persistence.SubmissionQueue)})
}

func (m *conversationTUI) View() tea.View {
	m.ensureUIModel()
	width, height := max(1, m.ui.Width), max(1, m.ui.Height)
	emergency := width < 40 || height < 12
	standard := width >= 80 && height >= 24
	var header []string
	welcomeScreen := !m.ui.HasPicker() && !m.ui.HasInspection() && !m.hasPendingApproval() &&
		len(m.ui.Transcript.Blocks) == 0 && m.ui.StreamingText() == "" && m.state != "running"
	if !emergency && !welcomeScreen {
		header = m.headerLines(standard)
	}
	footer := m.renderBottom(emergency, standard)
	status := "Connected"
	if m.standalone {
		status = "Standalone"
	}
	if m.errorText != "" {
		status = "Needs attention"
	}
	m.ui.SetScreenContent(tui.ScreenContent{Header: header, Footer: footer, WelcomeStatus: status, Version: tui.BuildVersion()})
	return m.ui.View()
}

func (m *conversationTUI) headerLines(standard bool) []string {
	return tui.RenderHeader(tui.Header{
		Width: m.ui.Width, Model: terminalText(m.startParams.Model), Workspace: terminalText(m.workspace),
		GitBranch: terminalText(m.gitBranch), State: terminalText(m.state), Standard: standard,
	})
}

func (m *conversationTUI) renderBottom(emergency, standard bool) []string {
	queued, steering := len(m.queued), len(m.steers)
	for _, submission := range m.pendingStart {
		if submission.mode == persistence.SubmissionSteer {
			steering++
		} else {
			queued++
		}
	}
	var approvalView, approvalKeys string
	if m.hasPendingApproval() {
		approvalKeys = m.ui.ApprovalActionRow()
		modalHeight := min(max(2, m.ui.Height/2), max(2, m.ui.Height))
		approvalView = m.ui.ApprovalView(modalHeight)
	}
	composer := m.renderTextarea()
	var renderedError string
	if m.errorText != "" {
		renderedError = errStyle.Render(terminalText(m.errorText))
	}
	return tui.RenderFooter(tui.Footer{
		Width: m.ui.Width, Height: m.ui.Height, Emergency: emergency, Standard: standard,
		PendingApproval: m.hasPendingApproval(), ApprovalView: approvalView, ApprovalKeys: approvalKeys,
		Queued: queued, Steering: steering, Error: renderedError,
		HasError: m.errorText != "", Completion: m.completionLines(), Composer: composer,
		State: m.state, Standalone: m.standalone, TranscriptFocused: m.ui.TranscriptFocused,
	})
}

func (m *conversationTUI) completionLines() []string {
	return m.ui.Composer.CompletionView(m.completionRows())
}

func (m *conversationTUI) start(prompt string) tea.Cmd {
	params := m.startParams
	params.StartID, params.SubmissionID = newConversationID("start"), newConversationID("submission")
	params.Prompt = prompt
	return func() tea.Msg {
		result, err := m.client.StartConversation(params)
		if err != nil {
			return conversationStartedMsg{err: err}
		}
		return conversationStartedMsg{result: result}
	}
}

func (m *conversationTUI) loadGitBranch() tea.Cmd {
	workspace := m.workspace
	return func() tea.Msg {
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		branch, err := exec.CommandContext(ctx, "git", "-C", workspace, "branch", "--show-current").Output()
		if err != nil {
			return gitBranchMsg{workspace: workspace}
		}
		return gitBranchMsg{workspace: workspace, branch: strings.TrimSpace(string(branch))}
	}
}

func (m *conversationTUI) readOutputPage(ref string, offset int64, generation uint64) tea.Cmd {
	conversationID := m.conversation
	return func() tea.Msg {
		result, err := m.client.ReadOutput(protocol.OutputReadParams{ConversationID: conversationID, OutputRef: ref, Offset: offset})
		return outputPageMsg{generation: generation, ref: ref, start: offset, result: result, err: err}
	}
}

func (m *conversationTUI) changeOutputPage(delta int) tea.Cmd {
	if m.outputLoading || len(m.outputPageStarts) == 0 {
		return nil
	}
	index := m.outputPageIndex + delta
	if index < 0 {
		return nil
	}
	if index < len(m.outputPageStarts) {
		m.outputLoading = true
		_, _ = m.ui.Update(tui.Event{Kind: tui.EventInspectionPageLoading, InspectionGeneration: m.inspectionGeneration})
		return m.readOutputPage(m.ui.InspectionRef(), m.outputPageStarts[index], m.inspectionGeneration)
	}
	page := m.ui.CurrentOutputPage()
	if delta > 0 && page != nil && page.HasMore {
		offset := page.NextOffset
		m.outputPageStarts = append(m.outputPageStarts, offset)
		m.outputLoading = true
		_, _ = m.ui.Update(tui.Event{Kind: tui.EventInspectionPageLoading, InspectionGeneration: m.inspectionGeneration})
		return m.readOutputPage(m.ui.InspectionRef(), offset, m.inspectionGeneration)
	}
	return nil
}

func indexInt64(values []int64, value int64) int {
	for i, v := range values {
		if v == value {
			return i
		}
	}
	return 0
}

func (m *conversationTUI) inspectDiff() tea.Cmd {
	m.ensureUIModel()
	conversationID := m.conversation
	m.inspectionGeneration++
	generation := m.inspectionGeneration
	_, _ = m.ui.Update(tui.Event{Kind: tui.EventInspectionStart, InspectionKind: tui.InspectionDiff, InspectionGeneration: generation})
	return func() tea.Msg {
		result, err := m.client.InspectDiff(protocol.DiffInspectParams{ConversationID: conversationID})
		return diffInspectMsg{generation: generation, result: result, err: err}
	}
}

func toTUIDiffView(result *protocol.DiffInspectResult) *tui.DiffView {
	if result == nil {
		return &tui.DiffView{}
	}
	view := &tui.DiffView{Stats: tui.DiffStats{FilesChanged: result.Stats.FilesChanged, Additions: result.Stats.Additions, Deletions: result.Stats.Deletions}, Warning: result.Warning, OmittedPaths: result.OmittedPaths, Truncated: result.Truncated}
	view.Files = make([]tui.DiffFile, len(result.Files))
	for i, file := range result.Files {
		view.Files[i] = tui.DiffFile{Path: file.Path, Diff: file.Diff, WasDirtyAtStart: file.WasDirtyAtStart, IsBuddyEdit: file.IsBuddyEdit, AttributionUnknown: file.AttributionUnknown, IsBinary: file.IsBinary, Truncated: file.Truncated}
	}
	return view
}

func (m *conversationTUI) submit(prompt string, mode persistence.SubmissionMode) tea.Cmd {
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	if m.conversation == "" {
		if m.startPending {
			m.pendingStart = append(m.pendingStart, pendingStartSubmission{prompt: prompt, mode: mode})
			return nil
		}
		m.startPending = true
		return m.start(prompt)
	}
	if len(m.pendingStart) > 0 || m.pendingStartInFlight {
		m.pendingStart = append(m.pendingStart, pendingStartSubmission{prompt: prompt, mode: mode})
		return m.submitNextPendingStart()
	}
	params := protocol.ConversationSubmitParams{ConversationID: m.conversation, SubmissionID: newConversationID("submission"), Prompt: prompt, Mode: string(mode)}
	return func() tea.Msg {
		result, err := m.client.SubmitConversation(params)
		return conversationSubmittedMsg{result: result, prompt: prompt, mode: mode, err: err}
	}
}

func (m *conversationTUI) interruptTurn() tea.Cmd {
	if m.conversation == "" {
		return func() tea.Msg { return errorMessage(fmt.Errorf("no active conversation")) }
	}
	return func() tea.Msg {
		result, err := m.client.SyncConversation(protocol.ConversationSyncParams{ConversationID: m.conversation})
		if err != nil {
			return errorMessage(err)
		}
		if result.ActiveTurnID == "" {
			return errorMessage(fmt.Errorf("no active turn to interrupt"))
		}
		_, err = m.client.InterruptTurn(protocol.TurnInterruptParams{ConversationID: m.conversation, TurnID: result.ActiveTurnID})
		return errorMessage(err)
	}
}

func (m *conversationTUI) submitNextPendingStart() tea.Cmd {
	if m.conversation == "" || m.pendingStartInFlight || len(m.pendingStart) == 0 {
		return nil
	}
	submission := m.pendingStart[0]
	params := protocol.ConversationSubmitParams{ConversationID: m.conversation, SubmissionID: newConversationID("submission"), Prompt: submission.prompt, Mode: string(submission.mode)}
	m.pendingStartInFlight = true
	return func() tea.Msg {
		result, err := m.client.SubmitConversation(params)
		return conversationSubmittedMsg{result: result, prompt: submission.prompt, mode: submission.mode, pendingStart: true, err: err}
	}
}

func (m *conversationTUI) watchEvent() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
			return nil
		case event, ok := <-m.client.ConversationEvents():
			if !ok {
				return conversationSyncMsg{err: fmt.Errorf("conversation event stream closed")}
			}
			return conversationEventMsg(event)
		case <-m.client.ConversationEventGaps():
			return conversationGapMsg{}
		case conversationID := <-m.client.ConversationEvictions():
			return conversationEvictedMsg(conversationID)
		}
	}
}

func (m *conversationTUI) watchPermission() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.ctx.Done():
			return nil
		case request, ok := <-m.client.PermissionRequests():
			if !ok {
				return nil
			}
			return permissionRequestMsg(request)
		}
	}
}

func (m *conversationTUI) hasPendingApproval() bool {
	return len(m.pendingApprovals) > 0
}

func (m *conversationTUI) activeApproval() *pendingApprovalRequest {
	if len(m.pendingApprovals) == 0 {
		return nil
	}
	return &m.pendingApprovals[0]
}

func (m *conversationTUI) approvalForGeneration(generation uint64) *pendingApprovalRequest {
	active := m.activeApproval()
	if active == nil || generation == 0 || active.generation != generation {
		return nil
	}
	return active
}

func (m *conversationTUI) addPendingApproval(request protocol.PermissionRequestNotification) {
	index := -1
	for i := range m.pendingApprovals {
		if m.pendingApprovals[i].request.RequestID == request.RequestID {
			index = i
			break
		}
	}
	var generation uint64
	if index >= 0 && m.pendingApprovals[index].request.ConversationID == request.ConversationID && m.pendingApprovals[index].request.TurnID == request.TurnID {
		m.pendingApprovals[index].request = request
	} else {
		m.approvalGeneration++
		generation = m.approvalGeneration
		pending := pendingApprovalRequest{request: request, generation: generation}
		if index >= 0 {
			m.pendingApprovals[index] = pending
		} else {
			m.pendingApprovals = append(m.pendingApprovals, pending)
		}
	}
	requests := make([]protocol.PermissionRequestNotification, len(m.pendingApprovals))
	for i := range m.pendingApprovals {
		requests[i] = m.pendingApprovals[i].request
	}
	m.setPendingApprovals(requests)
}

func (m *conversationTUI) resolvePendingApproval(requestID string, generation uint64) bool {
	index := -1
	for i, pending := range m.pendingApprovals {
		if pending.request.RequestID == requestID && pending.generation == generation {
			index = i
			continue
		}
	}
	if index < 0 {
		return false
	}
	copy(m.pendingApprovals[index:], m.pendingApprovals[index+1:])
	m.pendingApprovals = m.pendingApprovals[:len(m.pendingApprovals)-1]
	_, _ = m.ui.Update(tui.Event{Kind: tui.EventApprovalResolved, ApprovalGeneration: generation})
	if !m.hasPendingApproval() && !m.ui.TranscriptFocused {
		m.ui.Composer.Editor.Focus()
	}
	return true
}

func (m *conversationTUI) setPendingApprovals(requests []protocol.PermissionRequestNotification) {
	wasPending := m.hasPendingApproval()
	type approvalIdentity struct{ requestID, conversationID, turnID string }
	previous := make(map[approvalIdentity]uint64, len(m.pendingApprovals))
	for _, pending := range m.pendingApprovals {
		request := pending.request
		previous[approvalIdentity{request.RequestID, request.ConversationID, request.TurnID}] = pending.generation
	}
	next := make([]pendingApprovalRequest, 0, len(requests))
	visible := make([]tui.ApprovalRequest, 0, len(requests))
	for _, request := range requests {
		if request.RequestID == "" || request.ConversationID == "" || request.TurnID == "" {
			continue
		}
		generation := previous[approvalIdentity{request.RequestID, request.ConversationID, request.TurnID}]
		if generation == 0 {
			m.approvalGeneration++
			generation = m.approvalGeneration
		}
		next = append(next, pendingApprovalRequest{request: request, generation: generation})
		visible = append(visible, approvalViewRequest(request, generation))
	}
	m.pendingApprovals = next
	_, _ = m.ui.Update(tui.Event{Kind: tui.EventApprovalSnapshot, Approvals: visible})
	if m.hasPendingApproval() && !m.ui.TranscriptFocused {
		m.ui.Composer.Editor.Blur()
	} else if wasPending && !m.hasPendingApproval() && !m.ui.TranscriptFocused {
		m.ui.Composer.Editor.Focus()
	}
}

func (m *conversationTUI) respondPermission(request *pendingApprovalRequest, params protocol.PermissionRespondParams) tea.Cmd {
	if request == nil {
		return nil
	}
	requestID := request.request.RequestID
	generation := request.generation
	params.RequestID, params.ConversationID, params.TurnID = request.request.RequestID, request.request.ConversationID, request.request.TurnID
	return func() tea.Msg {
		err := m.client.RespondConversationPermissionParams(params)
		return permissionRespondedMsg{requestID: requestID, generation: generation, err: err}
	}
}

func (m *conversationTUI) syncConversation() tea.Cmd {
	id, epoch := m.conversation, m.workerEpoch
	return func() tea.Msg {
		result, err := readConversationSnapshot(m.client, id)
		return conversationSyncMsg{result: result, fromWorkerEpoch: epoch, err: err}
	}
}

func (m *conversationTUI) loadSync(result *protocol.ConversationSyncResult) {
	m.ensureUIModel()
	m.ui.ResetStreaming()
	if result == nil {
		m.setPendingApprovals(nil)
		return
	}
	activitiesByTurn := make(map[string][]TranscriptBlock)
	var activityTurns []string
	if m.conversation == result.ConversationID && m.workerEpoch == result.WorkerEpoch {
		for _, block := range m.ui.Transcript.Blocks {
			if block.Kind != BlockToolActivity && block.Kind != BlockSubagentActivity && block.Kind != BlockActivityGroup {
				continue
			}
			if len(activitiesByTurn[block.TurnID]) == 0 {
				activityTurns = append(activityTurns, block.TurnID)
			}
			activitiesByTurn[block.TurnID] = append(activitiesByTurn[block.TurnID], block)
		}
	} else {
		m.ui.SelectedActivity = ""
		m.inspectionGeneration++
		m.outputPageStarts, m.outputPageIndex, m.outputLoading = nil, 0, false
		_, _ = m.ui.Update(tui.Event{Kind: tui.EventInspectionClosed})
	}
	m.conversation, m.state = result.ConversationID, result.State
	m.workerEpoch = result.WorkerEpoch
	if result.ExecutionBlocked != "" {
		m.errorText = "Execution is paused: " + result.ExecutionBlocked
	} else if strings.HasPrefix(m.errorText, "Execution is paused: ") {
		m.errorText = ""
	}
	m.setPendingApprovals(result.PendingApprovals)
	m.lastSequence = result.EventWatermark
	restoredBlocks := make([]TranscriptBlock, 0, len(result.Transcript))
	m.queued, m.steers = nil, nil
	if result.OutputIncomplete {
		m.errorText = "Some transient output is unavailable; restored from durable conversation history."
	}
	// Keep transient activity groups next to their turn's last durable entry.
	lastEntry := make(map[string]int, len(activitiesByTurn))
	for i, entry := range result.Transcript {
		if entry.TurnID != "" && len(activitiesByTurn[entry.TurnID]) > 0 {
			lastEntry[entry.TurnID] = i
		}
	}
	// Replace the canonical blocks together so recovery keeps the reading offset.
	for i, entry := range result.Transcript {
		last, hasActivities := lastEntry[entry.TurnID]
		if hasActivities && last == i && entry.Role == "assistant" {
			restoredBlocks = append(restoredBlocks, activitiesByTurn[entry.TurnID]...)
		}
		if entry.Role == "user" {
			restoredBlocks = append(restoredBlocks, TranscriptBlock{
				ID:        newConversationID("block"),
				TurnID:    entry.TurnID,
				Kind:      BlockUserMessage,
				Role:      "user",
				Header:    "You",
				Body:      entry.Content,
				CreatedAt: time.Now(),
			})
			if entry.Status == string(persistence.SubmissionQueued) {
				m.queued = append(m.queued, entry.Content)
			}
			if entry.Status == string(persistence.SubmissionSteerPending) {
				m.steers = append(m.steers, entry.Content)
			}
		}
		if entry.Role == "assistant" {
			if strings.TrimSpace(entry.Content) == "" {
				continue
			}
			restoredBlocks = append(restoredBlocks, TranscriptBlock{
				ID:        newConversationID("block"),
				TurnID:    entry.TurnID,
				Kind:      BlockAssistantMessage,
				Role:      "assistant",
				Header:    "Buddy",
				Body:      entry.Content,
				CreatedAt: time.Now(),
			})
		}
		if hasActivities && last == i && entry.Role != "assistant" {
			restoredBlocks = append(restoredBlocks, activitiesByTurn[entry.TurnID]...)
		}
	}
	for _, turnID := range activityTurns {
		if _, found := lastEntry[turnID]; !found {
			restoredBlocks = append(restoredBlocks, activitiesByTurn[turnID]...)
		}
	}
	_, _ = m.ui.Update(tui.Event{Kind: tui.EventSnapshot, Blocks: restoredBlocks})
	m.syncUIRenderState()
}

func (m *conversationTUI) renderTextarea() string {
	defer func() {
		_ = recover()
	}()
	return m.ui.Composer.Editor.View()
}

func (m *conversationTUI) viewportHeight() int {
	return max(0, m.ui.Height-m.viewportOverhead()-m.completionRows())
}

func (m *conversationTUI) viewportOverhead() int {
	width, height := max(1, m.ui.Width), max(1, m.ui.Height)
	emergency := width < 40 || height < 12
	standard := width >= 80 && height >= 24
	headerHeight := 0
	if !emergency {
		headerHeight = 1
		if standard {
			headerHeight = 2
		}
	}
	composerHeight := max(1, m.ui.Composer.Editor.Height())
	if standard && width >= 40 {
		composerHeight += 2
	}
	footerHeight := 0
	if !emergency {
		footerHeight = 1
	}
	overhead := headerHeight + footerHeight
	if m.hasPendingApproval() && emergency {
		if height <= 1 {
			return 1
		}
		modalHeight := min(max(2, height/2), max(2, height))
		return lipgloss.Height(m.ui.ApprovalView(modalHeight))
	}
	if !m.hasPendingApproval() || !emergency {
		overhead += composerHeight
	}
	if len(m.queued)+len(m.steers)+len(m.pendingStart) > 0 {
		overhead++
	}
	if m.hasPendingApproval() {
		if height <= 1 {
			overhead++
		} else {
			modalHeight := min(max(2, height/2), max(2, height))
			overhead += lipgloss.Height(m.ui.ApprovalView(modalHeight))
		}
	}
	if m.errorText != "" {
		overhead++
	}
	return overhead
}

func (m *conversationTUI) completionRows() int {
	if m.ui.Composer.CompletionKind() == "" {
		return 0
	}
	return min(8, min(m.ui.Composer.CompletionCount(), max(0, m.ui.Height-m.viewportOverhead()-4)))
}

func (m *conversationTUI) ensureUIModel() {
	if m.ui == nil {
		m.ui = tui.NewModel(tui.Header{Model: m.startParams.Model, Workspace: m.workspace})
	}
	if m.ui.Composer == nil {
		m.ui.Composer = tui.NewComposer(nil)
	}
	m.ui.Metadata.Model = m.startParams.Model
	m.ui.Metadata.Workspace = m.workspace
	m.ui.Metadata.GitBranch = m.gitBranch
	m.ui.State = m.state
}

func (m *conversationTUI) syncUIRenderState() {
	m.ensureUIModel()
	m.ui.Metadata.Model = m.startParams.Model
	m.ui.Metadata.Workspace = m.workspace
	m.ui.Metadata.GitBranch = m.gitBranch
	m.ui.State = m.state
	m.ui.Transcript.SetRenderState(m.ui.StreamingText(), m.ui.SelectedActivity, m.ui.TranscriptFocused)
}

func (m *conversationTUI) clearCache() { m.ensureUIModel(); m.ui.Transcript.ClearCache() }

func (m *conversationTUI) appendBlock(block TranscriptBlock) {
	m.ensureUIModel()
	if block.ID == "" {
		block.ID = newConversationID("block")
	}
	m.setTranscriptRenderState()
	m.ui.Transcript.Append(block, m.ui.Width)
}

func (m *conversationTUI) getWrappedLines(block TranscriptBlock, width int) []string {
	m.ensureUIModel()
	m.setTranscriptRenderState()
	return m.ui.Transcript.WrappedLines(block, width)
}

func (m *conversationTUI) renderBlock(block TranscriptBlock, width int) (string, int) {
	m.ensureUIModel()
	m.setTranscriptRenderState()
	return tui.RenderBlock(block, m.ui.Transcript.Blocks, width, m.ui.StreamingText(), m.ui.SelectedActivity, m.ui.TranscriptFocused)
}

func (m *conversationTUI) setTranscriptRenderState() {
	m.ensureUIModel()
	m.ui.Transcript.SetRenderState(m.ui.StreamingText(), m.ui.SelectedActivity, m.ui.TranscriptFocused)
}

func terminalText(value string) string {
	value = ansi.Strip(value)
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
}

func tuiEventFromProtocol(msg protocol.SessionEventNotification) (tui.Event, bool) {
	event := tui.Event{TurnID: msg.TurnID, At: time.Now()}
	switch msg.Type {
	case string(agent.EventFinished):
		event.Kind = tui.EventTurnFinished
	case string(agent.EventError):
		event.Kind, event.Text = tui.EventFailure, msg.Content
	case string(agent.EventTextDelta):
		event.Kind, event.Text = tui.EventAssistantDelta, msg.Content
	case string(agent.EventToolCall):
		event.Kind, event.Activity = tui.EventActivityStarted, activityFromEvent(msg.Data, msg.TurnID)
	case string(agent.EventToolResult):
		event.Kind, event.Activity, event.Text = tui.EventActivityCompleted, activityFromEvent(msg.Data, msg.TurnID), msg.Content
		if msg.Data != nil {
			if result, ok := msg.Data["tool_result"].(string); ok && result != "" {
				event.Text = result
			}
		}
	case string(agent.EventReasoning):
		event.Kind, event.Text, event.Activity = tui.EventReasoning, msg.Content, activityFromEvent(msg.Data, msg.TurnID)
	case string(agent.EventTextComplete):
		event.Kind, event.Text, event.Activity = tui.EventAssistantComplete, msg.Content, activityFromEvent(msg.Data, msg.TurnID)
	case string(agent.EventCompactionStart):
		event.Kind, event.Text = tui.EventCompactionStatus, "Compacting conversation context…"
	default:
		return tui.Event{}, false
	}
	return event, true
}

func activityDataKey(data map[string]any) string {
	if data == nil {
		return ""
	}
	id, ok := data["activity_id"].(map[string]any)
	if !ok {
		return ""
	}
	return activityIdentityKey(id)
}

func activityFromEvent(data map[string]any, _ string) *tui.Activity {
	step := eventInt(data["step"])
	if step <= 0 {
		if id, ok := data["activity_id"].(map[string]any); ok {
			step = eventInt(id["step"])
		}
	}
	activity := &tui.Activity{Identity: activityDataKey(data), Step: step, StartedAt: time.Now()}
	if data == nil {
		return activity
	}
	activity.Tool, _ = data["tool_name"].(string)
	activity.AgentName, _ = data["agent_name"].(string)
	activity.Path, _ = data["path"].(string)
	if activity.Path == "" {
		activity.Path, _ = data["file_path"].(string)
	}
	activity.Line = eventInt(data["line"])
	activity.OutputRef, _ = data["output_ref"].(string)
	activity.OutputBytes = int64(eventInt(data["output_bytes"]))
	activity.OutputTruncated, _ = data["output_truncated"].(bool)
	if duration := eventInt(data["duration_ms"]); duration > 0 {
		activity.Elapsed = time.Duration(duration) * time.Millisecond
	}
	if exitCode, ok := data["exit_code"]; ok {
		code := eventInt(exitCode)
		activity.ExitCode = &code
	}
	if parent, ok := data["parent_activity_id"].(map[string]any); ok {
		activity.ParentIdentity = activityIdentityKey(parent)
	}
	return activity
}

func eventInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func activityIdentityKey(id map[string]any) string {
	if id == nil {
		return ""
	}
	turn, _ := id["turn_id"].(string)
	step := eventInt(id["step"])
	call, _ := id["tool_call_id"].(string)
	if turn == "" || call == "" {
		return ""
	}
	return turn + "\x00" + strconv.Itoa(step) + "\x00" + call
}

func (m *conversationTUI) computeVisibleBlocks() []TranscriptBlock {
	m.ensureUIModel()
	m.setTranscriptRenderState()
	return m.ui.Transcript.VisibleBlocks(m.viewportHeight(), m.ui.Width)
}

func (m *conversationTUI) computeVisibleLines() []string {
	m.ensureUIModel()
	m.setTranscriptRenderState()
	return m.ui.Transcript.VisibleLines(m.viewportHeight(), m.ui.Width)
}

func (m *conversationTUI) nextPickerGeneration() uint64 {
	m.pickerGeneration++
	return m.pickerGeneration
}

func (m *conversationTUI) nextResumeGeneration() uint64 {
	m.resumeGeneration++
	return m.resumeGeneration
}

func (m *conversationTUI) listWorkspaces(generation uint64) tea.Cmd {
	return func() tea.Msg {
		result, err := m.client.ListConversationWorkspaces(50)
		if err != nil {
			return conversationResumeListMsg{generation: generation, err: err}
		}
		return conversationResumeListMsg{generation: generation, workspaces: result.Workspaces}
	}
}

func (m *conversationTUI) listConversations(workspace string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		result, err := m.client.ListConversations(protocol.ConversationListParams{WorkspaceDir: workspace, Limit: 50})
		if err != nil {
			return conversationListMsg{generation: generation, err: err}
		}
		return conversationListMsg{generation: generation, conversations: result.Conversations}
	}
}

func (m *conversationTUI) resumeSelected(selected protocol.ConversationSummary, pickerGeneration, resumeGeneration uint64) tea.Cmd {
	workspace := m.selectedWorkspace
	startParams := m.startParams
	loadMCPConfig := m.loadWorkspaceMCPConfig
	api := m.client
	return func() tea.Msg {
		resumeParams, mcpConfig, err := conversationResumeParamsForWorkspace(workspace, selected.ConversationID, selected.DBPath, startParams, loadMCPConfig)
		if err != nil {
			return conversationResumedMsg{pickerGeneration: pickerGeneration, resumeGeneration: resumeGeneration, err: err}
		}
		_, err = api.ResumeConversation(resumeParams)
		if err != nil {
			return conversationResumedMsg{pickerGeneration: pickerGeneration, resumeGeneration: resumeGeneration, err: err}
		}
		result, err := readConversationSnapshot(api, selected.ConversationID)
		return conversationResumedMsg{
			pickerGeneration: pickerGeneration, resumeGeneration: resumeGeneration, result: result,
			workspace: workspace, databasePath: selected.DBPath, mcpConfig: mcpConfig, updateMCPConfig: true, err: err,
		}
	}
}

func (m *conversationTUI) resumeEvictedConversation(resumeGeneration uint64, sourceConversationID, sourceWorkerEpoch string) tea.Cmd {
	workspace, conversationID, databasePath := m.workspace, m.conversation, m.databasePath
	startParams := m.startParams
	api := m.client
	return func() tea.Msg {
		resumeParams := conversationResumeParams(workspace, conversationID, startParams)
		resumeParams.DBPath = databasePath
		_, err := api.ResumeConversation(resumeParams)
		if err != nil {
			return conversationResumedMsg{resumeGeneration: resumeGeneration, sourceConversationID: sourceConversationID, sourceWorkerEpoch: sourceWorkerEpoch, err: err}
		}
		result, err := readConversationSnapshot(api, conversationID)
		return conversationResumedMsg{
			resumeGeneration: resumeGeneration, sourceConversationID: sourceConversationID, sourceWorkerEpoch: sourceWorkerEpoch, result: result,
			workspace: workspace, databasePath: databasePath, mcpConfig: startParams.MCPConfig, updateMCPConfig: true, err: err,
		}
	}
}

func (m *conversationTUI) resumeParamsForWorkspace(workspace, conversationID, dbPath string) (protocol.ConversationResumeParams, *mcp.Config, error) {
	return conversationResumeParamsForWorkspace(workspace, conversationID, dbPath, m.startParams, m.loadWorkspaceMCPConfig)
}

func conversationResumeParamsForWorkspace(workspace, conversationID, dbPath string, start protocol.ConversationStartParams, loadMCPConfig func(string) (*mcp.Config, error)) (protocol.ConversationResumeParams, *mcp.Config, error) {
	params := conversationResumeParams(workspace, conversationID, start)
	params.DBPath = dbPath
	mcpConfig := start.MCPConfig
	if loadMCPConfig != nil {
		loaded, err := loadMCPConfig(workspace)
		if err != nil {
			return params, nil, fmt.Errorf("load MCP configuration for workspace %q: %w", workspace, err)
		}
		mcpConfig = loaded
	} else if workspace != start.WorkspaceDir {
		// The current config was resolved for another project. Never send it to
		// the selected workspace if no workspace loader is available.
		mcpConfig = nil
	}
	params.MCPConfig = mcpConfig
	return params, mcpConfig, nil
}

func conversationResumeParams(workspace, conversationID string, start protocol.ConversationStartParams) protocol.ConversationResumeParams {
	return protocol.ConversationResumeParams{
		WorkspaceDir: workspace, ConversationID: conversationID, DBPath: start.DBPath,
		Model: start.Model, BaseURL: start.BaseURL, APIType: start.APIType, APIKey: start.APIKey,
		ReasoningEffort: start.ReasoningEffort, MaxSteps: start.MaxSteps, MaxContextTokens: start.MaxContextTokens,
		MaxOutputChars: start.MaxOutputChars, WorkingTailSize: start.WorkingTailSize, MCPConfig: start.MCPConfig, Env: start.Env,
	}
}

func readConversationSnapshot(cl conversationAPI, conversationID string) (*protocol.ConversationSyncResult, error) {
	result, err := cl.SyncConversation(protocol.ConversationSyncParams{ConversationID: conversationID, PageSize: 100})
	if err != nil {
		return nil, err
	}
	for result.NextPageToken != "" {
		page, err := cl.SyncConversation(protocol.ConversationSyncParams{
			ConversationID:  conversationID,
			HistoryRevision: result.HistoryRevision,
			PageToken:       result.NextPageToken,
			PageSize:        100,
		})
		if err != nil {
			return nil, err
		}
		result.Transcript = append(result.Transcript, page.Transcript...)
		result.NextPageToken = page.NextPageToken
	}
	return result, nil
}

func newConversationID(prefix string) string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, timeNowUnixNano())
	}
	return prefix + "_" + hex.EncodeToString(bytes[:])
}

func timeNowUnixNano() int64 { return time.Now().UnixNano() }

func errorMessage(err error) tea.Msg {
	if err == nil {
		return conversationSyncMsg{}
	}
	return conversationSyncMsg{err: err}
}

func buildConversationStartParams(f *cmdutil.Factory) (protocol.ConversationStartParams, string, error) {
	cfg, err := f.ConfigLoader()
	if err != nil {
		return protocol.ConversationStartParams{}, "", fmt.Errorf("load configuration: %w", err)
	}
	workspace := "."
	params := protocol.ConversationStartParams{}
	if cfg != nil {
		if cfg.WorkspaceDir != "" {
			workspace = cfg.WorkspaceDir
		}
		params.Model, params.MaxSteps = cfg.Model, cfg.MaxSteps
		params.MaxContextTokens, params.MaxOutputChars = cfg.MaxContextTokens, cfg.MaxOutputChars
		params.WorkingTailSize = cfg.WorkingTailSize
		reasoning := cfg.ReasoningEffort
		params.ReasoningEffort = &reasoning
		params.BaseURL, params.APIType, params.DBPath = cfg.BaseURL, cfg.APIType, cfg.DBPath
		// The daemon resolves its configured credential. Do not send API keys on
		// ordinary starts; explicit API clients may supply an ephemeral key.
	}
	params.WorkspaceDir = workspace
	if params.DBPath != "" && params.DBPath != ":memory:" && !strings.HasPrefix(params.DBPath, "file:") && !filepath.IsAbs(params.DBPath) && f.WorkDir != nil {
		if cwd, cwdErr := f.WorkDir(); cwdErr == nil && cwd != "" {
			params.DBPath = filepath.Clean(filepath.Join(cwd, params.DBPath))
		}
	}
	if f.WorkDir != nil && f.Environ != nil {
		if cwd, cwdErr := f.WorkDir(); cwdErr == nil && cwd != "" {
			params.Env, _ = absolutizePATHEntries(f.Environ(), cwd)
		}
	}
	if f.MCPConfigLoader != nil {
		home, _ := f.UserHome()
		params.MCPConfig, _ = f.MCPConfigLoader(workspace, home)
	}
	return params, workspace, nil
}
