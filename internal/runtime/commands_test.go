package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	commandruntime "github.com/afterlune/luneCore/internal/command"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

type commandProbe struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	ended   atomic.Bool
}

func newCommandProbe() *commandProbe {
	return &commandProbe{started: make(chan struct{}), release: make(chan struct{})}
}
func (p *commandProbe) Execute(ctx context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
	p.calls.Add(1)
	if request.OnOutput != nil {
		if err := request.OnOutput([]byte("private live output")); err != nil {
			return publictool.CommandResult{}, err
		}
	}
	close(p.started)
	defer p.ended.Store(true)
	select {
	case <-ctx.Done():
		return publictool.CommandResult{}, ctx.Err()
	case <-p.release:
	}
	if request.OnOutput != nil {
		if err := request.OnOutput([]byte("\nfinished output")); err != nil {
			return publictool.CommandResult{}, err
		}
	}
	code := 0
	return publictool.CommandResult{ExitCode: &code}, nil
}

type commandFlowProvider struct {
	mu    sync.Mutex
	step  int
	mode  string
	probe *commandProbe
	id    string
}

func (*commandFlowProvider) Name() string { return "host" }
func (p *commandFlowProvider) CreateResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	step := p.step
	p.step++
	switch step {
	case 0:
		return hostToolCallResponse("select-command", "select_tool_category", `{"category":"command"}`), nil
	case 1:
		return hostToolCallResponse("start-command", "command", `{"command":"private-command-text","yield_time_ms":0}`), nil
	case 2:
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.probe.started:
		}
		output := latestCommandOutput(request)
		p.id = output.Command.ID
		if p.id == "" || p.probe.ended.Load() {
			return nil, errors.New("command did not survive the startup tool call")
		}
		if p.mode == "ignore" {
			return hostTextResponse("Premature final answer."), nil
		}
		if p.mode == "question" {
			return askUserToolCallResponse("Continue after stopping the command?", []string{"yes"}), nil
		}
		args, _ := json.Marshal(map[string]any{"command_id": p.id, "wait_ms": 0})
		return hostToolCallResponse("poll-command", "command_status", string(args)), nil
	case 3:
		if p.mode == "ignore" {
			return hostTextResponse("Another premature final answer."), nil
		}
		if p.mode == "question" {
			return hostTextResponse("Command stopped for clarification."), nil
		}
		output := latestCommandOutput(request)
		if !strings.Contains(output.Output, "private live output") || !output.Command.Status.Active() {
			return nil, errors.New("incremental command output was not available")
		}
		close(p.probe.release)
		args, _ := json.Marshal(map[string]any{"command_id": p.id, "cursor": output.NextCursor, "wait_ms": 1000})
		return hostToolCallResponse("finish-command", "command_status", string(args)), nil
	case 4:
		if p.mode == "ignore" {
			args, _ := json.Marshal(map[string]any{"command_id": p.id, "wait_ms": 0})
			return hostToolCallResponse("inspect-stopped-command", "command_status", string(args)), nil
		}
		output := latestCommandOutput(request)
		if output.Command.Status != CommandSucceeded || output.Command.ExitCode == nil || *output.Command.ExitCode != 0 {
			return nil, errors.New("command completion was not confirmed")
		}
	}
	return hostTextResponse("Command result inspected."), nil
}
func (p *commandFlowProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, emit func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	response, err := p.CreateResponse(ctx, request, profile)
	if err != nil {
		return response, err
	}
	if emit != nil {
		if err := emit(publicmodel.ModelStreamEvent{Type: publicmodel.ModelStreamCompleted, Response: response}); err != nil {
			return response, err
		}
	}
	return response, nil
}
func latestCommandOutput(request publicmodel.ModelRequest) CommandOutput {
	if request.Input != nil {
		for i := len(request.Input.Items) - 1; i >= 0; i-- {
			if output, ok := request.Input.Items[i].(publicmodel.InputFunctionCallOutput); ok && output.Output.Text != nil {
				var result CommandOutput
				if json.Unmarshal([]byte(*output.Output.Text), &result) == nil && result.Command.ID != "" {
					return result
				}
			}
		}
	}
	return CommandOutput{}
}

func TestLongCommandRuntimeOutputAndTermination(t *testing.T) {
	probe := newCommandProbe()
	provider := &commandFlowProvider{probe: probe}
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	config.CommandExecutor = probe
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, "Run and inspect a command")})
	if err != nil || result.Status != TaskCompleted || probe.calls.Load() != 1 || !probe.ended.Load() {
		t.Fatalf("result=%+v calls=%d error=%v", result, probe.calls.Load(), err)
	}
	runtime, _ := client.OpenSessionRuntime(context.Background(), saved.ID)
	infos, err := runtime.ListCommands(context.Background())
	if err != nil || len(infos) != 1 || infos[0].Status != CommandSucceeded {
		t.Fatalf("commands=%+v error=%v", infos, err)
	}
	for n := 0; n < 2; n++ {
		output, err := runtime.StopCommand(context.Background(), infos[0].ID)
		if err != nil || output.Command.Status != CommandSucceeded || !strings.Contains(output.Output, "finished output") {
			t.Fatalf("stop=%+v error=%v", output, err)
		}
	}
	events, err := client.sessions.RuntimeEventsAfter(context.Background(), saved.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var started, finished bool
	for _, stored := range events {
		if strings.Contains(string(stored.Payload), "private-command-text") || strings.Contains(string(stored.Payload), "private live output") {
			t.Fatal("command text/output persisted in lifecycle events")
		}
		var event Event
		_ = json.Unmarshal(stored.Payload, &event)
		started = started || event.Kind == EventCommandStarted
		finished = finished || event.Kind == EventCommandFinished
	}
	if !started || !finished {
		t.Fatalf("started=%v finished=%v", started, finished)
	}
	if err := client.DeleteSession(context.Background(), saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ReadCommand(context.Background(), infos[0].ID, CommandReadOptions{}); err == nil {
		t.Fatal("deleted session command remains")
	}
}

func TestCoreClosePersistsTerminalCommandEvent(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	probe := newCommandProbe()
	provider := &commandFlowProvider{probe: probe}
	config := runtimeTestConfig(workspace, stateDir, provider)
	config.CommandExecutor = probe
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	identity := publictool.CallIdentity{SessionID: saved.ID, ExecutionID: "close-event-execution", AgentID: "lead", InvocationID: "close-event-call"}
	if err := client.commands.OpenScope(identity.ExecutionID, context.Background()); err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	ctx := publictool.WithCallIdentity(context.Background(), identity)
	if _, err := client.commands.Start(ctx, publictool.CommandRequest{Command: "close-event", Workspace: workspace}, 0, 0); err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	select {
	case <-probe.started:
	case <-time.After(time.Second):
		_ = client.Close()
		t.Fatal("command did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close Core: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Core.Close did not join the terminal command event")
	}

	reopened, err := New(config)
	if err != nil {
		t.Fatalf("reopen Core: %v", err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened Core: %v", err)
		}
	}()
	events, err := reopened.sessions.RuntimeEventsAfter(context.Background(), saved.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, stored := range events {
		var event Event
		if err := json.Unmarshal(stored.Payload, &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind == EventCommandFinished && event.CommandID != "" {
			return
		}
	}
	t.Fatal("command_finished event was not durable after Core.Close")
}

func TestRunningCommandsBlockCompletionAndSurviveRestartAsRecords(t *testing.T) {
	ctx := context.Background()
	probe := newCommandProbe()
	provider := &commandFlowProvider{probe: probe, mode: "ignore"}
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	config.CommandExecutor = probe
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunTask(ctx, TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, "Inspect the running command")})
	if !errors.Is(err, ErrTaskPaused) || result.Status != TaskPaused || !probe.ended.Load() {
		t.Fatalf("result=%+v error=%v ended=%v", result, err, probe.ended.Load())
	}
	runtime, _ := client.OpenSessionRuntime(ctx, saved.ID)
	requests, err := runtime.PendingRequests(ctx)
	if err != nil || len(requests) != 1 || requests[0].Blocker == nil {
		t.Fatalf("requests=%+v error=%v", requests, err)
	}
	queued, err := runtime.SubmitTurn(ctx, TurnRequest{Input: mustSettingsTaskInput(t, "next question")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		client.mu.RLock()
		draining := client.queueDraining[saved.ID]
		client.mu.RUnlock()
		if !draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("paused queue drain did not stop")
		}
		time.Sleep(time.Millisecond)
	}
	fork, err := client.ForkPausedTask(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	forkRuntime, _ := client.OpenSessionRuntime(ctx, fork.ID)
	forkCommands, err := forkRuntime.ListCommands(ctx)
	if err != nil || len(forkCommands) != 1 || forkCommands[0].ID != provider.id || forkCommands[0].SessionID != fork.ID || forkCommands[0].ExecutionID == result.ExecutionID || forkCommands[0].Status != CommandCancelled {
		t.Fatalf("fork commands=%+v error=%v", forkCommands, err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	client, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	runtime, _ = client.OpenSessionRuntime(ctx, saved.ID)
	output, err := runtime.ReadCommand(ctx, provider.id, CommandReadOptions{})
	if err != nil || output.Command.Status != CommandCancelled || !strings.Contains(output.Output, "private live output") {
		t.Fatalf("saved=%+v error=%v", output, err)
	}
	completed, err := client.ResumeTask(ctx, saved.ID)
	if err != nil || completed.Status != TaskCompleted || completed.ExecutionID != result.ExecutionID || probe.calls.Load() != 1 {
		t.Fatalf("resume=%+v calls=%d error=%v", completed, probe.calls.Load(), err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	next, err := runtime.WaitTurn(waitCtx, queued.ExecutionID)
	if err != nil || next.Status != TaskCompleted {
		t.Fatalf("queued=%+v error=%v", next, err)
	}
	forkRuntime, _ = client.OpenSessionRuntime(ctx, fork.ID)
	if _, err := forkRuntime.DiscardPaused(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestClarificationStopsCommandsWithoutReplay(t *testing.T) {
	probe := newCommandProbe()
	provider := &commandFlowProvider{probe: probe, mode: "question"}
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	config.CommandExecutor = probe
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, "Inspect a command and ask a question")})
	if !errors.Is(err, ErrTaskPaused) || !probe.ended.Load() {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	runtime, _ := client.OpenSessionRuntime(context.Background(), saved.ID)
	requests, err := runtime.PendingRequests(context.Background())
	if err != nil || len(requests) != 1 || requests[0].UserQuestion == nil {
		t.Fatalf("requests=%+v error=%v", requests, err)
	}
	question := requests[0].UserQuestion
	if err := runtime.ResolveUserQuestion(context.Background(), UserQuestionAnswer{ExecutionID: question.ExecutionID, RunID: question.RunID, InvocationID: question.InvocationID, CallID: question.CallID, Answer: "yes"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completed, err := runtime.WaitTurn(ctx, result.ExecutionID)
	if err != nil || completed.Status != TaskCompleted || probe.calls.Load() != 1 {
		t.Fatalf("completed=%+v calls=%d error=%v", completed, probe.calls.Load(), err)
	}
}

func TestCommandStartupMarksUnfinishedRecordsInterrupted(t *testing.T) {
	provider := &scriptedHostProvider{}
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	record := commandruntime.Record{Info: CommandInfo{ID: "old-command", SessionID: saved.ID, ExecutionID: "old-execution", AgentID: "lead", Status: CommandRunning}, Output: []byte("saved before crash"), End: 18}
	if err := client.sessions.SaveCommand(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	client, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	runtime, _ := client.OpenSessionRuntime(context.Background(), saved.ID)
	output, err := runtime.ReadCommand(context.Background(), record.Info.ID, CommandReadOptions{})
	if err != nil || output.Command.Status != CommandInterrupted || output.Output != "saved before crash" {
		t.Fatalf("restart=%+v error=%v", output, err)
	}
}
