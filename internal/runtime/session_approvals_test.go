package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	runtimepermission "github.com/afterlune/luneCore/internal/tool"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func TestSessionApprovalPersistenceAndForkIsolation(t *testing.T) {
	provider := &scriptedHostProvider{}
	workspace, stateDir := t.TempDir(), t.TempDir()
	config := Config{
		Workspace: workspace, StateDir: stateDir,
		Memory: MemoryConfig{Disabled: true},
		ModelConfig: &publicmodel.Config{
			Providers:      map[string]publicmodel.ProviderConfig{"host": {Protocol: publicmodel.ProtocolResponses}},
			Profiles:       map[string]publicmodel.ModelProfile{"host": {Provider: "host", Model: "test-model"}},
			DefaultProfile: "host",
		},
		ModelProviders: map[string]publicmodel.Provider{"host": provider},
	}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}

	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}

	store := runtimepermission.NewSessionApprovalStore(nil)
	ctx := publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{SessionID: session.ID})
	ctx = runtimepermission.WithSessionApprovalStore(ctx, store)

	toolReq := runtimepermission.PermissionRequest{
		Tool:      "custom_exec",
		Arguments: json.RawMessage(`{"target": "build"}`),
	}

	// 1. Denial should not persist
	approvalID, resultChan := beginSessionToolApproval(t, client, session.ID, ctx, toolReq)
	if err := client.resolveApproval(session.ID, approvalID, false); err != nil {
		t.Fatal(err)
	}
	if allowed := awaitApprovalResult(t, resultChan); allowed {
		t.Fatal("expected approval to be denied")
	}

	saved, err := client.GetSession(context.Background(), session.ID)
	if err != nil || len(saved.Approvals) != 0 {
		t.Fatalf("denial persisted approvals %#v, error %v", saved.Approvals, err)
	}

	// 2. Allowance should persist and be in memory store
	approvalID, resultChan = beginSessionToolApproval(t, client, session.ID, ctx, toolReq)
	if err := client.resolveApproval(session.ID, approvalID, true); err != nil {
		t.Fatal(err)
	}
	if allowed := awaitApprovalResult(t, resultChan); !allowed {
		t.Fatal("expected approval to be allowed")
	}

	saved, err = client.GetSession(context.Background(), session.ID)
	if err != nil || len(saved.Approvals) != 1 || saved.Approvals[0].Tool != "custom_exec" {
		t.Fatalf("saved approvals = %#v, error %v", saved.Approvals, err)
	}
	if !store.Allows("custom_exec", json.RawMessage(`{"target":"build"}`)) {
		t.Fatal("in-memory store did not record approved tool")
	}

	// 3. Fork does not inherit approvals
	forked, err := client.ForkSession(context.Background(), session.ID)
	if err != nil || len(forked.Approvals) != 0 {
		t.Fatalf("fork inherited approvals: %#v", forked.Approvals)
	}

	// 4. New session starts empty
	created, err := client.CreateSession(context.Background(), nil)
	if err != nil || len(created.Approvals) != 0 {
		t.Fatalf("new session approvals = %#v, error %v", created.Approvals, err)
	}

	// 5. Reopened Core keeps approvals
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restored, err := reopened.GetSession(context.Background(), session.ID)
	if err != nil || len(restored.Approvals) != 1 || restored.Approvals[0].Tool != "custom_exec" {
		t.Fatalf("reopened approvals = %#v, error %v", restored.Approvals, err)
	}
}

func TestAutoReviewToolRequest(t *testing.T) {
	safeResponse := hostTextResponse(`{"safe": true, "reason": "standard build task"}`)
	unsafeResponse := hostTextResponse(`{"safe": false, "reason": "destructive operation"}`)
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{safeResponse, unsafeResponse}}
	client := newProgrammaticCore(t, provider)

	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}

	ctx := publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{SessionID: session.ID})

	// 1. Safe tool call -> auto-review returns safe=true and saves approval
	toolReq := runtimepermission.PermissionRequest{
		Tool:      "command",
		Arguments: json.RawMessage(`{"command": "go build ./..."}`),
	}
	safe, err := client.autoReviewToolRequest(ctx, toolReq)
	if err != nil {
		t.Fatalf("autoReviewToolRequest failed: %v", err)
	}
	if !safe {
		t.Fatal("expected auto-review to approve safe tool call")
	}

	saved, err := client.GetSession(context.Background(), session.ID)
	if err != nil || len(saved.Approvals) != 1 || saved.Approvals[0].Tool != "command" {
		t.Fatalf("saved approvals after safe review = %#v, error %v", saved.Approvals, err)
	}

	// 2. Unsafe tool call -> auto-review returns safe=false and does NOT save approval
	unsafeToolReq := runtimepermission.PermissionRequest{
		Tool:      "command",
		Arguments: json.RawMessage(`{"command": "rm -rf /"}`),
	}
	safe, err = client.autoReviewToolRequest(ctx, unsafeToolReq)
	if err != nil {
		t.Fatalf("autoReviewToolRequest failed: %v", err)
	}
	if safe {
		t.Fatal("expected auto-review to reject unsafe tool call")
	}

	saved, err = client.GetSession(context.Background(), session.ID)
	if err != nil || len(saved.Approvals) != 1 {
		t.Fatalf("approvals count should remain 1, got %#v", saved.Approvals)
	}
}

func beginSessionToolApproval(t *testing.T, client *Core, sessionID string, ctx context.Context, request runtimepermission.PermissionRequest) (string, <-chan bool) {
	t.Helper()
	result := make(chan bool, 1)
	go func() { result <- client.requestApproval(ctx, request) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client.mu.RLock()
		for id, pending := range client.approvals {
			if pending.sessionID == sessionID {
				client.mu.RUnlock()
				return id, result
			}
		}
		client.mu.RUnlock()
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatal("tool approval was not registered")
	return "", nil
}
