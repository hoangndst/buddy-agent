package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/client"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/daemon"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/spf13/cobra"
)

// NewCmdAttach creates the 'attach' command to connect to an active daemon session.
func NewCmdAttach(f *cmdutil.Factory) *cobra.Command {
	var socketPath string

	cmd := &cobra.Command{
		Use:   "attach <session_id>",
		Short: "Attach to an active or historical daemon session",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			sessionID := args[0]
			if socketPath == "" {
				socketPath = daemon.GetDefaultSocketPath()
			}

			cl, err := client.Connect(socketPath)
			if err != nil {
				return fmt.Errorf("failed to connect to daemon at %s: %w", socketPath, err)
			}
			defer func() { _ = cl.Close() }()

			attachRes, err := cl.AttachSession(protocol.SessionAttachParams{
				SessionID:   sessionID,
				LastEventID: 0,
				TakeControl: true,
			})
			if err != nil {
				return fmt.Errorf("failed to attach to session %s: %w", sessionID, err)
			}

			_, _ = fmt.Fprintf(f.IOStreams.Out, "Attached to session %s as %s (Active: %t)\n", sessionID, attachRes.Role, attachRes.Active)
			_, _ = fmt.Fprintln(f.IOStreams.Out, "Press Ctrl+C to detach (task continues executing in background).")

			// Handle Ctrl+C for clean detach
			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			inputScanner := bufio.NewScanner(f.IOStreams.In)
			permCh := cl.Permissions()
			eventsCh := cl.Events()

			for {
				select {
				case <-sigCh:
					_, _ = fmt.Fprintf(f.IOStreams.Out, "\nDetaching from session %s. Task continues in background.\n", sessionID)
					_ = cl.DetachSession(sessionID)
					return nil

				case req, ok := <-permCh:
					if !ok {
						permCh = nil
						continue
					}
					_, _ = fmt.Fprintf(f.IOStreams.Out, "\n%s Tool %s requests permission with arguments: %+v\nAllow? [y/N]: ",
						toolStyle.Render("⚡ [PERMISSION REQUIRED]"), req.Tool, req.Arguments)
					decision := "deny"
					if inputScanner.Scan() {
						text := strings.TrimSpace(strings.ToLower(inputScanner.Text()))
						if text == "y" || text == "yes" {
							decision = "allow"
						}
					}
					_ = cl.RespondPermission(req.RequestID, decision)

				case evt, ok := <-eventsCh:
					if !ok {
						return fmt.Errorf("connection to daemon closed unexpectedly before session completed")
					}
					renderEvent(f, evt)
					if evt.Type == string(agent.EventFinished) {
						return nil
					}
					if evt.Type == string(agent.EventError) {
						if term, _ := evt.Data["terminal"].(bool); term {
							errMsg := evt.Content
							if errMsg == "" && evt.Data != nil {
								if e, ok := evt.Data["error"].(string); ok {
									errMsg = e
								}
							}
							return fmt.Errorf("task failed: %s", errMsg)
						}
					}
				}
			}
		},
	}

	cmd.Flags().StringVar(&socketPath, "socket", "", "Path to the Unix domain socket")
	return cmd
}

func renderEvent(f *cmdutil.Factory, evt protocol.SessionEventNotification) {
	switch agent.EventType(evt.Type) {
	case agent.EventReasoning:
		if evt.Content != "" {
			_, _ = fmt.Fprintf(f.IOStreams.Out, "%s\n", thoughtStyle.Render(evt.Content))
		}
	case agent.EventToolCall:
		toolName, _ := evt.Data["tool_name"].(string)
		_, _ = fmt.Fprintf(f.IOStreams.Out, "%s\n", toolStyle.Render(fmt.Sprintf("⚙ %s", toolName)))
	case agent.EventToolResult:
		res := ""
		if evt.Data != nil {
			if tr, ok := evt.Data["tool_result"].(string); ok && strings.TrimSpace(tr) != "" {
				res = tr
			}
		}
		if res == "" {
			res = evt.Content
		}
		if strings.TrimSpace(res) != "" {
			trimmed := strings.TrimSpace(res)
			if len(trimmed) > 300 {
				trimmed = trimmed[:300] + "..."
			}
			label := strings.TrimSpace(evt.Content)
			if strings.HasPrefix(label, "[subagent:") && label != trimmed {
				_, _ = fmt.Fprintf(f.IOStreams.Out, "%s %s\n", thoughtStyle.Render(label), resultStyle.Render(trimmed))
			} else {
				_, _ = fmt.Fprintf(f.IOStreams.Out, "%s\n", resultStyle.Render(trimmed))
			}
		}
	case agent.EventFinished:
		_, _ = fmt.Fprintf(f.IOStreams.Out, "\n%s %s\n", doneStyle.Render("✓ Execution completed:"), evt.Content)
	case agent.EventError:
		errMsg := evt.Content
		if errMsg == "" && evt.Data != nil {
			if e, ok := evt.Data["error"].(string); ok {
				errMsg = e
			}
		}
		isTerminal, _ := evt.Data["terminal"].(bool)
		if isTerminal {
			_, _ = fmt.Fprintf(f.IOStreams.Out, "\n%s %s\n", errStyle.Render("✗ Execution failed:"), errMsg)
		} else {
			_, _ = fmt.Fprintf(f.IOStreams.Out, "%s %s\n", thoughtStyle.Render("⚠ Subagent notice:"), errMsg)
		}
	}
}
