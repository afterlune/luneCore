package rpcserver

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/model"
)

func TestWorkspaceReviewAndDiagnosisRPCs(t *testing.T) {
	workspace := t.TempDir()
	git := exec.Command("git", "init", "-q", workspace)
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("initialize review workspace: %v: %s", err, output)
	}
	changedFile := filepath.Join(workspace, "notes.txt")
	if err := os.WriteFile(changedFile, []byte("initial review evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedRPCProvider{}
	runtimeCore, err := core.New(core.Config{
		Workspace: workspace, StateDir: t.TempDir(), Memory: core.MemoryConfig{Disabled: true},
		ModelConfig: &model.Config{
			Providers:      map[string]model.ProviderConfig{"fixture": {Protocol: model.ProtocolResponses}},
			Profiles:       map[string]model.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
		ModelProviders: map[string]model.Provider{"fixture": provider},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeCore.SetPermissionMode(core.PermissionFullAccess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeCore.Close(context.Background()) })
	service, err := New(runtimeCore, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(service)
	t.Cleanup(server.Close)
	client := dialTestRPC(t, server.URL, "test-token", server.Certificate())
	t.Cleanup(client.close)

	var source sessionSummary
	decodeResult(t, client.call(t, "session.create", map[string]any{}), &source)
	var review struct {
		Changed         bool           `json:"changed"`
		Session         sessionSummary `json:"session"`
		ExecutionID     string         `json:"executionId"`
		MaterialsDigest string         `json:"materialsDigest"`
	}
	decodeResult(t, client.call(t, "review.start", map[string]any{"sourceSessionId": source.ID}), &review)
	if !review.Changed || review.Session.ID == "" || review.Session.ID == source.ID || review.ExecutionID == "" || len(review.MaterialsDigest) != 64 {
		t.Fatalf("review start result = %+v", review)
	}
	var reviewTurn core.TurnResult
	decodeResult(t, client.call(t, "turn.wait", map[string]any{"sessionId": review.Session.ID, "executionId": review.ExecutionID}), &reviewTurn)
	if reviewTurn.Status != core.TaskCompleted {
		t.Fatalf("review turn status = %q", reviewTurn.Status)
	}
	provider.mu.Lock()
	if len(provider.requests) == 0 {
		provider.mu.Unlock()
		t.Fatal("review did not call the model")
	}
	for _, item := range provider.requests[0].Tools {
		if function, ok := item.(model.FunctionTool); ok && (function.Name == "write_file" || function.Name == "apply_patch") {
			provider.mu.Unlock()
			t.Fatalf("read-only review exposed writable tool %q", function.Name)
		}
	}
	provider.mu.Unlock()

	var reviewState struct {
		Stale      bool             `json:"stale"`
		TurnResult *core.TurnResult `json:"turnResult"`
	}
	getReview := func() {
		t.Helper()
		decodeResult(t, client.call(t, "review.get", map[string]any{
			"sessionId": review.Session.ID,
		}), &reviewState)
	}
	getReview()
	if reviewState.Stale || reviewState.TurnResult == nil || reviewState.TurnResult.Status != core.TaskCompleted {
		t.Fatalf("fresh review result = %+v", reviewState)
	}
	if err := os.WriteFile(changedFile, []byte("changed review evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	getReview()
	if !reviewState.Stale {
		t.Fatal("review did not become stale after the evidence changed")
	}

	var diagnosis struct {
		Session        sessionSummary `json:"session"`
		ExecutionID    string         `json:"executionId"`
		EvidenceDigest string         `json:"evidenceDigest"`
	}
	decodeResult(t, client.call(t, "diagnosis.start", map[string]any{"sourceSessionId": source.ID}), &diagnosis)
	if diagnosis.Session.ID == "" || diagnosis.Session.ID == source.ID || diagnosis.ExecutionID == "" || len(diagnosis.EvidenceDigest) != 64 {
		t.Fatalf("diagnosis start result = %+v", diagnosis)
	}
	var diagnosisTurn core.TurnResult
	decodeResult(t, client.call(t, "turn.wait", map[string]any{"sessionId": diagnosis.Session.ID, "executionId": diagnosis.ExecutionID}), &diagnosisTurn)
	if diagnosisTurn.Status != core.TaskCompleted {
		t.Fatalf("diagnosis turn status = %q", diagnosisTurn.Status)
	}
	var diagnosisState struct {
		Stale      bool             `json:"stale"`
		TurnResult *core.TurnResult `json:"turnResult"`
	}
	decodeResult(t, client.call(t, "diagnosis.get", map[string]any{
		"sessionId": diagnosis.Session.ID,
	}), &diagnosisState)
	if diagnosisState.Stale || diagnosisState.TurnResult == nil || diagnosisState.TurnResult.Status != core.TaskCompleted {
		t.Fatalf("fresh diagnosis result = %+v", diagnosisState)
	}
}

func TestAnalysisLinkSurvivesClientsRestartAndFollowup(t *testing.T) {
	workspace := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", workspace).CombinedOutput(); err != nil {
		t.Fatalf("initialize workspace: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("review this evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	provider := &scriptedRPCProvider{responses: []*model.ModelResponse{
		rpcTextResponseWithText("original review report"),
		rpcTextResponseWithText("later follow-up response"),
	}}
	newCore := func() *core.Core {
		t.Helper()
		runtimeCore, err := core.New(core.Config{
			Workspace: workspace, StateDir: stateDir, Memory: core.MemoryConfig{Disabled: true},
			ModelConfig: &model.Config{
				Providers:      map[string]model.ProviderConfig{"fixture": {Protocol: model.ProtocolResponses}},
				Profiles:       map[string]model.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
				DefaultProfile: "fixture",
			},
			ModelProviders: map[string]model.Provider{"fixture": provider},
		})
		if err != nil {
			t.Fatal(err)
		}
		return runtimeCore
	}

	runtimeCore := newCore()
	service, err := New(runtimeCore, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(service)
	clientA := dialTestRPC(t, server.URL, "test-token", server.Certificate())
	clientB := dialTestRPC(t, server.URL, "test-token", server.Certificate())
	var source sessionSummary
	decodeResult(t, clientA.call(t, "session.create", map[string]any{}), &source)
	var review struct {
		Session     sessionSummary `json:"session"`
		ExecutionID string         `json:"executionId"`
	}
	decodeResult(t, clientA.call(t, "review.start", map[string]any{"sourceSessionId": source.ID}), &review)
	var initial core.TurnResult
	decodeResult(t, clientA.call(t, "turn.wait", map[string]any{"sessionId": review.Session.ID, "executionId": review.ExecutionID}), &initial)
	if initial.Status != core.TaskCompleted {
		t.Fatalf("initial review status = %q", initial.Status)
	}
	if review.Session.Analysis == nil || review.Session.Analysis.Kind != "review" || review.Session.Analysis.SourceSessionID != source.ID || review.Session.Analysis.ExecutionID != review.ExecutionID {
		t.Fatalf("review session association = %+v", review.Session.Analysis)
	}
	var restored sessionSummary
	decodeResult(t, clientB.call(t, "session.get", map[string]any{"sessionId": review.Session.ID}), &restored)
	if restored.Analysis == nil || restored.Analysis.ExecutionID != review.ExecutionID {
		t.Fatalf("independent client restored association = %+v", restored.Analysis)
	}
	type getResult struct {
		Stale      bool             `json:"stale"`
		Report     string           `json:"report"`
		TurnResult *core.TurnResult `json:"turnResult"`
	}
	getReview := func() getResult {
		t.Helper()
		var result getResult
		decodeResult(t, clientB.call(t, "review.get", map[string]any{"sessionId": review.Session.ID}), &result)
		return result
	}
	first := getReview()
	if first.Stale || first.Report != "original review report" || first.TurnResult == nil || first.TurnResult.ExecutionID != review.ExecutionID {
		t.Fatalf("review result restored by session id = %+v", first)
	}
	var followup core.TurnSubmission
	decodeResult(t, clientA.call(t, "turn.submit", map[string]any{
		"sessionId": review.Session.ID, "submissionId": "analysis-followup", "input": "continue the discussion", "mode": "start_if_idle",
	}), &followup)
	var followupResult core.TurnResult
	decodeResult(t, clientA.call(t, "turn.wait", map[string]any{"sessionId": review.Session.ID, "executionId": followup.ExecutionID}), &followupResult)
	if followupResult.Status != core.TaskCompleted || followupResult.ExecutionID == review.ExecutionID {
		t.Fatalf("follow-up result = %+v", followupResult)
	}
	if got := getReview(); got.Report != "original review report" || got.TurnResult == nil || got.TurnResult.ExecutionID != review.ExecutionID {
		t.Fatalf("follow-up replaced original analysis report: %+v", got)
	}
	if mismatch := clientB.call(t, "review.get", map[string]any{"sessionId": review.Session.ID, "sourceSessionId": "another-session"}); mismatch.Error == nil || !strings.Contains(mismatch.Error.Message, "does not match saved") {
		t.Fatalf("mismatched source session was not rejected: %+v", mismatch.Error)
	}
	var diagnosis struct {
		Session     sessionSummary `json:"session"`
		ExecutionID string         `json:"executionId"`
	}
	decodeResult(t, clientA.call(t, "diagnosis.start", map[string]any{"sourceSessionId": source.ID}), &diagnosis)
	var diagnosisTurn core.TurnResult
	decodeResult(t, clientA.call(t, "turn.wait", map[string]any{"sessionId": diagnosis.Session.ID, "executionId": diagnosis.ExecutionID}), &diagnosisTurn)
	if diagnosisTurn.Status != core.TaskCompleted || diagnosis.Session.Analysis == nil || diagnosis.Session.Analysis.Kind != "diagnosis" {
		t.Fatalf("diagnosis link/result = %+v / %+v", diagnosis.Session.Analysis, diagnosisTurn)
	}

	clientA.close()
	clientB.close()
	server.Close()
	if err := runtimeCore.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtimeCore = newCore()
	t.Cleanup(func() { _ = runtimeCore.Close(context.Background()) })
	service, err = New(runtimeCore, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	server = httptest.NewTLSServer(service)
	t.Cleanup(server.Close)
	reopenedClient := dialTestRPC(t, server.URL, "test-token", server.Certificate())
	t.Cleanup(reopenedClient.close)
	var reopened sessionSummary
	decodeResult(t, reopenedClient.call(t, "session.get", map[string]any{"sessionId": review.Session.ID}), &reopened)
	if reopened.Analysis == nil || reopened.Analysis.SourceSessionID != source.ID || reopened.Analysis.ExecutionID != review.ExecutionID {
		t.Fatalf("reopened session association = %+v", reopened.Analysis)
	}
	var afterRestart getResult
	decodeResult(t, reopenedClient.call(t, "review.get", map[string]any{"sessionId": review.Session.ID}), &afterRestart)
	if afterRestart.Report != "original review report" || afterRestart.TurnResult == nil || afterRestart.TurnResult.ExecutionID != review.ExecutionID {
		t.Fatalf("original report after restart = %+v", afterRestart)
	}
	var afterRestartDiagnosis struct {
		Stale           bool             `json:"stale"`
		SourceAvailable bool             `json:"sourceAvailable"`
		Report          string           `json:"report"`
		TurnResult      *core.TurnResult `json:"turnResult"`
	}
	decodeResult(t, reopenedClient.call(t, "diagnosis.get", map[string]any{"sessionId": diagnosis.Session.ID}), &afterRestartDiagnosis)
	if afterRestartDiagnosis.Stale || afterRestartDiagnosis.Report != "fixture response" || afterRestartDiagnosis.TurnResult == nil || afterRestartDiagnosis.TurnResult.ExecutionID != diagnosis.ExecutionID {
		t.Fatalf("diagnosis restored by id after restart = %+v", afterRestartDiagnosis)
	}

	var legacy sessionSummary
	decodeResult(t, reopenedClient.call(t, "session.create", map[string]any{}), &legacy)
	legacyReply := reopenedClient.call(t, "review.get", map[string]any{
		"sessionId": legacy.ID, "sourceSessionId": source.ID,
		"executionId": "", "materialsDigest": review.Session.Analysis.EvidenceDigest,
	})
	if legacyReply.Error != nil {
		t.Fatalf("legacy review lookup failed: %+v", legacyReply.Error)
	}
	if err := runtimeCore.DeleteSession(context.Background(), source.ID); err != nil {
		t.Fatal(err)
	}
	var deletedSource struct {
		Stale           bool   `json:"stale"`
		SourceAvailable bool   `json:"sourceAvailable"`
		Report          string `json:"report"`
	}
	decodeResult(t, reopenedClient.call(t, "review.get", map[string]any{"sessionId": review.Session.ID}), &deletedSource)
	if !deletedSource.Stale || deletedSource.SourceAvailable {
		t.Fatalf("deleted source session status = %+v", deletedSource)
	}
	var deletedDiagnosis struct {
		Stale           bool `json:"stale"`
		SourceAvailable bool `json:"sourceAvailable"`
	}
	decodeResult(t, reopenedClient.call(t, "diagnosis.get", map[string]any{"sessionId": diagnosis.Session.ID}), &deletedDiagnosis)
	if !deletedDiagnosis.Stale || deletedDiagnosis.SourceAvailable {
		t.Fatalf("deleted diagnosis source status = %+v", deletedDiagnosis)
	}
}

func rpcTextResponseWithText(text string) *model.ModelResponse {
	return &model.ModelResponse{Status: "completed", Output: []model.ResponseOutputItem{
		model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{model.OutputText{Text: text}}},
	}}
}

func TestAnalysisLookupDoesNotInventReportForUnfinishedTurn(t *testing.T) {
	workspace := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", workspace).CombinedOutput(); err != nil {
		t.Fatalf("initialize workspace: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("pending evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &rpcBarrierProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	runtimeCore, err := core.New(core.Config{
		Workspace: workspace, StateDir: t.TempDir(), Memory: core.MemoryConfig{Disabled: true},
		ModelConfig: &model.Config{
			Providers:      map[string]model.ProviderConfig{"fixture": {Protocol: model.ProtocolResponses}},
			Profiles:       map[string]model.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
		ModelProviders: map[string]model.Provider{"fixture": provider},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(provider.release); _ = runtimeCore.Close(context.Background()) })
	service, err := New(runtimeCore, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(service)
	t.Cleanup(server.Close)
	client := dialTestRPC(t, server.URL, "test-token", server.Certificate())
	t.Cleanup(client.close)
	var source sessionSummary
	decodeResult(t, client.call(t, "session.create", map[string]any{}), &source)
	var review struct {
		Session     sessionSummary `json:"session"`
		ExecutionID string         `json:"executionId"`
	}
	decodeResult(t, client.call(t, "review.start", map[string]any{"sourceSessionId": source.ID}), &review)
	select {
	case <-provider.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("analysis provider was not started")
	}
	var result struct {
		Runtime    core.RuntimeState `json:"runtime"`
		TurnResult *core.TurnResult  `json:"turnResult"`
		Report     string            `json:"report"`
	}
	decodeResult(t, client.call(t, "review.get", map[string]any{"sessionId": review.Session.ID}), &result)
	if result.Runtime.Task.Status != core.TaskRunning || result.TurnResult != nil || result.Report != "" {
		t.Fatalf("unfinished analysis inspection = %+v", result)
	}
}

func TestTurnReplaceHistoryIsAtomicAndIdempotent(t *testing.T) {
	provider := &scriptedRPCProvider{}
	runtimeCore, err := core.New(core.Config{
		Workspace: t.TempDir(), StateDir: t.TempDir(), Memory: core.MemoryConfig{Disabled: true},
		ModelConfig: &model.Config{
			Providers:      map[string]model.ProviderConfig{"fixture": {Protocol: model.ProtocolResponses}},
			Profiles:       map[string]model.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
		ModelProviders: map[string]model.Provider{"fixture": provider},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeCore.Close(context.Background()) })
	service, err := New(runtimeCore, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(service)
	t.Cleanup(server.Close)
	client := dialTestRPC(t, server.URL, "test-token", server.Certificate())
	t.Cleanup(client.close)

	var source sessionSummary
	decodeResult(t, client.call(t, "session.create", map[string]any{"history": []map[string]string{
		{"type": "message", "role": "user", "content": "keep this turn"},
		{"type": "message", "role": "assistant", "content": "keep this answer"},
		{"type": "message", "role": "user", "content": "replace this turn"},
		{"type": "message", "role": "assistant", "content": "discard this answer"},
	}}), &source)
	var preview struct {
		Digest  string `json:"digest"`
		Removed int    `json:"removed"`
	}
	decodeResult(t, client.call(t, "session.rewindPreview", map[string]any{"sessionId": source.ID, "turns": 1}), &preview)
	if preview.Removed != 1 || len(preview.Digest) != 64 {
		t.Fatalf("rewind preview = %+v", preview)
	}
	request := map[string]any{
		"sessionId": source.ID, "submissionId": "replace-once", "turns": 1,
		"historyDigest": preview.Digest, "input": "replacement turn", "mode": "start_if_idle",
	}
	var first core.TurnSubmission
	decodeResult(t, client.call(t, "turn.replaceHistory", request), &first)
	if first.ExecutionID == "" || first.ReplacedUserTurns != 1 {
		t.Fatalf("replacement result = %+v", first)
	}
	var turn core.TurnResult
	decodeResult(t, client.call(t, "turn.wait", map[string]any{"sessionId": source.ID, "executionId": first.ExecutionID}), &turn)
	if turn.Status != core.TaskCompleted {
		t.Fatalf("replacement task status = %q", turn.Status)
	}
	var retry core.TurnSubmission
	decodeResult(t, client.call(t, "turn.replaceHistory", request), &retry)
	if retry.ExecutionID != first.ExecutionID {
		t.Fatalf("idempotent retry returned execution %q, want %q", retry.ExecutionID, first.ExecutionID)
	}
	var history json.RawMessage
	decodeResult(t, client.call(t, "session.history", map[string]any{"sessionId": source.ID}), &history)
	if !strings.Contains(string(history), "keep this turn") || !strings.Contains(string(history), "replacement turn") || !strings.Contains(string(history), "fixture response") || strings.Contains(string(history), "replace this turn") || strings.Contains(string(history), "discard this answer") {
		t.Fatalf("history after replacement = %s", history)
	}
	stale := map[string]any{
		"sessionId": source.ID, "submissionId": "stale-edit", "turns": 1,
		"historyDigest": preview.Digest, "input": "must not be accepted", "mode": "start_if_idle",
	}
	if reply := client.call(t, "turn.replaceHistory", stale); reply.Error == nil || !strings.Contains(reply.Error.Message, "history changed") {
		t.Fatalf("stale replacement reply = %+v", reply)
	}
	var after json.RawMessage
	decodeResult(t, client.call(t, "session.history", map[string]any{"sessionId": source.ID}), &after)
	if string(after) != string(history) {
		t.Fatalf("stale replacement changed history: before=%s after=%s", history, after)
	}
}
