//go:build darwin || linux

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/creack/pty"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/tui"
	"golang.org/x/term"
)

const ptyHelperEnv = "BUDDY_TUI_PTY_HELPER"

type ptyReport struct {
	Type         string `json:"type"`
	Value        string `json:"value,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	Raw          bool   `json:"raw,omitempty"`
	Restored     bool   `json:"restored,omitempty"`
	OutputFailed bool   `json:"output_failed,omitempty"`
	Error        string `json:"error,omitempty"`
}

var (
	ptyReportMu sync.Mutex
	ptyEncoder  *json.Encoder
)

func reportPTY(report ptyReport) {
	ptyReportMu.Lock()
	defer ptyReportMu.Unlock()
	if ptyEncoder != nil {
		_ = ptyEncoder.Encode(report)
	}
}

type ptyProbeModel struct{ text string }

func (ptyProbeModel) Init() tea.Cmd { return nil }

func (m ptyProbeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		raw, _ := ptyInputIsRaw(0)
		reportPTY(ptyReport{Type: "window", Width: msg.Width, Height: msg.Height, Raw: raw})
	case tea.MouseWheelMsg:
		reportPTY(ptyReport{Type: "mouse", Value: fmt.Sprint(msg.Button)})
	case tea.KeyPressMsg:
		reportPTY(ptyReport{Type: "key", Value: msg.String()})
		if os.Getenv(ptyHelperEnv) == "panic" {
			panic("injected model failure")
		}
		if msg.String() == "ctrl+c" {
			return ptyProbeModel{text: msg.String()}, tea.Quit
		}
		return ptyProbeModel{text: msg.String()}, nil
	}
	return m, nil
}

func (m ptyProbeModel) View() tea.View {
	view := tea.NewView("pty probe " + m.text)
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

type failAfterAltPTYWriter struct {
	*os.File
	cancel  context.CancelFunc
	mu      sync.Mutex
	started bool
	failed  bool
}

func (w *failAfterAltPTYWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started && !w.failed {
		w.failed = true
		reportPTY(ptyReport{Type: "output-failed", OutputFailed: true})
		w.cancel()
		return 0, errors.New("injected PTY output failure")
	}
	n, err := w.File.Write(p)
	if bytes.Contains(p, []byte("\x1b[?1049h")) {
		w.started = true
	}
	return n, err
}

type ptyConversationAPI struct{ conversationAPITestDouble }

func (ptyConversationAPI) Capabilities() (*protocol.DaemonCapabilitiesResult, error) {
	return &protocol.DaemonCapabilitiesResult{Methods: []string{
		protocol.MethodConversationStart, protocol.MethodConversationResume,
		protocol.MethodConversationSubmit, protocol.MethodConversationSync,
		protocol.MethodConversationList, protocol.MethodConversationWorkspaces,
		protocol.MethodTurnInterrupt,
	}}, nil
}

func (ptyConversationAPI) ResumeConversation(protocol.ConversationResumeParams) (*protocol.ConversationResumeResult, error) {
	return &protocol.ConversationResumeResult{ConversationID: "pty-conversation", State: "idle"}, nil
}

func (ptyConversationAPI) SyncConversation(protocol.ConversationSyncParams) (*protocol.ConversationSyncResult, error) {
	return &protocol.ConversationSyncResult{ConversationID: "pty-conversation", State: "idle"}, nil
}

func (ptyConversationAPI) SubmitConversation(params protocol.ConversationSubmitParams) (*protocol.ConversationSubmitResult, error) {
	reportPTY(ptyReport{Type: "submitted", Value: params.Prompt})
	return &protocol.ConversationSubmitResult{
		ConversationID: params.ConversationID, SubmissionID: params.SubmissionID, Status: "accepted",
	}, nil
}

func TestConversationTUIPTYChild(t *testing.T) {
	scenario := os.Getenv(ptyHelperEnv)
	if scenario == "" {
		t.Skip("PTY child helper")
	}
	status := os.NewFile(3, "pty-status")
	control := os.NewFile(4, "pty-control")
	if status == nil || control == nil {
		t.Fatal("PTY helper pipes were not passed")
	}
	ptyEncoder = json.NewEncoder(status)
	defer func() { _ = status.Close(); _ = control.Close() }()

	_, err := term.GetState(0)
	if err != nil {
		t.Fatal(err)
	}
	rawBefore, err := ptyInputIsRaw(0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if scenario == "cancel" {
		go func() {
			line, _ := bufio.NewReader(control).ReadString('\n')
			if strings.TrimSpace(line) == "cancel" {
				cancel()
			}
		}()
	}

	var runErr error
	switch scenario {
	case "probe", "cancel", "panic":
		runErr = tui.Run(ctx, ptyProbeModel{}, os.Stdin, os.Stdout)
	case "output-error":
		runErr = tui.Run(ctx, ptyProbeModel{}, os.Stdin, &failAfterAltPTYWriter{File: os.Stdout, cancel: cancel})
	case "conversation":
		factory := &cmdutil.Factory{IOStreams: &cmdutil.IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: io.Discard}}
		runErr = runConversationTUIWithAPI(ctx, factory, &ptyConversationAPI{}, protocol.ConversationStartParams{Model: "pty-test"}, ".", true, false)
	default:
		t.Fatalf("unknown PTY scenario %q", scenario)
	}
	_, stateErr := term.GetState(0)
	rawAfter, rawErr := ptyInputIsRaw(0)
	report := ptyReport{Type: "done"}
	if runErr != nil {
		report.Error = runErr.Error()
	}
	if stateErr == nil && rawErr == nil {
		report.Restored = term.IsTerminal(0) && !rawBefore && !rawAfter
	}
	reportPTY(report)
}

type ptyChild struct {
	cmd      *exec.Cmd
	master   *os.File
	control  *os.File
	reports  chan ptyReport
	output   bytes.Buffer
	outputMu sync.Mutex
	wait     chan error
}

func startPTYChild(t *testing.T, scenario string) *ptyChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	controlR, controlW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestConversationTUIPTYChild$")
	cmd.Env = append(os.Environ(), ptyHelperEnv+"="+scenario, "BUDDY_HISTORY_FILE="+t.TempDir()+"/history.jsonl")
	cmd.ExtraFiles = []*os.File{statusW, controlR}
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	_ = statusW.Close()
	_ = controlR.Close()
	if err != nil {
		_ = statusR.Close()
		_ = controlW.Close()
		t.Fatal(err)
	}
	child := &ptyChild{cmd: cmd, master: master, control: controlW, reports: make(chan ptyReport, 32), wait: make(chan error, 1)}
	go func() {
		defer close(child.reports)
		scanner := bufio.NewScanner(statusR)
		for scanner.Scan() {
			var report ptyReport
			if json.Unmarshal(scanner.Bytes(), &report) == nil {
				child.reports <- report
			}
		}
		_ = statusR.Close()
	}()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := master.Read(buf)
			if n > 0 {
				child.outputMu.Lock()
				_, _ = child.output.Write(buf[:n])
				child.outputMu.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()
	go func() { child.wait <- cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-child.wait:
		default:
			_ = cmd.Process.Kill()
			<-child.wait
		}
		_ = child.control.Close()
		_ = child.master.Close()
	})
	return child
}

func (c *ptyChild) outputText() string {
	c.outputMu.Lock()
	defer c.outputMu.Unlock()
	return c.output.String()
}

func (c *ptyChild) waitReport(t *testing.T, kind string, match func(ptyReport) bool) ptyReport {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case report, ok := <-c.reports:
			if !ok {
				t.Fatalf("PTY child exited before %s report", kind)
			}
			if report.Type == kind && (match == nil || match(report)) {
				return report
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s report; output %q", kind, c.outputText())
		}
	}
}

func (c *ptyChild) waitOutput(t *testing.T, contains string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.outputText(), contains) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(c.outputText(), contains) {
		t.Fatalf("PTY output did not contain %q: %q", contains, c.outputText())
	}
}

func (c *ptyChild) writeInput(t *testing.T, input string) {
	t.Helper()
	if _, err := c.master.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
}

func TestTUIRunnerUsesAndRestoresRealPTY(t *testing.T) {
	child := startPTYChild(t, "probe")
	child.waitReport(t, "window", func(r ptyReport) bool { return r.Width == 80 && r.Height == 24 && r.Raw })
	child.waitOutput(t, "\x1b[?1049h")
	child.waitOutput(t, "\x1b[?1002h")
	child.writeInput(t, "x")
	child.waitReport(t, "key", func(r ptyReport) bool { return r.Value == "x" })
	child.writeInput(t, "\x1b[<64;1;1M")
	child.waitReport(t, "mouse", nil)
	if err := pty.Setsize(child.master, &pty.Winsize{Cols: 40, Rows: 12}); err != nil {
		t.Fatal(err)
	}
	if err := child.cmd.Process.Signal(syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	child.waitReport(t, "window", func(r ptyReport) bool { return r.Width == 40 && r.Height == 12 })
	child.writeInput(t, string([]byte{3}))
	done := child.waitReport(t, "done", nil)
	if done.Error != "" || !done.Restored {
		t.Fatalf("normal quit failed to restore the PTY: %+v", done)
	}
	child.waitOutput(t, "\x1b[?1049l")
	child.waitOutput(t, "\x1b[?25h")
}

func TestTUIRunnerRestoresPTYOnCancelAndErrors(t *testing.T) {
	for _, scenario := range []string{"cancel", "panic", "output-error"} {
		t.Run(scenario, func(t *testing.T) {
			child := startPTYChild(t, scenario)
			child.waitReport(t, "window", func(r ptyReport) bool { return r.Raw })
			child.waitOutput(t, "\x1b[?1049h")
			if scenario == "cancel" {
				if _, err := io.WriteString(child.control, "cancel\n"); err != nil {
					t.Fatal(err)
				}
			} else {
				child.writeInput(t, "x")
			}
			if scenario == "output-error" {
				failed := child.waitReport(t, "output-failed", nil)
				if !failed.OutputFailed {
					t.Fatal("expected injected terminal output failure")
				}
			}
			done := child.waitReport(t, "done", nil)
			if !done.Restored {
				t.Fatalf("terminal state was not restored: %+v", done)
			}
			if done.Error == "" {
				t.Fatal("expected the canceled or failed Bubble Tea run to return an error")
			}
			child.waitOutput(t, "\x1b[?1049l")
			child.waitOutput(t, "\x1b[?25h")
		})
	}
}

func TestConversationPromptSubmissionThroughRealPTY(t *testing.T) {
	child := startPTYChild(t, "conversation")
	child.waitOutput(t, "\x1b[?1049h")
	child.writeInput(t, "Explain this project\r")
	child.waitReport(t, "submitted", func(r ptyReport) bool { return r.Value == "Explain this project" })
	child.writeInput(t, string([]byte{3}))
	done := child.waitReport(t, "done", nil)
	if done.Error != "" || !done.Restored {
		t.Fatalf("conversation TUI did not exit cleanly: %+v", done)
	}
}

var _ conversationAPI = (*ptyConversationAPI)(nil)
var _ tea.Model = ptyProbeModel{}
