package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/client"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"golang.org/x/term"
)

type conversationTUI struct {
	ctx               context.Context
	client            conversationAPI
	workspace         string
	startParams       protocol.ConversationStartParams
	conversation      string
	databasePath      string
	standalone        bool
	state             string
	lastSequence      uint64
	scrollOffset      int
	transcript        []string
	streamingText     string
	pendingApproval   *protocol.PermissionRequestNotification
	queued            []string
	steers            []string
	textarea          textarea.Model
	width             int
	height            int
	errorText         string
	choosing          string
	choices           []string
	cursor            int
	workspaces        []protocol.ConversationWorkspace
	conversations     []protocol.ConversationSummary
	selectedWorkspace string
}

type conversationAPI interface {
	Capabilities() (*protocol.DaemonCapabilitiesResult, error)
	StartConversation(protocol.ConversationStartParams) (*protocol.ConversationStartResult, error)
	ResumeConversation(protocol.ConversationResumeParams) (*protocol.ConversationResumeResult, error)
	SyncConversation(protocol.ConversationSyncParams) (*protocol.ConversationSyncResult, error)
	ListConversationWorkspaces(int) (*protocol.ConversationListWorkspacesResult, error)
	ListConversations(protocol.ConversationListParams) (*protocol.ConversationListResult, error)
	SubmitConversation(protocol.ConversationSubmitParams) (*protocol.ConversationSubmitResult, error)
	InterruptTurn(protocol.TurnInterruptParams) (*protocol.TurnInterruptResult, error)
	RespondConversationPermission(string, string, string, string) error
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
	result *protocol.ConversationSubmitResult
	prompt string
	mode   persistence.SubmissionMode
	err    error
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
	result *protocol.ConversationSyncResult
	err    error
}
type conversationSyncMsg struct {
	result *protocol.ConversationSyncResult
	err    error
}
type conversationGapMsg struct{}
type conversationEvictedMsg string

var (
	uiTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	uiMutedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	uiUserStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("75"))
	uiBuddyStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
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
	capabilities, err := api.Capabilities()
	if err != nil {
		return fmt.Errorf("check daemon capabilities: %w", err)
	}
	if !containsConversationMethods(capabilities.Methods) {
		return fmt.Errorf("this daemon does not support M9 conversations; restart it when its current work is safe to stop, then retry")
	}
	input := textarea.New()
	input.Placeholder = "Ask Buddy anything…"
	input.Prompt = "▌ "
	input.SetVirtualCursor(false)
	input.SetStyles(textarea.DefaultStyles(true))
	input.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter"), key.WithHelp("shift+enter", "new line"))
	input.Focus()
	m := &conversationTUI{ctx: ctx, client: api, workspace: workspace, startParams: params, databasePath: params.DBPath, standalone: standalone, state: "ready", textarea: input, width: 80, height: 24}
	if continueLatest {
		result, resumeErr := api.ResumeConversation(protocol.ConversationResumeParams{WorkspaceDir: workspace, DBPath: params.DBPath})
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
		resumed, err := cl.ResumeConversation(protocol.ConversationResumeParams{WorkspaceDir: workspace, DBPath: params.DBPath})
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
		} else {
			_, err := cl.SubmitConversation(protocol.ConversationSubmitParams{ConversationID: conversationID, SubmissionID: newConversationID("submission"), Prompt: prompt, Mode: string(mode)})
			if err != nil {
				return fmt.Errorf("submit conversation prompt: %w", err)
			}
		}
		if err := waitConversationTerminal(ctx, f, cl, conversationID); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

func waitConversationTerminal(ctx context.Context, f *cmdutil.Factory, cl *client.Client, conversationID string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case evt, ok := <-cl.ConversationEvents():
			if !ok {
				return fmt.Errorf("conversation daemon connection closed")
			}
			if evt.ConversationID != conversationID {
				continue
			}
			renderEvent(f, evt)
			if evt.Type == string(agent.EventFinished) {
				return nil
			}
			if evt.Type == string(agent.EventError) && evt.Data["terminal"] == true {
				return fmt.Errorf("conversation turn failed: %s", evt.Content)
			}
		case <-cl.ConversationEventGaps():
			if _, err := cl.SyncConversation(protocol.ConversationSyncParams{ConversationID: conversationID}); err != nil {
				return fmt.Errorf("resync conversation after event gap: %w", err)
			}
		}
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
	if m.conversation != "" {
		return tea.Batch(textarea.Blink, m.watchEvent(), m.watchPermission())
	}
	return textarea.Blink
}

func (m *conversationTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.textarea.SetWidth(max(20, msg.Width-6))
		m.textarea.SetHeight(min(5, max(3, msg.Height/5)))
	case conversationStartedMsg:
		if msg.err != nil {
			m.errorText = msg.err.Error()
			return m, nil
		}
		m.conversation = msg.result.ConversationID
		m.state = "running"
		return m, tea.Batch(m.syncConversation(), m.watchPermission())
	case conversationSubmittedMsg:
		if msg.err != nil {
			m.errorText = msg.err.Error()
			m.state = "idle"
			return m, nil
		}
		if msg.mode == persistence.SubmissionSteer {
			m.steers = append(m.steers, msg.prompt)
		} else {
			m.queued = append(m.queued, msg.prompt)
		}
		m.state = "running"
		return m, m.syncConversation()
	case conversationEventMsg:
		if string(msg.ConversationID) != m.conversation {
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
			return m, m.syncConversation()
		case string(agent.EventError):
			m.state = "idle"
			m.streamingText = ""
			m.errorText = msg.Content
			return m, m.syncConversation()
		case string(agent.EventTextDelta):
			m.streamingText += msg.Content
		case string(agent.EventToolCall):
			tool := "tool"
			if msg.Data != nil {
				if name, ok := msg.Data["tool_name"].(string); ok && name != "" {
					tool = name
				}
			}
			m.transcript = append(m.transcript, toolStyle.Render("Running "+tool))
		case string(agent.EventToolResult):
			result := msg.Content
			if msg.Data != nil {
				if toolResult, ok := msg.Data["tool_result"].(string); ok && toolResult != "" {
					result = toolResult
				}
			}
			if result != "" {
				if len(result) > 300 {
					result = result[:300] + "…"
				}
				m.transcript = append(m.transcript, uiMutedStyle.Render(result))
			}
		case string(agent.EventReasoning):
			if msg.Content != "" {
				m.transcript = append(m.transcript, thoughtStyle.Render(msg.Content))
			}
		case string(agent.EventTextComplete):
			m.streamingText = ""
			if msg.Content != "" {
				m.transcript = append(m.transcript, uiBuddyStyle.Render("Buddy")+"\n"+msg.Content)
			}
		case string(agent.EventCompactionStart):
			m.transcript = append(m.transcript, uiMutedStyle.Render("Compacting conversation context…"))
		}
		return m, m.watchEvent()
	case permissionRequestMsg:
		if msg.ConversationID == m.conversation && msg.TurnID != "" && msg.RequestID != "" {
			request := protocol.PermissionRequestNotification(msg)
			m.pendingApproval = &request
			m.state = "awaiting_approval"
		}
		return m, m.watchPermission()
	case permissionRespondedMsg:
		if msg.err != nil {
			m.errorText = msg.err.Error()
		} else if m.pendingApproval != nil && m.pendingApproval.RequestID == msg.requestID {
			m.pendingApproval = nil
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
		m.workspace = m.selectedWorkspace
		m.loadSync(msg.result)
		return m, tea.Batch(m.watchEvent(), m.watchPermission())
	case conversationSyncMsg:
		if msg.err != nil {
			m.errorText = msg.err.Error()
		} else {
			m.loadSync(msg.result)
			return m, m.watchEvent()
		}
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.pendingApproval != nil {
			switch strings.ToLower(msg.String()) {
			case "y", "a":
				return m, m.respondPermission("allow")
			case "n", "d":
				return m, m.respondPermission("deny")
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
			if text == "/resume" {
				return m, m.listWorkspaces()
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
			if strings.HasPrefix(text, "/steer ") {
				return m, m.submit(strings.TrimSpace(strings.TrimPrefix(text, "/steer ")), persistence.SubmissionSteer)
			}
			if strings.HasPrefix(text, "/queue ") {
				return m, m.submit(strings.TrimSpace(strings.TrimPrefix(text, "/queue ")), persistence.SubmissionQueue)
			}
			if m.conversation == "" {
				return m, m.start(text)
			}
			return m, m.submit(text, persistence.SubmissionQueue)
		}
	}
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m *conversationTUI) View() tea.View {
	var b strings.Builder
	b.WriteString(uiTitleStyle.Render("buddy") + uiMutedStyle.Render("  ·  "+m.workspace) + "\n")
	if m.conversation != "" {
		b.WriteString(uiMutedStyle.Render("Conversation "+m.conversation+"  ·  "+m.state) + "\n")
	}
	b.WriteString(strings.Repeat("─", max(1, m.width)) + "\n")
	if m.choosing != "" {
		label := "Choose a project"
		if m.choosing == "conversation" {
			label = "Choose a conversation"
		}
		b.WriteString(label + "\n\n")
		for i, choice := range m.choices {
			marker := "  "
			if i == m.cursor {
				marker = "› "
			}
			b.WriteString(marker + choice + "\n")
		}
	} else {
		visible := max(4, m.height-12)
		end := max(0, len(m.transcript)-m.scrollOffset)
		start := max(0, end-visible)
		for _, line := range m.transcript[start:end] {
			b.WriteString(line + "\n\n")
		}
		if m.streamingText != "" {
			b.WriteString(uiBuddyStyle.Render("Buddy") + "\n" + m.streamingText + "\n\n")
		}
		for _, prompt := range m.queued {
			b.WriteString(uiMutedStyle.Render("Queued · "+prompt) + "\n")
		}
		for _, prompt := range m.steers {
			b.WriteString(uiTitleStyle.Render("Steering · "+prompt) + "\n")
		}
		if len(m.queued)+len(m.steers) > 0 {
			b.WriteByte('\n')
		}
		if m.pendingApproval != nil {
			b.WriteString(errStyle.Render("Approval required for "+m.pendingApproval.Tool+" · press y to allow or n to deny") + "\n")
			if len(m.pendingApproval.Arguments) > 0 {
				args, _ := json.Marshal(m.pendingApproval.Arguments)
				preview := string(args)
				if len(preview) > 500 {
					preview = preview[:500] + "…"
				}
				b.WriteString(uiMutedStyle.Render(preview) + "\n")
			}
		}
		b.WriteString(m.textarea.View())
		footer := "Enter send  ·  Shift+Enter newline  ·  /steer <prompt>  ·  /resume  ·  Ctrl+C detach"
		if m.standalone {
			footer = "Enter send  ·  Shift+Enter newline  ·  /steer <prompt>  ·  /resume  ·  Ctrl+C stop local run"
		}
		b.WriteString("\n" + uiMutedStyle.Render(footer) + "\n")
	}
	if m.errorText != "" {
		b.WriteString(errStyle.Render(m.errorText) + "\n")
	}
	return tea.NewView(b.String())
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

func (m *conversationTUI) submit(prompt string, mode persistence.SubmissionMode) tea.Cmd {
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	if m.conversation == "" {
		return m.start(prompt)
	}
	params := protocol.ConversationSubmitParams{ConversationID: m.conversation, SubmissionID: newConversationID("submission"), Prompt: prompt, Mode: string(mode)}
	return func() tea.Msg {
		result, err := m.client.SubmitConversation(params)
		return conversationSubmittedMsg{result: result, prompt: prompt, mode: mode, err: err}
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

func (m *conversationTUI) respondPermission(decision string) tea.Cmd {
	request := m.pendingApproval
	if request == nil {
		return nil
	}
	return func() tea.Msg {
		err := m.client.RespondConversationPermission(request.ConversationID, request.TurnID, request.RequestID, decision)
		return permissionRespondedMsg{requestID: request.RequestID, err: err}
	}
}

func (m *conversationTUI) syncConversation() tea.Cmd {
	id := m.conversation
	return func() tea.Msg {
		result, err := readConversationSnapshot(m.client, id)
		return conversationSyncMsg{result: result, err: err}
	}
}

func (m *conversationTUI) loadSync(result *protocol.ConversationSyncResult) {
	m.streamingText = ""
	m.pendingApproval = nil
	if result == nil {
		return
	}
	m.conversation, m.state = result.ConversationID, result.State
	if len(result.PendingApprovals) > 0 {
		request := result.PendingApprovals[0]
		m.pendingApproval = &request
	}
	m.lastSequence = result.EventWatermark
	m.transcript = m.transcript[:0]
	m.queued, m.steers = nil, nil
	if result.OutputIncomplete {
		m.errorText = "Some transient output is unavailable; restored from durable conversation history."
	}
	for _, entry := range result.Transcript {
		if entry.Role == "user" {
			m.transcript = append(m.transcript, uiUserStyle.Render("You")+"\n"+entry.Content)
			if entry.Status == string(persistence.SubmissionQueued) {
				m.queued = append(m.queued, entry.Content)
			}
			if entry.Status == string(persistence.SubmissionSteerPending) {
				m.steers = append(m.steers, entry.Content)
			}
		}
		if entry.Role == "assistant" {
			m.transcript = append(m.transcript, uiBuddyStyle.Render("Buddy")+"\n"+entry.Content)
		}
	}
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
	return func() tea.Msg {
		_, err := m.client.ResumeConversation(protocol.ConversationResumeParams{WorkspaceDir: workspace, ConversationID: selected.ConversationID, DBPath: selected.DBPath})
		if err != nil {
			return conversationResumedMsg{err: err}
		}
		result, err := readConversationSnapshot(m.client, selected.ConversationID)
		return conversationResumedMsg{result: result, err: err}
	}
}

func (m *conversationTUI) resumeEvictedConversation() tea.Cmd {
	workspace, conversationID, databasePath := m.workspace, m.conversation, m.databasePath
	return func() tea.Msg {
		_, err := m.client.ResumeConversation(protocol.ConversationResumeParams{WorkspaceDir: workspace, ConversationID: conversationID, DBPath: databasePath})
		if err != nil {
			return conversationResumedMsg{err: err}
		}
		result, err := readConversationSnapshot(m.client, conversationID)
		return conversationResumedMsg{result: result, err: err}
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
		apiKey := cfg.APIKey
		params.APIKey = &apiKey
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
