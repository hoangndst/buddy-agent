package cmd

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/persistence"
)

func runConversationStandalone(ctx context.Context, f *cmdutil.Factory, continueLatest bool) error {
	engine, cleanup, err := setupEngine(ctx, f)
	if err != nil {
		return err
	}
	defer cleanup()
	cfg, err := f.ConfigLoader()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	workspace := "."
	model := "unknown"
	maxSteps := 0
	if cfg != nil {
		if cfg.WorkspaceDir != "" {
			workspace = cfg.WorkspaceDir
		}
		if cfg.Model != "" {
			model = cfg.Model
		}
		maxSteps = cfg.MaxSteps
	}
	store := engine.PersistenceStore()
	var runtime *agent.ConversationRuntime
	conversationID := ""
	if continueLatest {
		if store == nil {
			return fmt.Errorf("persistence store is required to continue a conversation")
		}
		conversations, err := store.ListConversations(ctx, workspace, 1)
		if err != nil {
			return fmt.Errorf("list workspace conversations: %w", err)
		}
		if len(conversations) == 0 {
			return fmt.Errorf("no conversations found in workspace %q; run bare buddy to start one", workspace)
		}
		conversationID = conversations[0].ID
		runtime, err = agent.RestoreConversationRuntime(ctx, engine, store, conversationID)
		if err != nil {
			return fmt.Errorf("restore conversation %s: %w", conversationID, err)
		}
		if err := recoverStandaloneConversation(ctx, store, conversationID); err != nil {
			return fmt.Errorf("recover conversation %s: %w", conversationID, err)
		}
		if err := runStandalonePending(ctx, store, runtime, conversationID, f); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(f.IOStreams.Out, "Continuing conversation %s\n", conversationID)
	}
	_, _ = fmt.Fprintln(f.IOStreams.Out, doneStyle.Render("=== buddy-agent conversation ready ==="))
	_, _ = fmt.Fprintf(f.IOStreams.Out, "Model: %s | Workspace: %s\n", model, workspace)
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
		if strings.HasPrefix(prompt, "/resume") {
			return fmt.Errorf("use buddy --continue to resume the latest conversation in standalone mode")
		}
		mode := persistence.SubmissionQueue
		if strings.HasPrefix(prompt, "/steer ") {
			mode = persistence.SubmissionSteer
			prompt = strings.TrimSpace(strings.TrimPrefix(prompt, "/steer "))
		}
		if store == nil {
			return fmt.Errorf("persistence store is required for multi-turn conversations")
		}
		if runtime == nil {
			conversationID = newConversationID("conversation")
			turnID, submissionID := newConversationID("turn"), newConversationID("submission")
			fingerprint := persistence.SubmissionFingerprint(prompt, persistence.SubmissionQueue)
			now := time.Now().UTC()
			_, err := store.CreateConversation(ctx, &persistence.ConversationStartRequest{
				Session: persistence.SessionRecord{ID: conversationID, CreatedAt: now, UpdatedAt: now, Status: persistence.StatusRunning, Task: prompt, Model: model, WorkingDir: workspace, MaxSteps: maxSteps},
				StartID: newConversationID("start"), PayloadFingerprint: fingerprint,
				FirstSubmission: persistence.SubmissionRecord{SubmissionID: submissionID, Prompt: prompt, Mode: persistence.SubmissionQueue, PayloadFingerprint: fingerprint, TurnID: &turnID},
			})
			if err != nil {
				return fmt.Errorf("create conversation: %w", err)
			}
			historySession, err := engine.NewHistorySession(prompt)
			if err != nil {
				return err
			}
			runtime, err = agent.NewConversationRuntime(ctx, engine, store, conversationID, submissionID, historySession)
			if err != nil {
				return err
			}
			if _, err := runtime.RunTurn(ctx, submissionID, turnID); err != nil {
				_, _ = fmt.Fprintf(f.IOStreams.ErrOut, "%s\n", errStyle.Render(err.Error()))
			}
			continue
		}
		submissionID, turnID := newConversationID("submission"), newConversationID("turn")
		accepted, err := store.AdmitSubmission(ctx, &persistence.SubmissionRecord{ConversationID: conversationID, SubmissionID: submissionID, Prompt: prompt, Mode: mode})
		if err != nil {
			return fmt.Errorf("admit conversation prompt: %w", err)
		}
		if _, err := runtime.RunTurn(ctx, accepted.SubmissionID, turnID); err != nil {
			_, _ = fmt.Fprintf(f.IOStreams.ErrOut, "%s\n", errStyle.Render(err.Error()))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func recoverStandaloneConversation(ctx context.Context, store persistence.Store, conversationID string) error {
	turns, err := store.ListConversationTurns(ctx, conversationID)
	if err != nil {
		return err
	}
	interrupted := make(map[string]struct{})
	now := time.Now().UTC()
	for _, turn := range turns {
		if turn.Status != persistence.StatusRunning {
			continue
		}
		message := "standalone process exited before the turn completed"
		turn.Status, turn.FinishedAt, turn.ErrorMessage = persistence.StatusInterrupted, &now, &message
		if err := store.UpdateConversationTurn(ctx, turn); err != nil {
			return err
		}
		interrupted[turn.TurnID] = struct{}{}
	}
	submissions, err := store.ListSubmissions(ctx, conversationID)
	if err != nil {
		return err
	}
	for _, submission := range submissions {
		if submission.Status == persistence.SubmissionAttached && submission.TurnID != nil {
			if _, ok := interrupted[*submission.TurnID]; ok {
				if err := store.UpdateSubmissionStatus(ctx, conversationID, submission.SubmissionID, persistence.SubmissionConsumed, *submission.TurnID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func runStandalonePending(ctx context.Context, store persistence.Store, runtime *agent.ConversationRuntime, conversationID string, f *cmdutil.Factory) error {
	submissions, err := store.ListSubmissions(ctx, conversationID)
	if err != nil {
		return err
	}
	for _, submission := range submissions {
		if submission.Status != persistence.SubmissionQueued && submission.Status != persistence.SubmissionSteerPending {
			continue
		}
		turnID := newConversationID("turn")
		if _, err := runtime.RunTurn(ctx, submission.SubmissionID, turnID); err != nil {
			_, _ = fmt.Fprintf(f.IOStreams.ErrOut, "%s\n", errStyle.Render(err.Error()))
		}
	}
	return nil
}
