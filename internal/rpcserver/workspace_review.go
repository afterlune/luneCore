package rpcserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/internal/workspacereview"
)

type workspaceReviewStartParams struct {
	SourceSessionID   string `json:"sourceSessionId"`
	SourceExecutionID string `json:"sourceExecutionId,omitempty"`
	Path              string `json:"path,omitempty"`
}

type workspaceReviewStartResult struct {
	Changed           bool           `json:"changed"`
	Session           sessionSummary `json:"session,omitempty"`
	ExecutionID       string         `json:"executionId,omitempty"`
	SourceSessionID   string         `json:"sourceSessionId"`
	SourceExecutionID string         `json:"sourceExecutionId,omitempty"`
	Path              string         `json:"path,omitempty"`
	MaterialsDigest   string         `json:"materialsDigest,omitempty"`
}

func (c *connection) startWorkspaceReview(raw json.RawMessage) (any, *rpcError) {
	var params workspaceReviewStartParams
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	params.SourceSessionID = strings.TrimSpace(params.SourceSessionID)
	params.SourceExecutionID = strings.TrimSpace(params.SourceExecutionID)
	if params.SourceSessionID == "" {
		return nil, &rpcError{Code: -32602, Message: "sourceSessionId is required"}
	}

	source, err := c.server.core.GetSession(c.ctx, params.SourceSessionID)
	if err != nil {
		return nil, applicationError(err)
	}
	materials, err := workspacereview.CollectMaterialsSnapshot(c.server.core.Workspace(), params.Path)
	if err != nil {
		return nil, applicationError(err)
	}
	if !materials.Changed {
		return workspaceReviewStartResult{Changed: false, SourceSessionID: source.ID, SourceExecutionID: params.SourceExecutionID, Path: params.Path}, nil
	}

	prompt := workspacereview.Prompt(materials.Text, params.Path)
	link := "\n\nReview source conversation ID: `" + source.ID + "`."
	if params.SourceExecutionID != "" {
		link += " Source execution ID: `" + params.SourceExecutionID + "`."
	}
	prompt += link + " This is an independent review; do not continue or alter the source conversation."

	reviewSession, executionID, err := c.startReadOnlyAnalysis(source, "Review · ", prompt)
	if err != nil {
		return nil, applicationError(err)
	}
	return workspaceReviewStartResult{
		Changed: true, Session: reviewSession, ExecutionID: executionID,
		SourceSessionID: source.ID, SourceExecutionID: params.SourceExecutionID, Path: params.Path,
		MaterialsDigest: materials.Digest,
	}, nil
}

func (c *connection) startReadOnlyAnalysis(source core.Session, titlePrefix, prompt string) (sessionSummary, string, error) {
	reviewSession, err := c.server.core.CreateSession(c.ctx, nil)
	if err != nil {
		return sessionSummary{}, "", err
	}
	keepSession := false
	defer func() {
		if !keepSession {
			_ = c.server.core.DeleteSession(context.Background(), reviewSession.ID)
		}
	}()
	if err := c.server.core.SetSessionSettings(c.ctx, reviewSession.ID, source.Settings); err != nil {
		return sessionSummary{}, "", err
	}
	titleSource := strings.TrimSpace(source.Title)
	if titleSource == "" {
		titleSource = source.ID
	}
	if len([]rune(titleSource)) > 100 {
		titleSource = string([]rune(titleSource)[:100])
	}
	if err := c.server.core.SetSessionTitle(c.ctx, reviewSession.ID, titlePrefix+titleSource); err != nil {
		return sessionSummary{}, "", err
	}
	input, err := json.Marshal(prompt)
	if err != nil {
		return sessionSummary{}, "", err
	}
	var submissionNonce [24]byte
	if _, err := rand.Read(submissionNonce[:]); err != nil {
		return sessionSummary{}, "", err
	}
	submissionID := "analysis-" + hex.EncodeToString(submissionNonce[:])
	digestInput, err := json.Marshal(struct {
		SessionID    string          `json:"sessionId"`
		SubmissionID string          `json:"submissionId"`
		Input        json.RawMessage `json:"input"`
	}{reviewSession.ID, submissionID, input})
	if err != nil {
		return sessionSummary{}, "", err
	}
	submissionDigest := sha256.Sum256(digestInput)
	runtime, err := c.server.core.OpenSessionRuntime(c.ctx, reviewSession.ID)
	if err != nil {
		return sessionSummary{}, "", err
	}
	submission, submitErr := runtime.SubmitTurnWithID(c.ctx, core.TurnRequest{
		Input:          core.TaskInput{Raw: input},
		PermissionMode: core.PermissionReadOnly,
	}, core.StartIfIdle, submissionID, submissionDigest[:])
	closeErr := runtime.Close(context.Background())
	if err := errors.Join(submitErr, closeErr); err != nil {
		return sessionSummary{}, "", err
	}
	keepSession = true
	if updated, err := c.server.core.GetSession(c.ctx, reviewSession.ID); err == nil {
		reviewSession = updated
	}
	return summarizeSession(reviewSession), submission.ExecutionID, nil
}

func (c *connection) getWorkspaceReview(raw json.RawMessage) (any, *rpcError) {
	var params struct {
		SessionID       string `json:"sessionId"`
		SourceSessionID string `json:"sourceSessionId"`
		ExecutionID     string `json:"executionId,omitempty"`
		Path            string `json:"path,omitempty"`
		MaterialsDigest string `json:"materialsDigest"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	if strings.TrimSpace(params.SessionID) == "" || strings.TrimSpace(params.SourceSessionID) == "" || len(params.MaterialsDigest) != 64 {
		return nil, &rpcError{Code: -32602, Message: "sessionId, sourceSessionId, and materialsDigest are required"}
	}
	saved, err := c.server.core.GetSession(c.ctx, params.SessionID)
	if err != nil {
		return nil, applicationError(err)
	}
	materials, err := workspacereview.CollectMaterialsSnapshot(c.server.core.Workspace(), params.Path)
	if err != nil {
		return nil, applicationError(err)
	}
	currentDigest := materials.Digest
	var sourceTitle string
	if source, sourceErr := c.server.core.GetSession(c.ctx, params.SourceSessionID); sourceErr == nil {
		sourceTitle = source.Title
	}
	runtime, err := c.server.core.OpenSessionRuntimeReadOnly(c.ctx, params.SessionID)
	if err != nil {
		return nil, applicationError(err)
	}
	state, stateErr := runtime.State(c.ctx)
	var turnResult *core.TurnResult
	if params.ExecutionID != "" {
		if result, found, resultErr := runtime.GetTurnResult(c.ctx, params.ExecutionID); resultErr != nil {
			stateErr = errors.Join(stateErr, resultErr)
		} else if found {
			turnResult = &result
		}
	}
	closeErr := runtime.Close(context.Background())
	if err := errors.Join(stateErr, closeErr); err != nil {
		return nil, applicationError(err)
	}
	return map[string]any{
		"session": summarizeSession(saved), "runtime": state, "history": saved.History,
		"turnResult":      turnResult,
		"sourceSessionId": params.SourceSessionID, "sourceTitle": sourceTitle,
		"materialsDigest": params.MaterialsDigest, "currentMaterialsDigest": currentDigest,
		"stale": currentDigest != params.MaterialsDigest,
	}, nil
}
