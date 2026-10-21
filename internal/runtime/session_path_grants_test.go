package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	runtimepermission "github.com/afterlune/luneCore/internal/tool"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func TestExternalReadApprovalPersistsAndIsReusedByRuntime(t *testing.T) {
	workspace, stateDir, external := t.TempDir(), t.TempDir(), t.TempDir()
	file := filepath.Join(external, "private.txt")
	if err := os.WriteFile(file, []byte("approved content"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostToolCallResponse("read-one", "read_file", `{"path":`+quoteSessionGrantPath(file)+`}`),
		hostTextResponse("First read complete."),
		hostToolCallResponse("read-two", "read_file", `{"path":`+quoteSessionGrantPath(filepath.Join(external, "private.txt"))+`}`),
		hostTextResponse("Second read complete."),
	}}
	client, err := New(runtimeTestConfig(workspace, stateDir, provider))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.SetPermissionMode(PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	saved, err := client.CreateSession(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "read external file"), PermissionMode: PermissionReadOnly}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	approval := waitPathGrantApproval(t, runtime)
	if approval.PathGrant == nil || approval.PathGrant.Path != external || approval.PathGrant.Access != publictool.PathRead {
		t.Fatalf("path approval = %#v", approval)
	}
	if err := runtime.ResolveApproval(approval.ID, true); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.WaitTurn(context.Background(), first.ExecutionID)
	if err != nil || result.Status != TaskCompleted {
		t.Fatalf("first read result = %#v, error %v", result, err)
	}
	stored, err := client.GetSession(context.Background(), saved.ID)
	if err != nil || len(stored.PathGrants) != 1 || stored.PathGrants[0].Path != external {
		t.Fatalf("stored path grants = %#v, error %v", stored.PathGrants, err)
	}

	second, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "read it again"), PermissionMode: PermissionReadOnly}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	waitForProviderRequests(t, provider, 3)
	if requests, err := runtime.PendingRequests(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		for _, request := range requests {
			if request.Approval != nil {
				t.Fatalf("reused session grant prompted again: %#v", request.Approval)
			}
		}
	}
	result, err = runtime.WaitTurn(context.Background(), second.ExecutionID)
	if err != nil || result.Status != TaskCompleted {
		t.Fatalf("second read result = %#v, error %v", result, err)
	}
}

func waitPathGrantApproval(t *testing.T, runtime *SessionRuntime) ApprovalRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		requests, err := runtime.PendingRequests(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, request := range requests {
			if request.Approval != nil {
				return *request.Approval
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	saved, _ := runtime.core.GetSession(context.Background(), runtime.sessionID)
	t.Fatalf("external directory approval was not requested; mode=%s grants=%#v history=%s", runtime.core.PermissionMode(), saved.PathGrants, saved.History)
	return ApprovalRequest{}
}

func waitForProviderRequests(t *testing.T, provider *scriptedHostProvider, minimum int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		provider.mu.Lock()
		requests := provider.requests
		provider.mu.Unlock()
		if requests >= minimum {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("provider received fewer than %d requests", minimum)
}

func quoteSessionGrantPath(path string) string {
	encoded, _ := json.Marshal(path)
	return string(encoded)
}

func TestSessionPathGrantDenialRetryPersistenceAndForkIsolation(t *testing.T) {
	workspace, stateDir, external := t.TempDir(), t.TempDir(), t.TempDir()
	config := runtimeTestConfig(workspace, stateDir, &scriptedHostProvider{})
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = client.Close()
		}
	})
	session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	store := runtimepermission.NewSessionPathGrantStore(nil)
	ctx := publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{SessionID: session.ID, TaskID: "task", ExecutionID: "execution", AgentID: "lead"})
	ctx = runtimepermission.WithSessionPathGrantStore(ctx, store)
	grant := publictool.PathGrant{Path: external, Access: publictool.PathRead}
	request := toolPermissionRequestWithPathGrant(grant)

	approvalID, approvalResult := beginPathGrantApproval(t, client, session.ID, ctx, request)
	if err := client.resolveApproval(session.ID, approvalID, false); err != nil {
		t.Fatal(err)
	}
	if approved := awaitApprovalResult(t, approvalResult); approved {
		t.Fatal("denied grant returned approval")
	}
	saved, err := client.GetSession(context.Background(), session.ID)
	if err != nil || len(saved.PathGrants) != 0 {
		t.Fatalf("denial persisted grants %#v, error %v", saved.PathGrants, err)
	}
	if store.Allows(grant) {
		t.Fatal("denied grant entered the live session store")
	}

	approvalID, approvalResult = beginPathGrantApproval(t, client, session.ID, ctx, request)
	if err := client.resolveApproval(session.ID, approvalID, true); err != nil {
		t.Fatal(err)
	}
	if approved := awaitApprovalResult(t, approvalResult); !approved {
		t.Fatal("approved grant returned false")
	}
	if !store.Allows(grant) {
		t.Fatal("approved grant was not added to the live session store")
	}
	saved, err = client.GetSession(context.Background(), session.ID)
	if err != nil || len(saved.PathGrants) != 1 || saved.PathGrants[0] != grant {
		t.Fatalf("saved grants = %#v, error %v", saved.PathGrants, err)
	}

	forked, err := client.ForkSession(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(forked.PathGrants) != 0 {
		t.Fatalf("fork inherited path grants: %#v", forked.PathGrants)
	}
	created, err := client.CreateSession(context.Background(), nil)
	if err != nil || len(created.PathGrants) != 0 {
		t.Fatalf("new session grants = %#v, error %v", created.PathGrants, err)
	}

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restored, err := reopened.GetSession(context.Background(), session.ID)
	if err != nil || len(restored.PathGrants) != 1 || restored.PathGrants[0] != grant {
		t.Fatalf("reopened grants = %#v, error %v", restored.PathGrants, err)
	}
	if err := reopened.DeleteSession(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.GetSession(context.Background(), session.ID); err == nil {
		t.Fatal("deleted session still exists")
	}
}

func toolPermissionRequestWithPathGrant(grant publictool.PathGrant) runtimepermission.PermissionRequest {
	return runtimepermission.PermissionRequest{Tool: "read_file", Arguments: json.RawMessage(`{"path":"secret.txt"}`), PathGrant: &grant}
}

func beginPathGrantApproval(t *testing.T, client *Core, sessionID string, ctx context.Context, request runtimepermission.PermissionRequest) (string, <-chan bool) {
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
	t.Fatal("path grant approval was not registered")
	return "", nil
}

func awaitApprovalResult(t *testing.T, response <-chan bool) bool {
	t.Helper()
	select {
	case result := <-response:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("approval did not return")
		return false
	}
}
