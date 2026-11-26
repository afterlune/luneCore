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
			"sessionId": review.Session.ID, "sourceSessionId": source.ID,
			"executionId": review.ExecutionID, "materialsDigest": review.MaterialsDigest,
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
		"sessionId": diagnosis.Session.ID, "sourceSessionId": source.ID,
		"executionId": diagnosis.ExecutionID, "evidenceDigest": diagnosis.EvidenceDigest,
	}), &diagnosisState)
	if diagnosisState.Stale || diagnosisState.TurnResult == nil || diagnosisState.TurnResult.Status != core.TaskCompleted {
		t.Fatalf("fresh diagnosis result = %+v", diagnosisState)
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
