package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	internaltool "github.com/afterlune/luneCore/internal/tool"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

const stateLockTestEnv = "LUNECORE_STATE_LOCK_HELPER_DIR"

func TestStateDirectoryLockSubprocessHelper(t *testing.T) {
	stateDir := os.Getenv(stateLockTestEnv)
	if stateDir == "" {
		return
	}
	_, err := New(Config{StateDir: stateDir})
	if !errors.Is(err, ErrStateDirInUse) {
		t.Fatalf("New with locked StateDir error = %v, want ErrStateDirInUse", err)
	}
}

func TestStateDirectoryLockIsExclusiveAndReleased(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	config := stateLockTestConfig(workspace, stateDir)
	first, err := New(config)
	if err != nil {
		t.Fatalf("New first Core: %v", err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Errorf("close first Core: %v", err)
		}
	}()

	if _, err := New(config); !errors.Is(err, ErrStateDirInUse) {
		t.Fatalf("New second Core with same StateDir error = %v, want ErrStateDirInUse", err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestStateDirectoryLockSubprocessHelper$")
	command.Env = append(os.Environ(), stateLockTestEnv+"="+stateDir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("subprocess lock check failed: %v\n%s", err, output)
	}

	otherConfig := stateLockTestConfig(workspace, t.TempDir())
	other, err := New(otherConfig)
	if err != nil {
		t.Fatalf("New Core with a different StateDir: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("close Core with a different StateDir: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first Core: %v", err)
	}
	reopened, err := New(config)
	if err != nil {
		t.Fatalf("New after releasing StateDir lock: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened Core: %v", err)
	}
}

func TestStateDirectoryLockReleasedWhenNewFails(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	invalid := stateLockTestConfig(workspace, stateDir)
	invalid.Tools = []publictool.Registration{{}}
	if _, err := New(invalid); err == nil {
		t.Fatal("New with an invalid tool registration succeeded")
	}

	valid, err := New(stateLockTestConfig(workspace, stateDir))
	if err != nil {
		t.Fatalf("New after failed initialization: %v", err)
	}
	if err := valid.Close(); err != nil {
		t.Fatalf("close Core after failed initialization: %v", err)
	}
}

func TestCancelTaskStopsProviderAndResumesSameExecution(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 2), release: make(chan struct{})}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	events := subscribeTestEvents(t, client, saved.ID)
	input, err := NewTaskInput("wait for the provider")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: input})
		done <- err
	}()

	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("task did not reach provider")
	}
	started, ok := client.StateForSession(saved.ID)
	if !ok || started.Status != TaskRunning {
		t.Fatalf("task state before cancellation = %#v, ok=%t", started, ok)
	}
	if err := client.Close(); err == nil {
		t.Fatal("Close succeeded while a task was running")
	}
	if !interruptTestTask(t, client, saved.ID) {
		t.Fatal("CancelTask returned false for a running task")
	}
	if !interruptTestTask(t, client, saved.ID) {
		state, ok := client.StateForSession(saved.ID)
		if !ok || state.Status != TaskPaused {
			t.Fatalf("repeated interrupt returned false while task was still active: %#v", state)
		}
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunTask error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunTask did not stop after cancellation")
	}
	paused, ok := client.StateForSession(saved.ID)
	if !ok || paused.Status != TaskPaused || paused.ExecutionID != started.ExecutionID {
		t.Fatalf("task state after cancellation = %#v, want paused with execution %q", paused, started.ExecutionID)
	}
	if interruptTestTask(t, client, saved.ID) {
		t.Fatal("CancelTask returned true after the task exited")
	}
	pausedEventTimer := time.NewTimer(5 * time.Second)
	defer pausedEventTimer.Stop()
	for {
		select {
		case event := <-events:
			if event.Kind == EventTaskFinished && event.SessionID == saved.ID && event.Status == TaskPaused {
				goto resume
			}
		case <-pausedEventTimer.C:
			t.Fatal("missing EventTaskFinished with paused status")
		}
	}

resume:
	close(provider.release)
	if _, err := client.ResumeTask(context.Background(), saved.ID); err != nil {
		t.Fatalf("ResumeTask after cancellation: %v", err)
	}
	resumed, ok := client.StateForSession(saved.ID)
	if !ok || resumed.Status != TaskCompleted || resumed.ExecutionID != started.ExecutionID {
		t.Fatalf("resumed task state = %#v, want completed with execution %q", resumed, started.ExecutionID)
	}
}

func TestCancelTaskOnlyStopsSelectedSession(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 2), release: make(chan struct{}, 1)}
	client := newProgrammaticCore(t, provider)
	first, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	start := func(sessionID, prompt string, done chan<- error) {
		input, err := NewTaskInput(prompt)
		if err != nil {
			done <- err
			return
		}
		_, err = client.RunTask(ctx, TaskRequest{SessionID: sessionID, Input: input})
		done <- err
	}
	go start(first.ID, "first session", firstDone)
	go start(second.ID, "second session", secondDone)
	for range 2 {
		select {
		case <-provider.entered:
		case <-ctx.Done():
			t.Fatal("both tasks did not reach the provider")
		}
	}
	if !interruptTestTask(t, client, first.ID) {
		t.Fatal("CancelTask returned false for the first session")
	}
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first RunTask error = %v, want context.Canceled", err)
		}
	case <-ctx.Done():
		t.Fatal("first session did not stop")
	}
	provider.release <- struct{}{}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second RunTask: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("second session did not finish")
	}
	firstState, _ := client.StateForSession(first.ID)
	secondState, _ := client.StateForSession(second.ID)
	if firstState.Status != TaskPaused || secondState.Status != TaskCompleted {
		t.Fatalf("task states after selective cancellation: first=%q second=%q", firstState.Status, secondState.Status)
	}
}

func TestFinishingTaskDoesNotClearNextSessionTaskCancellation(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 2), release: make(chan struct{}, 1)}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	var eventMu sync.Mutex
	var firstTaskID string
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	clientEvents := subscribeTestEvents(t, client, saved.ID)
	go func() {
		for event := range clientEvents {
			eventMu.Lock()
			if event.Kind == EventTaskStarted && firstTaskID == "" {
				firstTaskID = event.TaskID
			}
			startSecond := event.Kind == EventTaskFinished && event.TaskID == firstTaskID
			eventMu.Unlock()
			if !startSecond {
				continue
			}
			go func() {
				input, _ := NewTaskInput("second task")
				_, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: input})
				secondDone <- err
			}()
			select {
			case <-provider.entered:
				close(secondStarted)
			case <-time.After(5 * time.Second):
				t.Error("second task did not reach the provider from the finish callback")
			}
		}
	}()

	firstDone := make(chan error, 1)
	input, _ := NewTaskInput("first task")
	go func() {
		_, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: input})
		firstDone <- err
	}()
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first task did not reach the provider")
	}
	provider.release <- struct{}{}
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first task: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first task did not finish")
	}
	select {
	case <-secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("finish callback did not start the second task")
	}
	if state, ok := client.StateForSession(saved.ID); !ok || state.Status != TaskRunning {
		t.Fatalf("second task state after first returned = %#v, ok=%t", state, ok)
	}
	if !interruptTestTask(t, client, saved.ID) {
		t.Fatal("first task cleanup removed the second task's cancellation handle")
	}
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second task error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second task did not stop after cancellation")
	}
}

type contextBlockingTool struct{ started chan struct{} }

func (tool contextBlockingTool) Definition() publictool.Definition {
	return publicmodel.FunctionTool{
		Name: "context_block", Description: "Wait until the runtime cancels the call.",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

func (tool contextBlockingTool) Run(ctx context.Context, _ json.RawMessage) publictool.Result {
	close(tool.started)
	<-ctx.Done()
	return publictool.Result{Err: ctx.Err()}
}

func TestCancelTaskPropagatesToHostTool(t *testing.T) {
	blockingTool := contextBlockingTool{started: make(chan struct{})}
	client := newProgrammaticCore(t, &scriptedHostProvider{}, publictool.Registration{
		Tool: blockingTool,
		Metadata: publictool.Metadata{
			Category:        publictool.Category{Name: "host", Description: "Host supplied tools."},
			PermissionClass: publictool.PermissionWorkspaceRead,
			ReadOnly:        true,
		},
	})
	if err := client.SetPermissionMode(PermissionFullAccess); err != nil {
		t.Fatal(err)
	}
	taskCtx, cancel := context.WithCancel(context.Background())
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	const executionID = "tool-execution"
	taskCtx = publictool.WithCallIdentity(taskCtx, publictool.CallIdentity{SessionID: saved.ID, TaskID: "tool-task", ExecutionID: executionID})
	client.mu.Lock()
	client.activeTasks[saved.ID] = TaskState{Status: TaskRunning, SessionID: saved.ID, TaskID: "tool-task", ExecutionID: executionID}
	client.taskCancels[saved.ID] = cancel
	client.mu.Unlock()
	defer func() {
		cancel()
		client.mu.Lock()
		state := client.activeTasks[saved.ID]
		state.Status = TaskIdle
		client.activeTasks[saved.ID] = state
		delete(client.taskCancels, saved.ID)
		client.mu.Unlock()
	}()
	done := make(chan internaltool.Result, 1)
	go func() {
		done <- client.registry.Run(taskCtx, "context_block", json.RawMessage(`{}`))
	}()
	select {
	case <-blockingTool.started:
	case <-time.After(5 * time.Second):
		t.Fatal("host tool did not start")
	}
	if !interruptTestTask(t, client, saved.ID) {
		t.Fatal("CancelTask returned false for the active host tool")
	}
	select {
	case result := <-done:
		if !errors.Is(result.Err, context.Canceled) {
			t.Fatalf("host tool error = %v, want context.Canceled", result.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("host tool did not observe task cancellation")
	}
	cancel()
}

func TestCancelTaskCancelsPendingApproval(t *testing.T) {
	client := newProgrammaticCore(t, &scriptedHostProvider{})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	taskCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := subscribeTestEvents(t, client, saved.ID)
	const executionID = "approval-execution"
	identity := publictool.CallIdentity{SessionID: saved.ID, TaskID: "approval-task", ExecutionID: executionID, AgentID: "lead"}
	taskCtx = publictool.WithCallIdentity(taskCtx, identity)
	client.mu.Lock()
	client.activeTasks[saved.ID] = TaskState{Status: TaskRunning, SessionID: saved.ID, TaskID: identity.TaskID, ExecutionID: executionID}
	client.taskCancels[saved.ID] = cancel
	client.mu.Unlock()
	defer func() {
		client.mu.Lock()
		state := client.activeTasks[saved.ID]
		state.Status = TaskIdle
		client.activeTasks[saved.ID] = state
		delete(client.taskCancels, saved.ID)
		client.mu.Unlock()
	}()
	approvalDone := make(chan bool, 1)
	go func() {
		approvalDone <- client.requestApproval(taskCtx, toolPermissionRequestForTest())
	}()
	var approvalID string
	select {
	case event := <-events:
		if event.Kind != EventApprovalRequired || event.Approval == nil {
			t.Fatalf("event while awaiting approval = %#v", event)
		}
		approvalID = event.Approval.ID
	case <-time.After(5 * time.Second):
		t.Fatal("approval request was not emitted")
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := runtime.PendingRequests(context.Background())
	if err != nil || len(pending) != 1 || pending[0].Approval == nil || pending[0].Approval.ID != approvalID {
		t.Fatalf("pending approval snapshot = %#v, error=%v", pending, err)
	}
	if !interruptTestTask(t, client, saved.ID) {
		t.Fatal("CancelTask returned false while approval was pending")
	}
	select {
	case approved := <-approvalDone:
		if approved {
			t.Fatal("canceled approval was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approval wait did not stop after cancellation")
	}
	select {
	case event := <-events:
		if event.Kind != EventApprovalCancelled || event.ApprovalID != approvalID {
			t.Fatalf("approval cancellation event = %#v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approval cancellation event was not emitted")
	}
}

func stateLockTestConfig(workspace, stateDir string) Config {
	return Config{
		Workspace: workspace, StateDir: stateDir,
		ModelConfig: &publicmodel.Config{
			Providers:    map[string]publicmodel.ProviderConfig{"host": {Protocol: publicmodel.ProtocolResponses}},
			Models:       map[string]publicmodel.ModelProfile{"host": {Provider: "host", Model: "test-model"}},
			DefaultModel: "host",
		},
		ModelProviders: map[string]publicmodel.Provider{"host": &scriptedHostProvider{}},
	}
}

func toolPermissionRequestForTest() internaltool.PermissionRequest {
	return internaltool.PermissionRequest{Tool: "test-tool"}
}
