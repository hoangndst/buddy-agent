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

	"charm.land/bubbles/v2/key"
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
	scrollOffset           int
	blocks                 []TranscriptBlock
	lineCountCache         map[string]int
	wrapCache              map[string][]string
	deltaBuffer            strings.Builder
	tickerActive           bool
	activeBlockID          string
	streamingText          string
	pendingApproval        *protocol.PermissionRequestNotification
	approvalModal          *approvalModalState
	queued                 []string
	steers                 []string
	textarea               textarea.Model
	history                *tui.InputHistoryRing
	slashCommands          tui.SlashCommandRegistry
	completionKind         string
	commandMatches         []tui.SlashCommand
	fileMatches            []string
	completionCursor       int
	completionStart        int
	completionEnd          int
	width                  int
	height                 int
	errorText              string
	choosing               string
	choices                []string
	cursor                 int
	workspaces             []protocol.ConversationWorkspace
	conversations          []protocol.ConversationSummary
	selectedWorkspace      string
	loadWorkspaceMCPConfig func(string) (*mcp.Config, error)
	transcriptFocused      bool
	selectedActivity       string
	outputRef              string
	outputPage             *protocol.OutputReadResult
	outputPageStarts       []int64
	outputPageIndex        int
	outputLineScroll       int
	outputLoading          bool
	diffView               *protocol.DiffInspectResult
	diffLoading            bool
	diffRequestGeneration  uint64
	diffSelected           int
	diffFileOpen           bool
	diffScroll             int
	inspectionColumn       int
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
	// BlockSystemStatus identifies a system notice.
	BlockSystemStatus = tui.BlockSystemStatus
)

// TranscriptBlock is the canonical rendered conversation entry.
type TranscriptBlock = tui.TranscriptBlock

type renderTickMsg time.Time

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
	requestID string
	err       error
}
type conversationResumeListMsg struct {
	workspaces []protocol.ConversationWorkspace
	err        error
}
type conversationListMsg struct {
	conversations []protocol.ConversationSummary
	err           error
}
type conversationResumedMsg struct {
	result          *protocol.ConversationSyncResult
	workspace       string
	mcpConfig       *mcp.Config
	updateMCPConfig bool
	err             error
}
type conversationSyncMsg struct {
	result          *protocol.ConversationSyncResult
	fromWorkerEpoch string
	err             error
}
type outputPageMsg struct {
	ref    string
	start  int64
	result *protocol.OutputReadResult
	err    error
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
	uiTitleStyle     = lipgloss.NewStyle().Bold(true).Foreground(tui.ThemeColor("#A94721", "#EFA276"))
	uiMutedStyle     = lipgloss.NewStyle().Foreground(tui.ThemeColor("#62656D", "#9297A3"))
	uiUserStyle      = lipgloss.NewStyle().Bold(true).Foreground(tui.ThemeColor("#27609B", "#78B9EF"))
	uiBuddyStyle     = lipgloss.NewStyle().Bold(true).Foreground(tui.ThemeColor("#A94721", "#EFA276"))
	uiConnectedStyle = lipgloss.NewStyle().Foreground(tui.ThemeColor("#387E61", "#78B99A"))
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
	input := textarea.New()
	input.ShowLineNumbers = false
	input.Placeholder = "Ask Buddy anything…"
	input.SetHeight(1)
	input.SetVirtualCursor(false)
	input.SetStyles(textarea.DefaultStyles(true))
	input.Prompt = "› "
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter"), key.WithHelp("shift+enter", "new line"))
	input.Focus()
	historyPath := os.Getenv("BUDDY_HISTORY_FILE")
	if historyPath == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return homeErr
		}
		historyPath = filepath.Join(home, ".buddy", "history.jsonl")
	}
	history, historyErr := tui.NewInputHistoryRing(historyPath)
	m := &conversationTUI{
		ctx:            ctx,
		client:         api,
		workspace:      workspace,
		startParams:    params,
		databasePath:   params.DBPath,
		standalone:     standalone,
		state:          "ready",
		textarea:       input,
		history:        history,
		width:          80,
		height:         24,
		lineCountCache: make(map[string]int),
		wrapCache:      make(map[string][]string),
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
		m.slashCommands.Register(command)
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
	program := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(f.IOStreams.In), tea.WithOutput(f.IOStreams.Out))
	_, err = program.Run()
	return err
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
	if m.lineCountCache == nil {
		m.lineCountCache = make(map[string]int)
	}
	if m.wrapCache == nil {
		m.wrapCache = make(map[string][]string)
	}
	if m.conversation != "" {
		return tea.Batch(textarea.Blink, m.loadGitBranch(), m.watchEvent(), m.watchPermission())
	}
	return tea.Batch(textarea.Blink, m.loadGitBranch())
}

func (m *conversationTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if m.width != msg.Width {
			m.inspectionColumn = 0
		}
		m.width, m.height = msg.Width, msg.Height
		func() {
			defer func() { _ = recover() }()
			m.textarea.SetWidth(max(1, msg.Width-4))
			m.resizeComposer()
		}()
		if m.approvalModal != nil {
			m.approvalModal.resize(msg.Width, msg.Height)
		}
		m.clearCache()
		if m.activeBlockID != "" {
			for _, b := range m.blocks {
				if b.ID == m.activeBlockID {
					_ = m.getWrappedLines(b, m.width)
					break
				}
			}
		}
		visible := m.computeVisibleBlocks()
		for _, b := range visible {
			_ = m.getWrappedLines(b, m.width)
		}
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scrollOffset += 3
		case tea.MouseWheelDown:
			m.scrollOffset = max(0, m.scrollOffset-3)
		}
		return m, nil
	case gitBranchMsg:
		if msg.workspace == m.workspace {
			m.gitBranch = msg.branch
		}
	case renderTickMsg:
		if m.deltaBuffer.Len() > 0 {
			flushed := m.deltaBuffer.String()
			m.deltaBuffer.Reset()
			// Measure the active block before publishing the new streaming fallback.
			if m.activeBlockID != "" {
				for i := range m.blocks {
					if m.blocks[i].ID == m.activeBlockID {
						m.updateBlock(i, func(block *TranscriptBlock) { block.Body += flushed })
						break
					}
				}
			}
			m.streamingText += flushed
		}
		if m.tickerActive && (m.state == "running" || m.activeBlockID != "" || m.deltaBuffer.Len() > 0) {
			return m, tea.Tick(33*time.Millisecond, func(t time.Time) tea.Msg {
				return renderTickMsg(t)
			})
		}
		m.tickerActive = false
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
		switch msg.Type {
		case string(agent.EventFinished):
			m.state = "idle"
			m.tickerActive = false
			m.deltaBuffer.Reset()
			m.activeBlockID = ""
			return m, m.syncConversation()
		case string(agent.EventError):
			m.state = "idle"
			m.streamingText = ""
			m.tickerActive = false
			m.deltaBuffer.Reset()
			m.activeBlockID = ""
			m.errorText = msg.Content
			return m, m.syncConversation()
		case string(agent.EventTextDelta):
			m.deltaBuffer.WriteString(msg.Content)
			if m.activeBlockID == "" {
				block := TranscriptBlock{
					ID:        newConversationID("block"),
					TurnID:    msg.TurnID,
					Kind:      BlockAssistantMessage,
					Role:      "assistant",
					Header:    "Buddy",
					Body:      "",
					Streaming: true,
					CreatedAt: time.Now(),
				}
				m.appendBlock(block)
				m.activeBlockID = block.ID
			}
			if !m.tickerActive {
				m.tickerActive = true
				return m, tea.Batch(
					m.watchEvent(),
					tea.Tick(33*time.Millisecond, func(t time.Time) tea.Msg {
						return renderTickMsg(t)
					}),
				)
			}
			return m, m.watchEvent()
		case string(agent.EventToolCall):
			tool := "tool"
			kind := BlockToolActivity
			activity := activityFromEvent(msg.Data, msg.TurnID)
			if msg.Data != nil {
				if activity.Tool != "" {
					tool = terminalText(activity.Tool)
				}
				if activity.Path != "" {
					tool += " " + tui.FormatWorkspaceFileHyperlink(m.workspace, terminalText(activity.Path), activity.Line)
				}
				if activity.ParentIdentity != "" {
					kind = BlockSubagentActivity
				}
			}
			block := TranscriptBlock{
				ID:        newConversationID("block"),
				TurnID:    msg.TurnID,
				Kind:      kind,
				Role:      "tool",
				Header:    "Running " + tool,
				Body:      "",
				CreatedAt: time.Now(),
				Activity:  activity,
			}
			if kind == BlockSubagentActivity {
				block.Collapsed = true
				parentKey := activity.ParentIdentity
				for _, candidate := range m.blocks {
					if activityIdentity(candidate) == parentKey && parentKey != "" {
						block.ParentID = candidate.ID
						for i := range m.blocks {
							if m.blocks[i].ID == candidate.ID {
								m.updateBlock(i, func(parent *TranscriptBlock) { parent.Collapsed = true })
								break
							}
						}
						break
					}
				}
				if block.ParentID == "" {
					block.Header = "Detached child · " + block.Header
				}
			}
			m.appendBlock(block)
		case string(agent.EventToolResult):
			result := msg.Content
			if msg.Data != nil {
				if toolResult, ok := msg.Data["tool_result"].(string); ok && toolResult != "" {
					result = toolResult
				}
			}
			activity := activityFromEvent(msg.Data, msg.TurnID)
			key := activity.Identity
			matched := false
			for i := len(m.blocks) - 1; i >= 0; i-- {
				if m.blocks[i].TurnID == msg.TurnID && activityIdentity(m.blocks[i]) == key && key != "" {
					m.updateBlock(i, func(block *TranscriptBlock) {
						block.Body = result
						block.Activity = activity
						block.Completed = true
						name := terminalText(activity.Tool)
						exitCode := 0
						if activity.ExitCode != nil {
							exitCode = *activity.ExitCode
						}
						if name != "" {
							mark := "Done"
							if exitCode != 0 {
								mark = "Failed"
							}
							duration := ""
							if activity.Elapsed > 0 {
								duration = "  ·  " + activity.Elapsed.Round(100*time.Millisecond).String()
							}
							block.Header = mark + " " + name + duration
						}
						block.Collapsed = true
					})
					matched = true
					break
				}
			}
			if result != "" && !matched {
				block := TranscriptBlock{ID: newConversationID("block"), TurnID: msg.TurnID, Kind: BlockToolActivity, Role: "tool", Header: "Result", Body: result, CreatedAt: time.Now(), Collapsed: len(result) > 500, Completed: true, Activity: activity}
				if msg.Data != nil {
					if activity.OutputRef != "" {
						block.Collapsed = true
					}
					if activity.ParentIdentity != "" {
						block.Kind = BlockSubagentActivity
						name := activity.Tool
						if name == "" {
							name = "tool"
						}
						block.Header = terminalText(name)
						for i := range m.blocks {
							if activityIdentity(m.blocks[i]) == activity.ParentIdentity && activity.ParentIdentity != "" {
								block.ParentID = m.blocks[i].ID
								m.updateBlock(i, func(parent *TranscriptBlock) { parent.Collapsed = true })
								break
							}
						}
						if block.ParentID == "" {
							block.Header = "Detached child · " + block.Header
						}
					}
				}
				m.appendBlock(block)
			}
		case string(agent.EventReasoning):
			if msg.Content != "" {
				block := TranscriptBlock{
					ID:        newConversationID("block"),
					TurnID:    msg.TurnID,
					Kind:      BlockAssistantMessage,
					Role:      "reasoning",
					Header:    "Reasoning",
					Body:      msg.Content,
					CreatedAt: time.Now(),
					Activity:  activityFromEvent(msg.Data, msg.TurnID),
				}
				m.appendBlock(block)
			}
		case string(agent.EventTextComplete):
			if m.deltaBuffer.Len() > 0 {
				flushed := m.deltaBuffer.String()
				m.deltaBuffer.Reset()
				m.streamingText += flushed
			}
			m.tickerActive = false
			content := msg.Content
			if content == "" {
				content = m.streamingText
			}
			m.streamingText = ""
			if m.activeBlockID != "" {
				for i := range m.blocks {
					if m.blocks[i].ID == m.activeBlockID {
						m.updateBlock(i, func(block *TranscriptBlock) {
							block.Streaming = false
							if content != "" {
								block.Body = content
							}
						})
						break
					}
				}
				m.activeBlockID = ""
			} else if content != "" {
				block := TranscriptBlock{
					ID:        newConversationID("block"),
					TurnID:    msg.TurnID,
					Kind:      BlockAssistantMessage,
					Role:      "assistant",
					Header:    "Buddy",
					Body:      content,
					CreatedAt: time.Now(),
					Activity:  activityFromEvent(msg.Data, msg.TurnID),
				}
				m.appendBlock(block)
			}
		case string(agent.EventCompactionStart):
			block := TranscriptBlock{
				ID:        newConversationID("block"),
				TurnID:    msg.TurnID,
				Kind:      BlockSystemStatus,
				Role:      "system",
				Header:    "System",
				Body:      "Compacting conversation context…",
				CreatedAt: time.Now(),
			}
			m.appendBlock(block)
		}
		return m, m.watchEvent()
	case permissionRequestMsg:
		if msg.ConversationID == m.conversation && msg.TurnID != "" && msg.RequestID != "" {
			request := protocol.PermissionRequestNotification(msg)
			m.pendingApproval = &request
			m.approvalModal = newApprovalModalState(request.Arguments, m.width, m.height)
			if !m.transcriptFocused {
				m.textarea.Blur()
			}
			m.state = "awaiting_approval"
		}
		return m, m.watchPermission()
	case permissionRespondedMsg:
		if msg.err != nil {
			m.errorText = msg.err.Error()
		} else if m.pendingApproval != nil && m.pendingApproval.RequestID == msg.requestID {
			m.pendingApproval = nil
			m.approvalModal = nil
			if !m.transcriptFocused {
				m.textarea.Focus()
			}
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
		return m, m.resumeEvictedConversation()
	case conversationResumeListMsg:
		if msg.err != nil {
			m.errorText = msg.err.Error()
			m.choosing = ""
			return m, nil
		}
		m.workspaces = msg.workspaces
		if len(msg.workspaces) == 0 {
			m.errorText = "No saved conversations are available. Submit a prompt to start one."
			m.choosing = ""
			return m, nil
		}
		m.choices = make([]string, len(msg.workspaces))
		for i, workspace := range msg.workspaces {
			m.choices[i] = workspace.WorkspaceDir
		}
		m.cursor, m.choosing = 0, "workspace"
	case conversationListMsg:
		if msg.err != nil {
			m.errorText = msg.err.Error()
			m.choosing = ""
			return m, nil
		}
		m.conversations = msg.conversations
		if len(msg.conversations) == 0 {
			m.errorText = "No saved conversations in this project."
			m.choosing = ""
			return m, nil
		}
		m.choices = make([]string, len(msg.conversations))
		for i, conversation := range msg.conversations {
			m.choices[i] = conversation.Task + "  ·  " + conversation.ConversationID
		}
		m.cursor, m.choosing = 0, "conversation"
	case conversationResumedMsg:
		if msg.err != nil {
			m.errorText = msg.err.Error()
			m.choosing = ""
			return m, nil
		}
		m.choosing = ""
		if msg.workspace != "" {
			m.workspace = msg.workspace
			m.startParams.WorkspaceDir = msg.workspace
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
		if m.outputRef == "" || msg.ref != m.outputRef {
			return m, nil
		}
		m.outputLoading = false
		if msg.err != nil {
			m.errorText = msg.err.Error()
			return m, nil
		}
		pageIndex := indexInt64(m.outputPageStarts, msg.start)
		if pageIndex != m.outputPageIndex {
			m.outputLineScroll = 0
			m.inspectionColumn = 0
		}
		m.outputPage = msg.result
		m.outputPageIndex = pageIndex
		return m, nil
	case diffInspectMsg:
		if msg.generation != m.diffRequestGeneration {
			return m, nil
		}
		m.diffLoading = false
		if msg.err != nil {
			m.errorText = msg.err.Error()
			return m, nil
		}
		m.diffView, m.diffSelected, m.diffFileOpen, m.diffScroll = msg.result, 0, false, 0
		return m, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.pendingApproval != nil {
			if m.approvalModal == nil {
				m.approvalModal = newApprovalModalState(m.pendingApproval.Arguments, m.width, m.height)
			}
			response, done, cmd := m.approvalModal.handleKey(msg, *m.pendingApproval)
			if done {
				response.RequestID = m.pendingApproval.RequestID
				response.ConversationID = m.pendingApproval.ConversationID
				response.TurnID = m.pendingApproval.TurnID
				return m, m.respondPermission(*response)
			}
			return m, cmd
		}
		if m.diffLoading && m.diffView == nil {
			if msg.String() == "esc" {
				m.diffLoading = false
				m.diffRequestGeneration++
			}
			return m, nil
		}
		if m.diffView != nil {
			switch msg.String() {
			case "esc":
				if m.diffFileOpen {
					m.diffFileOpen = false
					m.diffScroll = 0
				} else {
					m.diffView = nil
				}
				return m, nil
			case "up", "k":
				if m.diffFileOpen {
					m.diffScroll = max(0, m.diffScroll-1)
				} else if m.diffSelected > 0 {
					m.diffSelected--
				}
				return m, nil
			case "down", "j":
				if m.diffFileOpen {
					lastLine := strings.Count(m.diffView.Files[m.diffSelected].Diff, "\n")
					m.diffScroll = min(lastLine, m.diffScroll+1)
				} else if m.diffSelected+1 < len(m.diffView.Files) {
					m.diffSelected++
				}
				return m, nil
			case "enter":
				if !m.diffFileOpen && len(m.diffView.Files) > 0 {
					m.diffFileOpen = true
					m.inspectionColumn = 0
				}
				return m, nil
			case "left", "right":
				if m.diffFileOpen {
					m.panInspection(m.diffView.Files[m.diffSelected].Diff, msg.String() == "right")
				}
				return m, nil
			case "pgup":
				if m.diffFileOpen {
					m.moveDiffHunk(-1)
				}
				return m, nil
			case "pgdown":
				if m.diffFileOpen {
					m.moveDiffHunk(1)
				}
				return m, nil
			}
			return m, nil
		}
		if m.outputRef != "" {
			switch msg.String() {
			case "esc":
				m.outputRef, m.outputPage = "", nil
				m.clearCache()
				return m, nil
			case "pgup":
				return m, m.changeOutputPage(-1)
			case "pgdown":
				return m, m.changeOutputPage(1)
			case "left", "right":
				if m.outputPage != nil {
					m.panInspection(m.outputPage.Data, msg.String() == "right")
				}
				return m, nil
			case "up":
				m.outputLineScroll = max(0, m.outputLineScroll-1)
				return m, nil
			case "down":
				if m.outputPage != nil {
					lines := strings.Split(m.outputPage.Data, "\n")
					m.outputLineScroll = min(max(0, len(lines)-1), m.outputLineScroll+1)
				}
				return m, nil
			}
			return m, nil
		}
		if msg.String() == "f6" && m.choosing == "" {
			m.transcriptFocused = !m.transcriptFocused
			if m.transcriptFocused {
				m.textarea.Blur()
				m.selectActivity(0)
			} else {
				m.textarea.Focus()
				m.invalidateActivityCache(m.selectedActivity)
			}
			return m, nil
		}
		if m.transcriptFocused {
			switch msg.String() {
			case "up":
				m.selectActivity(-1)
				return m, nil
			case "down":
				m.selectActivity(1)
				return m, nil
			case "enter":
				for _, block := range m.blocks {
					if block.ID != m.selectedActivity {
						continue
					}
					if block.Activity != nil && block.Activity.OutputRef != "" {
						m.outputRef, m.outputPageStarts, m.outputPageIndex = block.Activity.OutputRef, []int64{0}, 0
						m.outputLineScroll = 0
						m.inspectionColumn = 0
						m.outputLoading = true
						m.textarea.Blur()
						return m, m.readOutputPage(block.Activity.OutputRef, 0)
					}
					for i := range m.blocks {
						if m.blocks[i].ID == block.ID {
							m.updateBlock(i, func(current *TranscriptBlock) { current.Collapsed = !current.Collapsed })
							break
						}
					}
					return m, nil
				}
			case "pgup":
				m.scrollOffset += max(1, m.height/2)
				return m, nil
			case "pgdown":
				m.scrollOffset = max(0, m.scrollOffset-max(1, m.height/2))
				return m, nil
			}
			return m, nil
		}
		if m.completionKind != "" {
			switch msg.String() {
			case "esc":
				m.completionKind = ""
				return m, nil
			case "up", "down":
				items := m.completionCount()
				if items > 0 {
					step := -1
					if msg.String() == "down" {
						step = 1
					}
					m.completionCursor = (m.completionCursor + step + items) % items
				}
				return m, nil
			case "tab", "enter":
				if m.completionCount() > 0 {
					isEnter := msg.String() == "enter"
					if m.completionKind == "file" {
						m.replaceCompletion(m.fileMatches[m.completionCursor])
						m.completionKind = ""
						return m, nil
					}
					command := m.commandMatches[m.completionCursor]
					suffix := ""
					if command.ArgsHint != "" {
						suffix = " "
					}
					m.replaceCompletion(command.Name + suffix)
					m.completionKind = ""
					if !isEnter || suffix != "" {
						return m, nil
					}
				}
			}
		}
		if m.choosing == "" && m.history != nil {
			switch msg.String() {
			case "ctrl+p":
				if prompt, ok := m.history.Previous(m.textarea.Value()); ok {
					m.textarea.SetValue(prompt)
					m.textarea.CursorEnd()
					m.clearCompletion()
				}
				return m, nil
			case "ctrl+n":
				if prompt, ok := m.history.Next(); ok {
					m.textarea.SetValue(prompt)
					m.textarea.CursorEnd()
					m.clearCompletion()
				}
				return m, nil
			case "up":
				lineInfo := m.textarea.LineInfo()
				if m.textarea.Line() == 0 && lineInfo.RowOffset == 0 {
					if prompt, ok := m.history.Previous(m.textarea.Value()); ok {
						m.textarea.SetValue(prompt)
						m.textarea.CursorEnd()
						m.clearCompletion()
						return m, nil
					}
				}
			case "down":
				lineInfo := m.textarea.LineInfo()
				if m.textarea.Line() == m.textarea.LineCount()-1 && lineInfo.RowOffset+1 >= lineInfo.Height {
					if prompt, ok := m.history.Next(); ok {
						m.textarea.SetValue(prompt)
						m.textarea.CursorEnd()
						m.clearCompletion()
						return m, nil
					}
				}
			}
		}
		if m.choosing == "" {
			switch msg.String() {
			case "pgup":
				m.scrollOffset += max(1, m.height/2)
				return m, nil
			case "pgdown":
				m.scrollOffset = max(0, m.scrollOffset-max(1, m.height/2))
				return m, nil
			case "end":
				m.scrollOffset = 0
				return m, nil
			}
		}
		if m.choosing != "" {
			switch msg.String() {
			case "up", "k":
				if m.cursor > 0 {
					m.cursor--
				}
			case "down", "j":
				if m.cursor+1 < len(m.choices) {
					m.cursor++
				}
			case "esc":
				m.choosing = ""
			case "enter":
				if len(m.choices) == 0 {
					return m, nil
				}
				if m.choosing == "workspace" {
					m.selectedWorkspace = m.workspaces[m.cursor].WorkspaceDir
					return m, m.listConversations(m.selectedWorkspace)
				}
				conversation := m.conversations[m.cursor]
				m.workspace, m.databasePath = m.selectedWorkspace, conversation.DBPath
				return m, m.resumeSelected(conversation)
			}
			return m, nil
		}
		if msg.String() == "enter" {
			text := strings.TrimSpace(m.textarea.Value())
			if text == "" {
				return m, nil
			}
			m.textarea.Reset()
			m.errorText = ""
			if m.history != nil {
				if err := m.history.Add(text); err != nil {
					m.errorText = fmt.Sprintf("save prompt history: %v", err)
				}
				m.history.Reset()
			}
			if text == "/resume" {
				return m, m.listWorkspaces()
			}
			if text == "/help" {
				var help strings.Builder
				for _, command := range m.slashCommands.Commands() {
					help.WriteString(command.Name + " " + command.ArgsHint + "  " + command.Description + "\n")
				}
				m.appendBlock(TranscriptBlock{ID: newConversationID("block"), Kind: BlockSystemStatus, Role: "system", Header: "Help", Body: strings.TrimSpace(help.String()), CreatedAt: time.Now()})
				return m, nil
			}
			if text == "/diff" {
				if m.conversation == "" {
					m.errorText = "Start or resume a conversation before opening /diff."
					return m, nil
				}
				return m, m.inspectDiff()
			}
			if text == "/interrupt" {
				if m.conversation == "" {
					m.errorText = "No active conversation."
					return m, nil
				}
				return m, func() tea.Msg {
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
			if text == "/copy" {
				var full strings.Builder
				for _, block := range m.blocks {
					if full.Len() > 0 {
						full.WriteString("\n\n")
					}
					full.WriteString(block.Body)
				}
				const status = "Requested clipboard copy of full transcript."
				m.appendBlock(TranscriptBlock{
					ID:        newConversationID("block"),
					Kind:      BlockAssistantMessage,
					Role:      "assistant",
					Header:    "Buddy",
					Body:      status,
					CreatedAt: time.Now(),
				})
				return m, tea.SetClipboard(full.String())
			}
			if text == "/quit" || text == "/exit" {
				return m, tea.Quit
			}
			if text == "/clear" {
				if m.activeBlockID != "" {
					var activeBlock *TranscriptBlock
					for _, block := range m.blocks {
						if block.ID == m.activeBlockID {
							activeBlock = &block
							break
						}
					}
					m.blocks = nil
					if activeBlock != nil {
						m.blocks = append(m.blocks, *activeBlock)
					} else {
						m.activeBlockID = ""
					}
				} else {
					m.blocks = nil
				}
				m.scrollOffset = 0
				m.clearCache()
				return m, nil
			}
			if strings.HasPrefix(text, "/steer ") {
				return m, m.submit(strings.TrimSpace(strings.TrimPrefix(text, "/steer ")), persistence.SubmissionSteer)
			}
			if strings.HasPrefix(text, "/queue ") {
				return m, m.submit(strings.TrimSpace(strings.TrimPrefix(text, "/queue ")), persistence.SubmissionQueue)
			}
			return m, m.submit(text, persistence.SubmissionQueue)
		}
	case tea.PasteMsg:
		if m.pendingApproval != nil && m.approvalModal != nil && m.approvalModal.editing {
			var cmd tea.Cmd
			m.approvalModal.editor, cmd = m.approvalModal.editor.Update(msg)
			return m, cmd
		}
	}
	var cmd tea.Cmd
	beforeValue := m.textarea.Value()
	beforeLine := m.textarea.Line()
	beforeLineInfo := m.textarea.LineInfo()
	m.textarea, cmd = m.textarea.Update(msg)
	m.resizeComposer()
	if beforeValue != m.textarea.Value() || beforeLine != m.textarea.Line() || beforeLineInfo != m.textarea.LineInfo() {
		m.updateCompletion()
	}
	return m, cmd
}

func (m *conversationTUI) completionCount() int {
	if m.completionKind == "file" {
		return len(m.fileMatches)
	}
	return len(m.commandMatches)
}

func (m *conversationTUI) clearCompletion() {
	m.completionKind = ""
	m.commandMatches = nil
	m.fileMatches = nil
	m.completionCursor = 0
	m.completionStart = 0
	m.completionEnd = 0
}

func (m *conversationTUI) replaceCompletion(value string) {
	text := m.textarea.Value()
	if m.completionStart <= m.completionEnd && m.completionEnd <= len(text) {
		m.textarea.SetValue(text[:m.completionStart] + value + text[m.completionEnd:])
		m.textarea.CursorEnd()
	}
}

func (m *conversationTUI) updateCompletion() {
	text := m.textarea.Value()
	line := m.textarea.Line()
	lines := strings.Split(text, "\n")
	if line >= len(lines) {
		m.completionKind = ""
		return
	}
	current := lines[line]
	runes := []rune(current)
	lineInfo := m.textarea.LineInfo()
	cursor := min(len(runes), lineInfo.StartColumn+lineInfo.ColumnOffset)
	start, end := cursor, cursor
	for start > 0 && runes[start-1] != ' ' && runes[start-1] != '\t' {
		start--
	}
	for end < len(runes) && runes[end] != ' ' && runes[end] != '\t' {
		end++
	}
	word := string(runes[start:cursor])
	lineStart := 0
	for i := 0; i < line; i++ {
		lineStart += len(lines[i]) + 1
	}
	m.completionStart = lineStart + len(string(runes[:start]))
	m.completionEnd = lineStart + len(string(runes[:end]))
	m.completionCursor = 0
	if strings.HasPrefix(word, "/") {
		m.commandMatches = m.slashCommands.Match(word)
		m.fileMatches = nil
		m.completionKind = "command"
		if len(m.commandMatches) == 0 {
			m.completionKind = ""
		}
		return
	}
	if strings.HasPrefix(word, "@") {
		m.fileMatches = nil
		entries, err := os.ReadDir(m.workspace)
		if err != nil {
			m.completionKind = ""
			return
		}
		prefix := strings.TrimPrefix(word, "@")
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), prefix) && len(m.fileMatches) < 50 {
				m.fileMatches = append(m.fileMatches, entry.Name())
			}
		}
		m.commandMatches = nil
		m.completionKind = "file"
		if len(m.fileMatches) == 0 {
			m.completionKind = ""
		}
		return
	}
	m.completionKind = ""
}

func (m *conversationTUI) View() tea.View {
	width, height := max(1, m.width), max(1, m.height)
	emergency := width < 40 || height < 12
	standard := width >= 80 && height >= 24
	var rows []string
	if !emergency {
		rows = append(rows, m.headerLines(standard)...)
	}
	if m.choosing != "" && m.pendingApproval == nil {
		footerRows := 0
		if !emergency {
			footerRows = 1
		}
		rows = append(rows, m.renderChooser(height-len(rows)-footerRows)...)
		if footerRows == 1 {
			rows = append(rows, uiMutedStyle.Render("↑/↓ select · Enter choose · Esc back"))
		}
	} else if m.pendingApproval == nil && (m.diffView != nil || m.diffLoading || m.outputRef != "") {
		available := max(0, height-len(rows))
		inspection := m.renderDiffView(available)
		if m.diffView == nil && !m.diffLoading {
			inspection = m.renderOutputView(available)
		}
		rows = append(rows, strings.Split(strings.TrimSuffix(inspection, "\n"), "\n")...)
	} else {
		bottom := m.renderBottom(emergency, standard)
		available := max(0, height-len(rows)-len(bottom))
		transcript := m.conversationLines(available)
		if len(transcript) > available {
			transcript = transcript[len(transcript)-available:]
		}
		leadingRows := 0
		if len(m.blocks) == 0 && m.streamingText == "" && available > len(transcript) {
			leadingRows = (available - len(transcript)) / 2
			rows = append(rows, make([]string, leadingRows)...)
		}
		rows = append(rows, transcript...)
		rows = append(rows, make([]string, max(0, available-leadingRows-len(transcript)))...)
		rows = append(rows, bottom...)
	}
	rows = fitTerminalRows(rows, height)
	for i := range rows {
		rows[i] = ansi.Truncate(rows[i], width, "…")
	}
	view := tea.NewView(strings.Join(rows, "\n"))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

func (m *conversationTUI) headerLines(standard bool) []string {
	workspace := terminalText(m.workspace)
	if workspace == "" {
		workspace = "workspace"
	}
	title := uiTitleStyle.Render("Buddy Agent")
	if !standard {
		state := m.state
		if state == "" {
			state = "idle"
		}
		prefix := title + uiMutedStyle.Render(" · "+state+" · ")
		pathWidth := max(1, m.width-lipgloss.Width(prefix))
		return []string{prefix + uiMutedStyle.Render(ansi.Truncate(workspace, pathWidth, "…"))}
	}
	metadata := []string{workspace}
	if model := strings.TrimSpace(m.startParams.Model); model != "" {
		metadata = append([]string{model}, metadata...)
	}
	if m.gitBranch != "" {
		metadata = append(metadata, m.gitBranch)
	}
	return []string{title, uiMutedStyle.Render(ansi.Truncate(strings.Join(metadata, " · "), max(1, m.width), "…"))}
}

func (m *conversationTUI) renderChooser(height int) []string {
	label := "Choose a project"
	if m.choosing == "conversation" {
		label = "Choose a conversation"
	}
	lines := []string{uiTitleStyle.Render(label)}
	if height <= 1 {
		return lines
	}
	start := max(0, min(m.cursor-height+2, len(m.choices)-height+1))
	for i := start; i < min(len(m.choices), start+height-1); i++ {
		marker := "  "
		if i == m.cursor {
			marker = "› "
		}
		lines = append(lines, marker+terminalText(m.choices[i]))
	}
	return lines
}

func (m *conversationTUI) conversationLines(available int) []string {
	if available <= 0 {
		return nil
	}
	var lines []string
	if len(m.blocks) > 0 {
		lines = m.computeVisibleLines()
	}
	if len(lines) == 0 && len(m.blocks) == 0 && m.streamingText == "" {
		if m.state == "running" {
			lines = []string{uiMutedStyle.Render("Working…")}
		} else {
			lines = []string{uiMutedStyle.Render("Ready when you are."), uiMutedStyle.Render("Describe a task or type /help for commands.")}
		}
	}
	if m.streamingText != "" && m.activeBlockID == "" {
		lines = append(lines, uiBuddyStyle.Render("Buddy"))
		if rendered, err := tui.RenderMarkdown(terminalText(m.streamingText), max(1, m.width-4)); err == nil {
			lines = append(lines, strings.Split(rendered, "\n")...)
		} else {
			lines = append(lines, strings.Split(terminalText(m.streamingText), "\n")...)
		}
	}
	return lines
}

func (m *conversationTUI) renderBottom(emergency, standard bool) []string {
	var rows []string
	if m.pendingApproval != nil {
		if emergency {
			if m.height <= 1 {
				return []string{m.approvalActions(*m.pendingApproval)}
			}
			modalHeight := min(max(2, m.height/2), max(2, m.height))
			if m.approvalModal == nil {
				return []string{"Resize terminal to decide"}
			}
			return fitTerminalRows(strings.Split(m.approvalModal.view(*m.pendingApproval, m.width, modalHeight), "\n"), m.height)
		}
		modalHeight := min(max(2, m.height/2), max(2, m.height))
		if m.approvalModal != nil {
			rows = append(rows, strings.Split(m.approvalModal.view(*m.pendingApproval, m.width, modalHeight), "\n")...)
		} else {
			rows = append(rows, "Resize terminal to decide")
		}
	}
	queued, steering := len(m.queued), len(m.steers)
	for _, submission := range m.pendingStart {
		if submission.mode == persistence.SubmissionSteer {
			steering++
		} else {
			queued++
		}
	}
	if queued+steering > 0 {
		rows = append(rows, uiMutedStyle.Render(fmt.Sprintf("%d queued · %d steering", queued, steering)))
	}
	if m.errorText != "" {
		rows = append(rows, errStyle.Render(terminalText(m.errorText)))
	}
	if m.completionKind != "" {
		rows = append(rows, m.completionLines()...)
	}
	if m.height <= 0 {
		return rows
	}
	if m.pendingApproval == nil || !emergency {
		inputContent := m.renderTextarea()
		if m.pendingApproval != nil {
			inputContent = uiMutedStyle.Render(inputContent)
		}
		if standard {
			rows = append(rows, uiMutedStyle.Render(strings.Repeat("─", max(1, m.width))))
		}
		rows = append(rows, strings.Split(inputContent, "\n")...)
	}
	if !emergency {
		connection := "Connected"
		if m.errorText != "" {
			connection = "Needs attention"
		}
		footer := uiConnectedStyle.Render("● "+connection) + "  " + uiMutedStyle.Render("? shortcuts · / commands")
		if m.state == "running" {
			footer = uiConnectedStyle.Render("● Connected") + "  " + uiBuddyStyle.Render("Working…") + "  " + uiMutedStyle.Render("/interrupt · Ctrl+C detach")
		} else if standard {
			footer += uiMutedStyle.Render("  Enter send · Shift+Enter newline · Ctrl+C detach")
			if m.standalone {
				footer = strings.ReplaceAll(footer, "Ctrl+C detach", "Ctrl+C stop")
			}
		}
		if m.pendingApproval != nil {
			footer = "Approval · " + m.approvalActions(*m.pendingApproval)
		} else if standard && m.transcriptFocused {
			footer = "Transcript · ↑/↓ select · Enter inspect · PgUp/PgDn scroll · F6 composer"
		}
		rows = append(rows, uiMutedStyle.Render(ansi.Truncate(footer, max(1, m.width), "…")))
	}
	return fitTerminalRows(rows, max(1, m.height))
}

func (m *conversationTUI) approvalActions(request protocol.PermissionRequestNotification) string {
	if m.approvalModal == nil {
		return "Resize terminal to decide"
	}
	return m.approvalModal.actionRow(request, max(1, m.width))
}

func (m *conversationTUI) completionLines() []string {
	var rows []string
	start, end := m.completionWindowStart(), m.completionWindowStart()+m.completionRows()
	if m.completionKind == "command" {
		for i := start; i < min(end, len(m.commandMatches)); i++ {
			marker := "  "
			if i == m.completionCursor {
				marker = "› "
			}
			rows = append(rows, marker+m.commandMatches[i].Name+"  "+uiMutedStyle.Render(m.commandMatches[i].Description+" "+m.commandMatches[i].ArgsHint))
		}
	} else {
		for i := start; i < min(end, len(m.fileMatches)); i++ {
			marker := "  "
			if i == m.completionCursor {
				marker = "› "
			}
			rows = append(rows, marker+m.fileMatches[i])
		}
	}
	return rows
}

func fitTerminalRows(rows []string, height int) []string {
	if height <= 0 {
		return nil
	}
	if len(rows) > height {
		return rows[len(rows)-height:]
	}
	return rows
}

func (m *conversationTUI) renderOutputView(height int) string {
	var header, notices strings.Builder
	var lines []string
	header.WriteString(uiTitleStyle.Render("Tool output") + "  ·  " + uiMutedStyle.Render(m.outputRef) + "\n")
	if m.outputLoading {
		header.WriteString("Loading output page…\n")
	} else if m.outputPage != nil {
		byteStart := int64(0)
		if m.outputPageIndex < len(m.outputPageStarts) {
			byteStart = m.outputPageStarts[m.outputPageIndex]
		}
		end := byteStart + int64(len(m.outputPage.Data))
		byteRange := "No output bytes"
		if end > byteStart {
			byteRange = fmt.Sprintf("Bytes %d–%d of %d", byteStart+1, end, m.outputPage.TotalBytes)
		}
		header.WriteString(uiMutedStyle.Render(byteRange) + "\n\n")
		lines = strings.Split(m.outputPage.Data, "\n")
		if m.outputPage.Truncated {
			notices.WriteString(errStyle.Render("Output capped at 1 MiB; rerun the command with narrower output.") + "\n")
		}
		if !m.outputPage.HasMore {
			notices.WriteString(uiMutedStyle.Render("End of available output") + "\n")
		}
	}
	top, footer, rows := m.inspectionLayout(height, header.String(), notices.String(), "↑/↓ scroll · ←/→ pan · PageUp/PageDown pages · Esc returns to activity")
	var b strings.Builder
	b.WriteString(top)
	m.writeInspectionLines(&b, lines, m.outputLineScroll, rows)
	b.WriteString(footer)
	return b.String()
}

func (m *conversationTUI) renderDiffView(height int) string {
	var header, notices strings.Builder
	header.WriteString(uiTitleStyle.Render("Working tree diff") + "  ·  " + uiMutedStyle.Render("/diff") + "\n")
	var files []protocol.DiffFileEntry
	if m.diffView != nil && !m.diffLoading {
		files = m.diffView.Files
	}
	if m.diffLoading {
		header.WriteString("Inspecting Git changes…\n")
	} else if len(files) == 0 {
		header.WriteString("No changed files found.\n")
	} else if !m.diffFileOpen {
		header.WriteString(fmt.Sprintf("%d files  ·  +%d  −%d\n\n", m.diffView.Stats.FilesChanged, m.diffView.Stats.Additions, m.diffView.Stats.Deletions))
	} else {
		f := files[m.diffSelected]
		header.WriteString(terminalText(f.Path) + "  ·  " + diffAttributionLabel(f) + "\n")
		if notice := diffFileNotice(f); notice != "" {
			header.WriteString(uiMutedStyle.Render(notice) + "\n")
		}
	}
	if m.diffView != nil && !m.diffLoading {
		if m.diffView.Warning != "" {
			notices.WriteString(errStyle.Render(terminalText(m.diffView.Warning)) + "\n")
		}
		if m.diffView.OmittedPaths > 0 {
			notices.WriteString(uiMutedStyle.Render(fmt.Sprintf("%d paths omitted because they could not be represented or read safely", m.diffView.OmittedPaths)) + "\n")
		}
		if m.diffView.Truncated {
			notices.WriteString(errStyle.Render("Diff output was capped to fit the safe response limit.") + "\n")
		}
	}
	hint := "↑/↓ files · Enter open · Esc close"
	if m.diffLoading {
		hint = "Esc close"
	} else if m.diffFileOpen {
		hint = "↑/↓ scroll · ←/→ pan · PageUp/PageDown hunks · Esc file list"
	}
	top, footer, rows := m.inspectionLayout(height, header.String(), notices.String(), hint)
	var b strings.Builder
	b.WriteString(top)
	if len(files) > 0 && m.diffFileOpen {
		m.writeInspectionLines(&b, strings.Split(files[m.diffSelected].Diff, "\n"), m.diffScroll, rows)
	} else if len(files) > 0 {
		start := max(0, min(m.diffSelected-rows+1, len(files)-rows))
		for i := start; i < min(len(files), start+rows); i++ {
			f := files[i]
			marker := "  "
			if i == m.diffSelected {
				marker = "› "
			}
			path := strings.ReplaceAll(f.Path, "\n", " ")
			line := fmt.Sprintf("%s%s %-18s %s", marker, inspectionText(path), diffAttributionLabel(f), diffFileNotice(f))
			b.WriteString(ansi.Truncate(line, max(1, m.width), "…") + "\n")
		}
	}
	b.WriteString(footer)
	return b.String()
}

func (m *conversationTUI) inspectionLayout(height int, header, notices, hint string) (string, string, int) {
	if m.errorText != "" {
		notices += errStyle.Render(terminalText(m.errorText)) + "\n"
	}
	controls := wrapTranscriptLines(uiMutedStyle.Render(hint), m.width)
	if len(controls) > height {
		controls = wrapTranscriptLines(uiMutedStyle.Render("Esc close"), m.width)
	}
	controls = controls[:min(len(controls), max(0, height))]
	headerRows := wrapTranscriptLines(strings.TrimSuffix(header, "\n"), m.width)
	headerRows = headerRows[:min(len(headerRows), max(0, height-len(controls)))]
	noticeRows := wrapTranscriptLines(notices, m.width)
	noticeRows = noticeRows[:min(len(noticeRows), max(0, height-len(controls)-len(headerRows)))]
	rows := max(0, height-len(headerRows)-len(noticeRows)-len(controls))
	return strings.Join(append(headerRows, ""), "\n"), strings.Join(append(noticeRows, controls...), "\n"), rows
}

func (m *conversationTUI) writeInspectionLines(b *strings.Builder, lines []string, offset, rows int) {
	start := min(offset, max(0, len(lines)-1))
	for _, line := range lines[start:min(len(lines), start+rows)] {
		b.WriteString(m.inspectionLine(line) + "\n")
	}
}

func inspectionText(line string) string {
	return strings.ReplaceAll(terminalText(line), "\t", "    ")
}

func (m *conversationTUI) inspectionLine(line string) string {
	return ansi.Cut(inspectionText(line), m.inspectionColumn, m.inspectionColumn+max(1, m.width))
}

func (m *conversationTUI) panInspection(content string, right bool) {
	step := max(1, m.width/2)
	if !right {
		m.inspectionColumn = max(0, m.inspectionColumn-step)
		return
	}
	longest := 0
	for line := range strings.SplitSeq(content, "\n") {
		longest = max(longest, ansi.StringWidth(inspectionText(line)))
	}
	m.inspectionColumn = min(max(0, longest-max(1, m.width)), m.inspectionColumn+step)
}

func diffAttributionLabel(f protocol.DiffFileEntry) string {
	if f.AttributionUnknown {
		return "Unknown"
	}
	if f.IsBuddyEdit && f.WasDirtyAtStart {
		return "Buddy + pre-existing"
	}
	if f.IsBuddyEdit {
		return "Buddy edit"
	}
	if f.WasDirtyAtStart {
		return "Pre-existing"
	}
	return "Unknown"
}

func diffFileNotice(f protocol.DiffFileEntry) string {
	var values []string
	if f.IsBinary {
		values = append(values, "binary omitted")
	}
	if f.Truncated {
		values = append(values, "truncated")
	}
	return strings.Join(values, ", ")
}

func (m *conversationTUI) moveDiffHunk(direction int) {
	if m.diffView == nil || m.diffSelected >= len(m.diffView.Files) {
		return
	}
	lines := strings.Split(m.diffView.Files[m.diffSelected].Diff, "\n")
	if direction > 0 {
		for i := m.diffScroll + 1; i < len(lines); i++ {
			if strings.HasPrefix(lines[i], "@@") {
				m.diffScroll = i
				return
			}
		}
		m.diffScroll = min(len(lines)-1, m.diffScroll+max(1, m.height/2))
		return
	}
	for i := m.diffScroll - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "@@") {
			m.diffScroll = i
			return
		}
	}
	m.diffScroll = max(0, m.diffScroll-max(1, m.height/2))
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

func (m *conversationTUI) selectActivity(delta int) {
	activities := make([]string, 0)
	for _, block := range m.blocks {
		if block.Kind != BlockToolActivity && block.Kind != BlockSubagentActivity {
			continue
		}
		if block.ParentID == "" || parentActivityExpanded(block, m.blocks) {
			activities = append(activities, block.ID)
		}
	}
	if len(activities) == 0 {
		m.selectedActivity = ""
		return
	}
	index := 0
	for i, id := range activities {
		if id == m.selectedActivity {
			index = i
			break
		}
	}
	index = max(0, min(len(activities)-1, index+delta))
	previous := m.selectedActivity
	m.selectedActivity = activities[index]
	if previous != m.selectedActivity {
		m.invalidateActivityCache(previous)
	}
	m.invalidateActivityCache(m.selectedActivity)
	selectedParent := m.blocks[m.blockIndex(m.selectedActivity)].ParentID
	selectedBlock := m.selectedActivity
	if selectedParent != "" {
		selectedBlock = selectedParent
	}
	rowsAfter := 0
	for i := len(m.blocks) - 1; i >= 0; i-- {
		block := m.blocks[i]
		count := m.getBlockLineCount(block, m.width)
		if block.ID == selectedBlock {
			row := count / 2
			if selectedParent != "" {
				rendered, childOffset := m.renderBlock(block, m.width)
				if childOffset >= 0 {
					row = len(wrapTranscriptLines(rendered[:childOffset], m.width))
				}
			}
			m.scrollOffset = max(0, rowsAfter+count-row-m.viewportHeight()/2)
			break
		}
		rowsAfter += count
	}
}

func (m *conversationTUI) readOutputPage(ref string, offset int64) tea.Cmd {
	conversationID := m.conversation
	return func() tea.Msg {
		result, err := m.client.ReadOutput(protocol.OutputReadParams{ConversationID: conversationID, OutputRef: ref, Offset: offset})
		return outputPageMsg{ref: ref, start: offset, result: result, err: err}
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
		return m.readOutputPage(m.outputRef, m.outputPageStarts[index])
	}
	if delta > 0 && m.outputPage != nil && m.outputPage.HasMore {
		offset := m.outputPage.NextOffset
		m.outputPageStarts = append(m.outputPageStarts, offset)
		m.outputLoading = true
		return m.readOutputPage(m.outputRef, offset)
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
	conversationID := m.conversation
	m.diffRequestGeneration++
	generation := m.diffRequestGeneration
	m.diffLoading = true
	return func() tea.Msg {
		result, err := m.client.InspectDiff(protocol.DiffInspectParams{ConversationID: conversationID})
		return diffInspectMsg{generation: generation, result: result, err: err}
	}
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

func (m *conversationTUI) respondPermission(params protocol.PermissionRespondParams) tea.Cmd {
	request := m.pendingApproval
	if request == nil {
		return nil
	}
	params.RequestID, params.ConversationID, params.TurnID = request.RequestID, request.ConversationID, request.TurnID
	return func() tea.Msg {
		err := m.client.RespondConversationPermissionParams(params)
		return permissionRespondedMsg{requestID: request.RequestID, err: err}
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
	hadPendingApproval := m.pendingApproval != nil
	m.streamingText = ""
	m.deltaBuffer.Reset()
	m.tickerActive = false
	m.activeBlockID = ""
	m.pendingApproval = nil
	m.approvalModal = nil
	if result == nil {
		return
	}
	activitiesByTurn := make(map[string][]TranscriptBlock)
	var activityTurns []string
	if m.conversation == result.ConversationID && m.workerEpoch == result.WorkerEpoch {
		for _, block := range m.blocks {
			if block.Kind != BlockToolActivity && block.Kind != BlockSubagentActivity {
				continue
			}
			if len(activitiesByTurn[block.TurnID]) == 0 {
				activityTurns = append(activityTurns, block.TurnID)
			}
			activitiesByTurn[block.TurnID] = append(activitiesByTurn[block.TurnID], block)
		}
	} else {
		m.selectedActivity, m.outputRef, m.outputPage, m.outputLoading = "", "", nil, false
	}
	m.conversation, m.state = result.ConversationID, result.State
	m.workerEpoch = result.WorkerEpoch
	if result.ExecutionBlocked != "" {
		m.errorText = "Execution is paused: " + result.ExecutionBlocked
	} else if strings.HasPrefix(m.errorText, "Execution is paused: ") {
		m.errorText = ""
	}
	if len(result.PendingApprovals) > 0 {
		request := result.PendingApprovals[0]
		m.pendingApproval = &request
		m.approvalModal = newApprovalModalState(request.Arguments, m.width, m.height)
		if !m.transcriptFocused {
			m.textarea.Blur()
		}
	} else if hadPendingApproval && !m.transcriptFocused {
		m.textarea.Focus()
	}
	m.lastSequence = result.EventWatermark
	m.blocks = m.blocks[:0]
	m.clearCache()
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
	// Append durable blocks directly to preserve the reading offset.
	for i, entry := range result.Transcript {
		last, hasActivities := lastEntry[entry.TurnID]
		if hasActivities && last == i && entry.Role == "assistant" {
			m.blocks = append(m.blocks, activitiesByTurn[entry.TurnID]...)
		}
		if entry.Role == "user" {
			m.blocks = append(m.blocks, TranscriptBlock{
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
			m.blocks = append(m.blocks, TranscriptBlock{
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
			m.blocks = append(m.blocks, activitiesByTurn[entry.TurnID]...)
		}
	}
	for _, turnID := range activityTurns {
		if _, found := lastEntry[turnID]; !found {
			m.blocks = append(m.blocks, activitiesByTurn[turnID]...)
		}
	}
}

func (m *conversationTUI) renderTextarea() string {
	defer func() {
		_ = recover()
	}()
	return m.textarea.View()
}

func (m *conversationTUI) resizeComposer() {
	maxRows := 5
	if m.width < 80 || m.height < 24 {
		maxRows = 3
	}
	if m.width < 40 || m.height < 12 {
		maxRows = 1
	}
	rows := 1
	if strings.TrimSpace(m.textarea.Value()) != "" {
		width := max(1, m.textarea.Width())
		rows = 0
		for _, line := range strings.Split(m.textarea.Value(), "\n") {
			rows += max(1, (ansi.StringWidth(line)+width-1)/width)
		}
		rows = min(maxRows, max(1, rows))
	}
	m.textarea.SetHeight(rows)
}

func (m *conversationTUI) viewportHeight() int {
	return max(0, m.height-m.viewportOverhead()-m.completionRows())
}

func (m *conversationTUI) viewportOverhead() int {
	width, height := max(1, m.width), max(1, m.height)
	emergency := width < 40 || height < 12
	standard := width >= 80 && height >= 24
	headerHeight := 0
	if !emergency {
		headerHeight = 1
		if standard {
			headerHeight = 2
		}
	}
	composerHeight := max(1, m.textarea.Height())
	if standard && width >= 40 {
		composerHeight += 2
	}
	footerHeight := 0
	if !emergency {
		footerHeight = 1
	}
	overhead := headerHeight + footerHeight
	if m.pendingApproval != nil && emergency {
		if height <= 1 {
			return 1
		}
		modalHeight := min(max(2, height/2), max(2, height))
		if m.approvalModal != nil {
			return lipgloss.Height(m.approvalModal.view(*m.pendingApproval, width, modalHeight))
		}
		return 1
	}
	if m.pendingApproval == nil || !emergency {
		overhead += composerHeight
	}
	if len(m.queued)+len(m.steers)+len(m.pendingStart) > 0 {
		overhead++
	}
	if m.pendingApproval != nil {
		if height <= 1 {
			overhead++
		} else {
			modalHeight := min(max(2, height/2), max(2, height))
			if m.approvalModal != nil {
				overhead += lipgloss.Height(m.approvalModal.view(*m.pendingApproval, width, modalHeight))
			} else {
				overhead++
			}
		}
	}
	if m.errorText != "" {
		overhead++
	}
	return overhead
}

func (m *conversationTUI) completionRows() int {
	if m.completionKind == "" {
		return 0
	}
	return min(8, min(m.completionCount(), max(0, m.height-m.viewportOverhead()-4)))
}

func (m *conversationTUI) completionWindowStart() int {
	rows := m.completionRows()
	return min(max(0, m.completionCursor-rows+1), max(0, m.completionCount()-rows))
}

func (m *conversationTUI) clearCache() {
	m.lineCountCache = make(map[string]int)
	m.wrapCache = make(map[string][]string)
}

func (m *conversationTUI) invalidateBlockCache(blockID string) {
	if m.lineCountCache != nil {
		for k := range m.lineCountCache {
			if strings.HasPrefix(k, blockID+":") {
				delete(m.lineCountCache, k)
			}
		}
	}
	if m.wrapCache != nil {
		for k := range m.wrapCache {
			if strings.HasPrefix(k, blockID+":") {
				delete(m.wrapCache, k)
			}
		}
	}
}

func (m *conversationTUI) invalidateActivityCache(blockID string) {
	if index := m.blockIndex(blockID); index >= 0 {
		m.invalidateBlockCache(blockID)
		if parentID := m.blocks[index].ParentID; parentID != "" {
			m.invalidateBlockCache(parentID)
		}
	}
}

func (m *conversationTUI) appendBlock(block TranscriptBlock) {
	if block.ID == "" {
		block.ID = newConversationID("block")
	}
	if block.CreatedAt.IsZero() {
		block.CreatedAt = time.Now()
	}
	parentIndex, before := -1, 0
	if block.ParentID != "" {
		parentIndex = m.blockIndex(block.ParentID)
		if parentIndex >= 0 && m.scrollOffset > 0 {
			before = m.getBlockLineCount(m.blocks[parentIndex], m.width)
		}
	}
	m.blocks = append(m.blocks, block)
	if parentIndex >= 0 {
		m.invalidateBlockCache(block.ParentID)
		if m.scrollOffset > 0 {
			m.scrollOffset += m.getBlockLineCount(m.blocks[parentIndex], m.width) - before
		}
	} else if block.ParentID == "" && m.scrollOffset > 0 {
		m.scrollOffset += m.getBlockLineCount(block, m.width)
	}
}

func (m *conversationTUI) blockIndex(id string) int {
	for i := range m.blocks {
		if m.blocks[i].ID == id {
			return i
		}
	}
	return -1
}

func (m *conversationTUI) updateBlock(index int, update func(*TranscriptBlock)) {
	block := m.blocks[index]
	parentIndex := m.blockIndex(block.ParentID)
	before, parentBefore := 0, 0
	if m.scrollOffset > 0 {
		before = m.getBlockLineCount(block, m.width)
		if parentIndex >= 0 {
			parentBefore = m.getBlockLineCount(m.blocks[parentIndex], m.width)
		}
	}
	update(&m.blocks[index])
	m.invalidateBlockCache(block.ID)
	if parentIndex >= 0 {
		m.invalidateBlockCache(block.ParentID)
	}
	if m.scrollOffset > 0 {
		after := m.getBlockLineCount(m.blocks[index], m.width)
		m.scrollOffset += after - before
		if parentIndex >= 0 {
			m.scrollOffset += m.getBlockLineCount(m.blocks[parentIndex], m.width) - parentBefore
		}
	}
}

func (m *conversationTUI) getBlockLineCount(b TranscriptBlock, width int) int {
	if b.ParentID != "" {
		return 0
	}
	if b.Kind == BlockAssistantMessage && !b.Streaming && strings.TrimSpace(b.Body) == "" {
		return 0
	}
	if width <= 0 {
		width = 80
	}
	// Running tools can change elapsed time when a viewport entry is rendered again.
	if b.Kind == BlockToolActivity && !b.Completed {
		return len(m.getWrappedLines(b, width)) + 1
	}
	key := fmt.Sprintf("%s:%d", b.ID, width)
	if m.lineCountCache != nil {
		if count, ok := m.lineCountCache[key]; ok {
			return count
		}
	}
	count := len(m.getWrappedLines(b, width)) + 1
	if m.lineCountCache == nil {
		m.lineCountCache = make(map[string]int)
	}
	m.lineCountCache[key] = count
	return count
}

func (m *conversationTUI) getWrappedLines(b TranscriptBlock, width int) []string {
	if width <= 0 {
		width = 80
	}
	if b.ParentID != "" {
		return nil
	}
	if b.Kind == BlockAssistantMessage && !b.Streaming && strings.TrimSpace(b.Body) == "" {
		return nil
	}
	if m.wrapCache == nil {
		m.wrapCache = make(map[string][]string)
	}
	key := fmt.Sprintf("%s:%d", b.ID, width)
	if lines, ok := m.wrapCache[key]; ok {
		return lines
	}
	rendered, _ := m.renderBlock(b, width)
	wrapped := wrapTranscriptLines(rendered, width)
	m.wrapCache[key] = wrapped
	return wrapped
}

func wrapTranscriptLines(rendered string, width int) []string {
	if width <= 0 {
		width = 80
	}
	var wrapped []string
	for l := range strings.SplitSeq(rendered, "\n") {
		if lipgloss.Width(l) <= width {
			wrapped = append(wrapped, l)
		} else {
			renderedLine := lipgloss.NewStyle().Width(width).Render(l)
			wrapped = append(wrapped, strings.Split(renderedLine, "\n")...)
		}
	}
	return wrapped
}

func (m *conversationTUI) renderBlock(block TranscriptBlock, width int) (string, int) {
	return tui.RenderBlock(block, m.blocks, width, m.streamingText, m.selectedActivity, m.transcriptFocused)
}

func parentActivityExpanded(block TranscriptBlock, blocks []TranscriptBlock) bool {
	for _, parent := range blocks {
		if parent.ID == block.ParentID {
			return !parent.Collapsed
		}
	}
	return false
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

func activityIdentity(block TranscriptBlock) string {
	if block.Activity == nil {
		return ""
	}
	return block.Activity.Identity
}

func activityFromEvent(data map[string]any, _ string) *tui.Activity {
	activity := &tui.Activity{Identity: activityDataKey(data), StartedAt: time.Now()}
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
	step, _ := id["step"].(float64)
	call, _ := id["tool_call_id"].(string)
	if turn == "" || call == "" {
		return ""
	}
	return turn + "\x00" + strconv.Itoa(int(step)) + "\x00" + call
}

func (m *conversationTUI) visibleTranscriptWindow() ([]TranscriptBlock, int, int) {
	height := m.viewportHeight()
	if len(m.blocks) == 0 || height == 0 {
		return nil, 0, 0
	}
	m.scrollOffset = max(0, m.scrollOffset)

	// Walk back from the tail so opening a transcript only measures the rows it needs.
	first, last := len(m.blocks), len(m.blocks)
	rows := 0
	for first > 0 && rows-height < m.scrollOffset {
		first--
		rows += m.getBlockLineCount(m.blocks[first], m.width)
	}
	m.scrollOffset = min(m.scrollOffset, max(0, rows-height))
	bottom := rows - m.scrollOffset
	top := max(0, bottom-height)
	for last > first {
		count := m.getBlockLineCount(m.blocks[last-1], m.width)
		if rows-count < bottom {
			break
		}
		rows -= count
		last--
	}
	return m.blocks[first:last], top, bottom
}

func (m *conversationTUI) computeVisibleBlocks() []TranscriptBlock {
	blocks, _, _ := m.visibleTranscriptWindow()
	var visible []TranscriptBlock
	for _, block := range blocks {
		if m.getBlockLineCount(block, m.width) > 0 {
			visible = append(visible, block)
		}
	}
	return visible
}

func (m *conversationTUI) computeVisibleLines() []string {
	blocks, top, bottom := m.visibleTranscriptWindow()
	lines := make([]string, 0, bottom-top)
	visibleIDs := make(map[string]bool)
	row := 0
	for _, block := range blocks {
		count := m.getBlockLineCount(block, m.width)
		if count == 0 {
			continue
		}
		visibleIDs[block.ID] = true
		wrapped := m.getWrappedLines(block, m.width)
		start, end := max(0, top-row), min(count, bottom-row)
		lines = append(lines, wrapped[start:min(len(wrapped), end)]...)
		if end == count {
			lines = append(lines, "")
		}
		row += count
	}
	for key := range m.wrapCache {
		id, _, _ := strings.Cut(key, ":")
		if !visibleIDs[id] {
			delete(m.wrapCache, key)
		}
	}
	return lines
}

func (m *conversationTUI) listWorkspaces() tea.Cmd {
	return func() tea.Msg {
		result, err := m.client.ListConversationWorkspaces(50)
		if err != nil {
			return conversationResumeListMsg{err: err}
		}
		return conversationResumeListMsg{workspaces: result.Workspaces}
	}
}

func (m *conversationTUI) listConversations(workspace string) tea.Cmd {
	return func() tea.Msg {
		result, err := m.client.ListConversations(protocol.ConversationListParams{WorkspaceDir: workspace, Limit: 50})
		if err != nil {
			return conversationListMsg{err: err}
		}
		return conversationListMsg{conversations: result.Conversations}
	}
}

func (m *conversationTUI) resumeSelected(selected protocol.ConversationSummary) tea.Cmd {
	workspace := m.selectedWorkspace
	startParams := m.startParams
	loadMCPConfig := m.loadWorkspaceMCPConfig
	api := m.client
	return func() tea.Msg {
		resumeParams, mcpConfig, err := conversationResumeParamsForWorkspace(workspace, selected.ConversationID, selected.DBPath, startParams, loadMCPConfig)
		if err != nil {
			return conversationResumedMsg{err: err}
		}
		_, err = api.ResumeConversation(resumeParams)
		if err != nil {
			return conversationResumedMsg{err: err}
		}
		result, err := readConversationSnapshot(api, selected.ConversationID)
		return conversationResumedMsg{result: result, workspace: workspace, mcpConfig: mcpConfig, updateMCPConfig: true, err: err}
	}
}

func (m *conversationTUI) resumeEvictedConversation() tea.Cmd {
	workspace, conversationID, databasePath := m.workspace, m.conversation, m.databasePath
	startParams := m.startParams
	api := m.client
	return func() tea.Msg {
		resumeParams := conversationResumeParams(workspace, conversationID, startParams)
		resumeParams.DBPath = databasePath
		_, err := api.ResumeConversation(resumeParams)
		if err != nil {
			return conversationResumedMsg{err: err}
		}
		result, err := readConversationSnapshot(api, conversationID)
		return conversationResumedMsg{result: result, workspace: workspace, mcpConfig: startParams.MCPConfig, updateMCPConfig: true, err: err}
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
