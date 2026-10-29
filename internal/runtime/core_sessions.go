package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneGraph/checkpoint"
)

func (c *Core) CreateSession(ctx context.Context, history json.RawMessage) (Session, error) {
	if c == nil || ctx == nil {
		return Session{}, errors.New("core and context are required")
	}
	c.mu.Lock()
	if c.closed || c.closing {
		c.mu.Unlock()
		return Session{}, errors.New("core is closing or closed")
	}
	c.sessionOps.Add(1)
	c.mu.Unlock()
	defer c.sessionOps.Done()
	return c.createSession(ctx, history, SessionSettings{ModelProfile: c.models.DefaultModel()})
}

func (c *Core) createSession(ctx context.Context, history json.RawMessage, settings SessionSettings) (Session, error) {
	if len(history) == 0 {
		history = json.RawMessage("null")
	}
	if !json.Valid(history) {
		return Session{}, errors.New("session history must be valid JSON")
	}
	id, err := newID()
	if err != nil {
		return Session{}, err
	}
	saved := Session{
		ID: id, Workspace: c.workspace, UpdatedAt: time.Now(), DataVersion: CurrentSessionDataVersion,
		Settings: settings, AgentUsage: map[string]UsageTotals{}, AgentUsageComplete: true,
		History: append(json.RawMessage(nil), history...),
	}
	if err := c.createSnapshot(ctx, saved); err != nil {
		return Session{}, err
	}
	return saved, nil
}

func (c *Core) LatestSession(ctx context.Context) (Session, bool, error) {
	if c == nil || ctx == nil {
		return Session{}, false, errors.New("core and context are required")
	}
	snapshot, err := c.sessions.Latest(ctx, c.workspace)
	if err != nil || snapshot == nil {
		return Session{}, false, err
	}
	saved, err := c.decodeSession(ctx, *snapshot)
	return saved, err == nil, err
}

func (c *Core) ListSessions(ctx context.Context, limit int, includeArchived bool) ([]Session, error) {
	if c == nil || ctx == nil {
		return nil, errors.New("core and context are required")
	}
	snapshots, err := c.sessions.List(ctx, c.workspace, limit, includeArchived)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(snapshots))
	for _, snapshot := range snapshots {
		saved, err := c.decodeSession(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, saved)
	}
	return sessions, nil
}

// ListSessionPage returns a page of sessions ordered from newest to oldest.
func (c *Core) ListSessionPage(ctx context.Context, offset, limit int, includeArchived bool) ([]Session, error) {
	if c == nil || ctx == nil {
		return nil, errors.New("core and context are required")
	}
	snapshots, err := c.sessions.ListPage(ctx, c.workspace, offset, limit, includeArchived)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, 0, len(snapshots))
	for _, snapshot := range snapshots {
		saved, err := c.decodeSession(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, saved)
	}
	return sessions, nil
}

func (c *Core) SetSessionTitle(ctx context.Context, sessionID, title string) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	title = strings.TrimSpace(title)
	if utf8.RuneCountInString(title) > 120 {
		return errors.New("session title must be at most 120 characters")
	}
	return c.withSessionLock(sessionID, func() error {
		return c.sessions.UpdateTitle(ctx, sessionID, c.workspace, title)
	})
}

func (c *Core) SetSessionArchived(ctx context.Context, sessionID string, archived bool) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	return c.withSessionLock(sessionID, func() error {
		return c.sessions.SetArchived(ctx, sessionID, c.workspace, archived)
	})
}

func (c *Core) DeleteSession(ctx context.Context, sessionID string) error {
	if c == nil || ctx == nil || sessionID == "" {
		return errors.New("core, context, and session id are required")
	}
	return c.withSessionLock(sessionID, func() error {
		return c.deleteSessionOwned(ctx, sessionID)
	})
}

func (c *Core) deleteSessionOwned(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closing {
		return errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) {
		return errors.New("cannot delete a session while its task is running")
	}
	if c.queueDraining[sessionID] {
		queued, err := c.sessions.QueuedTurns(ctx, sessionID)
		if err != nil {
			return err
		}
		// Opening an idle runtime also schedules a drain. Only actual queued
		// work can race with deletion; an empty drain owns no session work.
		if len(queued) > 0 {
			return errors.New("cannot delete a session while queued work is starting")
		}
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 35*time.Second)
	cleanupErr := c.commands.StopSession(cleanupCtx, sessionID)
	cleanupCancel()
	if cleanupErr != nil {
		return cleanupErr
	}
	// Remove the session row last so interrupted cleanup can be retried.
	if _, err := c.GetSession(ctx, sessionID); err != nil {
		return err
	}
	queuedTurns, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return err
	}
	parentID := c.runtime.CheckpointID(sessionID)
	delete(c.liveExecutions, parentID)
	if err := c.checkpoints.Delete(ctx, parentID); err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
		return fmt.Errorf("delete task checkpoint for session %q: %w", sessionID, err)
	}
	if err := c.sessions.DeleteCommands(ctx, sessionID); err != nil {
		return err
	}
	if err := c.sessions.DeleteToolInvocationsForSession(ctx, sessionID); err != nil {
		return err
	}
	if err := c.sessions.DeleteGraphResumeInputs(ctx, sessionID, parentID); err != nil {
		return err
	}
	deleted, err := c.sessions.Delete(ctx, sessionID, c.workspace)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("session %q does not exist in workspace", sessionID)
	}
	for _, queued := range queuedTurns {
		if waiter := c.turnWaiters[queued.ExecutionID]; waiter != nil {
			delete(c.turnWaiters, queued.ExecutionID)
			waiter <- turnResult{err: errors.New("session was deleted before the turn started")}
		}
	}
	if hub := c.removeEventHub(sessionID, nil); hub != nil {
		hub.close()
	}
	delete(c.interactiveSessions, sessionID)
	return nil
}

func (c *Core) RewindSession(ctx context.Context, sessionID string, userTurns int) (int, error) {
	if c == nil || ctx == nil || sessionID == "" {
		return 0, errors.New("core, context, and session id are required")
	}
	if userTurns <= 0 {
		return 0, errors.New("rewind count must be greater than zero")
	}
	var removed int
	err := c.withSessionLock(sessionID, func() error {
		var err error
		removed, err = c.rewindSessionOwned(ctx, sessionID, userTurns)
		return err
	})
	return removed, err
}

func (c *Core) rewindSessionOwned(ctx context.Context, sessionID string, userTurns int) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.closing {
		return 0, errors.New("core is closed")
	}
	if c.sessionIsRunningLocked(sessionID) {
		return 0, errors.New("cannot rewind a session while its task is running")
	}
	queued, err := c.sessions.QueuedTurns(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if len(queued) > 0 {
		return 0, errors.New("cannot rewind a session while turns are queued")
	}
	pending, err := c.hasPending(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	if pending {
		return 0, errors.New("cannot rewind a session while a task is paused")
	}
	saved, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	var history model.ResponseInput
	if len(saved.History) > 0 && string(saved.History) != "null" {
		if err := json.Unmarshal(saved.History, &history); err != nil {
			return 0, fmt.Errorf("decode session history: %w", err)
		}
	}
	items := responseHistoryItems(&history)
	var starts []int
	for index, item := range items {
		if message, ok := item.(model.InputMessage); ok && message.Role == model.RoleUser {
			starts = append(starts, index)
		}
	}
	if len(starts) == 0 {
		return 0, nil
	}
	removed := min(userTurns, len(starts))
	cut := starts[len(starts)-removed]
	encoded, err := json.Marshal(model.NewResponseInputItems(items[:cut]...))
	if err != nil {
		return 0, fmt.Errorf("encode rewound history: %w", err)
	}
	if err := c.sessions.UpdateHistory(ctx, sessionID, c.workspace, encoded); err != nil {
		return 0, err
	}
	return removed, nil
}

func (c *Core) GetSession(ctx context.Context, id string) (Session, error) {
	if c == nil || ctx == nil || id == "" {
		return Session{}, errors.New("core, context, and session id are required")
	}
	snapshot, err := c.sessions.Get(ctx, id, c.workspace)
	if err != nil {
		return Session{}, err
	}
	return c.decodeSession(ctx, *snapshot)
}

func (c *Core) SetSessionHistory(ctx context.Context, id string, history json.RawMessage) error {
	if c == nil || ctx == nil || id == "" {
		return errors.New("core, context, and session id are required")
	}
	if len(history) == 0 || !json.Valid(history) {
		return errors.New("session history must be valid JSON")
	}
	if _, err := model.NewResponseInput(history); err != nil {
		return err
	}
	return c.withSessionLock(id, func() error {
		return c.sessions.UpdateHistory(ctx, id, c.workspace, history)
	})
}

func (c *Core) setSessionConversation(ctx context.Context, id string, transcript, modelContext *model.ResponseInput, contextKey string, agentContext json.RawMessage) error {
	if c == nil || ctx == nil || id == "" {
		return errors.New("core, context, and session id are required")
	}
	if transcript == nil {
		transcript = model.NewResponseInputItems()
	}
	transcriptJSON, err := json.Marshal(transcript)
	if err != nil {
		return fmt.Errorf("encode session transcript: %w", err)
	}
	var contextJSON json.RawMessage = json.RawMessage("null")
	contextKeyToSave := contextKey
	if modelContext != nil {
		contextJSON, err = json.Marshal(modelContext)
		if err != nil {
			return fmt.Errorf("encode provider model context: %w", err)
		}
		if bytes.Equal(contextJSON, transcriptJSON) {
			// Avoid storing the same potentially large inline image payload twice.
			// A task rebuilds this provider context from History when it is absent.
			contextJSON = json.RawMessage("null")
			contextKeyToSave = ""
		}
	}
	return c.sessions.UpdateConversation(ctx, id, c.workspace, transcriptJSON, contextJSON, contextKeyToSave, agentContext)
}
