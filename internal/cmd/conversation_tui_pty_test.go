//go:build darwin || linux

package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"charm.land/bubbletea/v2"
	"github.com/creack/pty"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type ptyConversationAPI struct{ conversationAPITestDouble }

func (ptyConversationAPI) Capabilities() (*protocol.DaemonCapabilitiesResult, error) {
	return &protocol.DaemonCapabilitiesResult{Methods: []string{
		protocol.MethodConversationStart, protocol.MethodConversationResume,
		protocol.MethodConversationSubmit, protocol.MethodConversationSync,
		protocol.MethodConversationList, protocol.MethodConversationWorkspaces,
		protocol.MethodTurnInterrupt,
	}}, nil
}

func (*ptyConversationAPI) ResumeConversation(protocol.ConversationResumeParams) (*protocol.ConversationResumeResult, error) {
	return &protocol.ConversationResumeResult{ConversationID: "pty-conversation", State: "idle"}, nil
}

func (*ptyConversationAPI) SyncConversation(protocol.ConversationSyncParams) (*protocol.ConversationSyncResult, error) {
	return &protocol.ConversationSyncResult{ConversationID: "pty-conversation", State: "idle"}, nil
}

func TestConversationTUIUsesAndRestoresRealPTY(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = master.Close() }()
	defer func() { _ = slave.Close() }()
	if err := pty.Setsize(master, &pty.Winsize{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(int(master.Fd()), true); err != nil {
		t.Fatal(err)
	}
	original, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	factory := &cmdutil.Factory{IOStreams: &cmdutil.IOStreams{In: slave, Out: slave, ErrOut: io.Discard}}
	api := ptyConversationAPI{}
	workspace := "/" + strings.Repeat("w", 50)
	done := make(chan error, 1)
	go func() {
		done <- runConversationTUIWithAPI(ctx, factory, &api, protocol.ConversationStartParams{Model: "test-model"}, workspace, true, false)
	}()

	var output bytes.Buffer
	var outputMu sync.Mutex
	outputText := func() string {
		outputMu.Lock()
		defer outputMu.Unlock()
		return output.String()
	}
	readAvailable := func() error {
		buf := make([]byte, 4096)
		for {
			n, readErr := unix.Read(int(master.Fd()), buf)
			if n > 0 {
				output.Write(buf[:n])
			}
			if readErr != nil {
				if errors.Is(readErr, unix.EAGAIN) || errors.Is(readErr, unix.EWOULDBLOCK) {
					return nil
				}
				return readErr
			}
			if n == 0 {
				return nil
			}
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(outputText(), "\x1b[?1049h") && time.Now().Before(deadline) {
		if err := readAvailable(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = readAvailable()
	if !strings.Contains(outputText(), "\x1b[?1049h") {
		t.Fatalf("TUI did not enter alternate screen; output starts %q", outputText())
	}
	if !strings.Contains(outputText(), "\x1b[?1003h") && !strings.Contains(outputText(), "\x1b[?1002h") {
		t.Fatalf("TUI did not enable mouse reporting: %q", outputText())
	}
	raw, err := ptyInputIsRaw(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !raw {
		t.Fatal("PTY was not in raw input mode")
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: 40, Rows: 12}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for !strings.Contains(outputText(), "Buddy Agent · idle · /") && time.Now().Before(deadline) {
		if err := readAvailable(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = readAvailable()
	if !strings.Contains(outputText(), "Buddy Agent · idle · /") {
		t.Fatalf("resize did not render the compact layout: %q", outputText())
	}
	if _, err := master.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run TUI: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TUI did not exit after Ctrl+C")
	}
	_ = readAvailable()
	_ = master.Close()

	final, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !term.IsTerminal(int(slave.Fd())) {
		t.Fatal("PTY stopped being a terminal")
	}
	if !reflect.DeepEqual(original, final) {
		t.Fatalf("terminal state was not restored: before=%#v after=%#v", original, final)
	}
	text := outputText()
	for _, sequence := range []string{"\x1b[?1049l", "\x1b[?25h"} {
		if !strings.Contains(text, sequence) {
			t.Errorf("terminal cleanup omitted %q", sequence)
		}
	}
}

var _ tea.Model = (*conversationTUI)(nil)
