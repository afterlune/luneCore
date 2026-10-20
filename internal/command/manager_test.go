package command

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	publictool "github.com/afterlune/luneCore/tool"
)

type memoryStore struct {
	mu      sync.Mutex
	records map[string][]byte
	fail    bool
}

func (s *memoryStore) SaveCommand(_ context.Context, r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("injected save error")
	}
	data, err := json.Marshal(r)
	s.records[r.Info.SessionID+"/"+r.Info.ID] = data
	return err
}
func (s *memoryStore) GetCommand(_ context.Context, sessionID, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var record Record
	data, ok := s.records[sessionID+"/"+id]
	if !ok {
		return record, errors.New("not found")
	}
	err := json.Unmarshal(data, &record)
	return record, err
}
func (s *memoryStore) ListCommands(_ context.Context, sessionID, executionID string) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var records []Record
	for _, data := range s.records {
		var r Record
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		if (sessionID == "" || r.Info.SessionID == sessionID) && (executionID == "" || r.Info.ExecutionID == executionID) {
			records = append(records, r)
		}
	}
	return records, nil
}
func (s *memoryStore) InterruptCommands(ctx context.Context) error {
	records, err := s.ListCommands(ctx, "", "")
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Info.Status.Active() {
			r.Info.Status = publictool.CommandInterrupted
			if err := s.SaveCommand(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

type executorFunc func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error)

func (f executorFunc) Execute(ctx context.Context, r publictool.CommandRequest) (publictool.CommandResult, error) {
	return f(ctx, r)
}
func identityContext() context.Context {
	return publictool.WithCallIdentity(context.Background(), publictool.CallIdentity{SessionID: "session", ExecutionID: "execution", AgentID: "lead", InvocationID: "invocation"})
}
func managerForTest(t *testing.T, executor publictool.CommandExecutor, options publictool.CommandOptions) (*Manager, *memoryStore) {
	t.Helper()
	store := &memoryStore{records: map[string][]byte{}}
	m, err := New(context.Background(), store, executor, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.OpenScope("execution", context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.mu.Lock()
		store.fail = false
		store.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return m, store
}

func TestCommandShortAndLegacyOutput(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		m, _ := managerForTest(t, executorFunc(func(_ context.Context, r publictool.CommandRequest) (publictool.CommandResult, error) {
			if streaming {
				if err := r.OnOutput([]byte("output")); err != nil {
					return publictool.CommandResult{}, err
				}
			}
			code := 0
			return publictool.CommandResult{Output: "output", ExitCode: &code}, nil
		}), publictool.CommandOptions{})
		result, err := m.Start(identityContext(), publictool.CommandRequest{Command: "test"}, 0, time.Second)
		if err != nil || result.Command.Status != publictool.CommandSucceeded || result.Output != "output" || result.Command.ExitCode == nil || *result.Command.ExitCode != 0 {
			t.Fatalf("result=%+v error=%v", result, err)
		}
	}
}

func TestManagerCloseWaitsForTerminalObserver(t *testing.T) {
	store := &memoryStore{records: map[string][]byte{}}
	observerStarted := make(chan struct{})
	releaseObserver := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseObserver) }) }
	defer release()
	code := 0
	m, err := New(context.Background(), store, executorFunc(func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error) {
		return publictool.CommandResult{ExitCode: &code}, nil
	}), publictool.CommandOptions{}, func(info publictool.CommandInfo) {
		if info.Status.Active() {
			return
		}
		close(observerStarted)
		<-releaseObserver
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.OpenScope("execution", context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(identityContext(), publictool.CommandRequest{Command: "quick"}, 0, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observerStarted:
	case <-time.After(time.Second):
		t.Fatal("terminal observer did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want observer wait deadline", err)
	}
	release()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close after terminal observer returned: %v", err)
	}
}

func TestCommandSurvivesToolContextAndStopsIdempotently(t *testing.T) {
	entered := make(chan struct{})
	m, _ := managerForTest(t, executorFunc(func(ctx context.Context, r publictool.CommandRequest) (publictool.CommandResult, error) {
		_ = r.OnOutput([]byte("live"))
		close(entered)
		<-ctx.Done()
		return publictool.CommandResult{}, ctx.Err()
	}), publictool.CommandOptions{})
	ctx, cancel := context.WithCancel(identityContext())
	result, err := m.Start(ctx, publictool.CommandRequest{Command: "long"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-entered
	if !m.Active("execution", "lead") {
		t.Fatal("tool context cancelled the task command")
	}
	if _, err := m.Read(context.Background(), "other", result.Command.ID, "", "", publictool.CommandReadOptions{}); err == nil {
		t.Fatal("cross-session read allowed")
	}
	if _, err := m.Stop(context.Background(), "session", result.Command.ID, "execution", "worker"); err == nil {
		t.Fatal("cross-agent stop allowed")
	}
	for n := 0; n < 2; n++ {
		stopped, err := m.Stop(context.Background(), "session", result.Command.ID, "execution", "lead")
		if err != nil || stopped.Command.Status != publictool.CommandCancelled || stopped.Output != "live" {
			t.Fatalf("stop=%+v error=%v", stopped, err)
		}
	}
}

func TestBackgroundCommandSurvivesTaskAndCanBeStoppedInLaterTask(t *testing.T) {
	started := make(chan struct{})
	manager, _ := managerForTest(t, executorFunc(func(ctx context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
		if !request.Background {
			t.Error("background request flag was lost before execution")
		}
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			t.Error("background command received an implicit timeout")
		}
		close(started)
		<-ctx.Done()
		return publictool.CommandResult{}, ctx.Err()
	}), publictool.CommandOptions{})

	request := publictool.CommandRequest{Command: "service", Background: true}
	startedCommand, err := manager.Start(identityContext(), request, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background command did not start")
	}
	if !startedCommand.Command.Background {
		t.Fatal("background command metadata was not returned")
	}

	if err := manager.StopScope(context.Background(), "execution", ""); err != nil {
		t.Fatal(err)
	}
	if manager.Active("execution", "lead") {
		t.Fatal("session background service was counted as a task-blocking command")
	}

	if err := manager.OpenScope("next-execution", context.Background()); err != nil {
		t.Fatal(err)
	}
	identity := publictool.CallIdentityFromContext(identityContext())
	identity.ExecutionID = "next-execution"
	identity.InvocationID = "next-invocation"
	nextTask := publictool.WithCallIdentity(context.Background(), identity)
	status, err := manager.Read(nextTask, "session", startedCommand.Command.ID, identity.ExecutionID, identity.AgentID, publictool.CommandReadOptions{})
	if err != nil || status.Command.Status != publictool.CommandRunning {
		t.Fatalf("cross-task status = %+v, error = %v", status, err)
	}
	stopped, err := manager.Stop(nextTask, "session", startedCommand.Command.ID, identity.ExecutionID, identity.AgentID)
	if err != nil || stopped.Command.Status != publictool.CommandCancelled {
		t.Fatalf("cross-task stop = %+v, error = %v", stopped, err)
	}
}

func TestStopSessionStopsBackgroundCommands(t *testing.T) {
	started := make(chan struct{})
	manager, _ := managerForTest(t, executorFunc(func(ctx context.Context, _ publictool.CommandRequest) (publictool.CommandResult, error) {
		close(started)
		<-ctx.Done()
		return publictool.CommandResult{}, ctx.Err()
	}), publictool.CommandOptions{})
	command, err := manager.Start(identityContext(), publictool.CommandRequest{Command: "service", Background: true}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background command did not start")
	}
	if err := manager.StopSession(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Read(context.Background(), "session", command.Command.ID, "", "", publictool.CommandReadOptions{})
	if err != nil || status.Command.Status != publictool.CommandCancelled {
		t.Fatalf("session-stop status = %+v, error = %v", status, err)
	}
}

func TestManagerCloseStopsBackgroundCommands(t *testing.T) {
	started := make(chan struct{})
	ended := make(chan struct{})
	manager, _ := managerForTest(t, executorFunc(func(ctx context.Context, _ publictool.CommandRequest) (publictool.CommandResult, error) {
		close(started)
		<-ctx.Done()
		close(ended)
		return publictool.CommandResult{}, ctx.Err()
	}), publictool.CommandOptions{})
	command, err := manager.Start(identityContext(), publictool.CommandRequest{Command: "service", Background: true}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background command did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("Core-style manager close did not terminate the service")
	}
	status, err := manager.Read(context.Background(), "session", command.Command.ID, "", "", publictool.CommandReadOptions{})
	if err != nil || status.Command.Status != publictool.CommandCancelled {
		t.Fatalf("closed service status = %#v, error = %v", status, err)
	}
}

func TestCommandOutputCursorAndTruncation(t *testing.T) {
	text := strings.Repeat("a", OutputLimit) + "tail"
	m, _ := managerForTest(t, executorFunc(func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error) {
		return publictool.CommandResult{Output: text}, nil
	}), publictool.CommandOptions{})
	result, err := m.Start(identityContext(), publictool.CommandRequest{Command: "output"}, 0, time.Second)
	if err != nil || !result.Truncated || result.Cursor != 4 || len(result.Output) != ReadLimit || result.EndCursor != int64(len(text)) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	last, err := m.Read(context.Background(), "session", result.Command.ID, "", "", publictool.CommandReadOptions{Cursor: result.EndCursor - 4})
	if err != nil || last.Output != "tail" || last.NextCursor != result.EndCursor {
		t.Fatalf("tail=%+v error=%v", last, err)
	}
	if _, err := m.Read(context.Background(), "session", result.Command.ID, "", "", publictool.CommandReadOptions{Cursor: result.EndCursor + 1}); err == nil {
		t.Fatal("future cursor accepted")
	}
}

func TestCommandConcurrencyTimeoutAndScopeClosure(t *testing.T) {
	m, _ := managerForTest(t, executorFunc(func(ctx context.Context, _ publictool.CommandRequest) (publictool.CommandResult, error) {
		<-ctx.Done()
		return publictool.CommandResult{}, ctx.Err()
	}), publictool.CommandOptions{MaxConcurrent: 1})
	result, err := m.Start(identityContext(), publictool.CommandRequest{Command: "long"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(identityContext(), publictool.CommandRequest{Command: "other"}, 0, 0); err == nil {
		t.Fatal("concurrency cap ignored")
	}
	if err := m.StopScope(context.Background(), "execution", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(identityContext(), publictool.CommandRequest{Command: "after end"}, 0, 0); err == nil {
		t.Fatal("closed scope accepted a command")
	}
	if err := m.OpenScope("execution", context.Background()); err != nil {
		t.Fatal(err)
	}
	timed, err := m.Start(identityContext(), publictool.CommandRequest{Command: "timeout"}, time.Millisecond, time.Second)
	if err != nil || timed.Command.Status != publictool.CommandTimedOut {
		t.Fatalf("result=%+v error=%v", timed, err)
	}
	saved, err := m.Read(context.Background(), "session", result.Command.ID, "", "", publictool.CommandReadOptions{})
	if err != nil || saved.Command.Status != publictool.CommandCancelled {
		t.Fatalf("saved=%+v error=%v", saved, err)
	}
}

func TestCommandReadWaitCancellationAndCleanupPersistence(t *testing.T) {
	m, store := managerForTest(t, executorFunc(func(ctx context.Context, _ publictool.CommandRequest) (publictool.CommandResult, error) {
		<-ctx.Done()
		return publictool.CommandResult{}, ctx.Err()
	}), publictool.CommandOptions{})
	result, err := m.Start(identityContext(), publictool.CommandRequest{Command: "long"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Read(ctx, "session", result.Command.ID, "", "", publictool.CommandReadOptions{Wait: time.Second}); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error=%v", err)
	}
	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()
	if err := m.StopScope(context.Background(), "execution", ""); err == nil {
		t.Fatal("cleanup persistence failure ignored")
	}
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	if err := m.StopScope(context.Background(), "execution", ""); err != nil {
		t.Fatal(err)
	}
}

func TestCommandRestartAndExecutorPanic(t *testing.T) {
	store := &memoryStore{records: map[string][]byte{}}
	if err := store.SaveCommand(context.Background(), Record{Info: publictool.CommandInfo{ID: "old", SessionID: "session", ExecutionID: "execution", Status: publictool.CommandRunning}, Output: []byte("saved"), End: 5}); err != nil {
		t.Fatal(err)
	}
	m, err := New(context.Background(), store, executorFunc(func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error) {
		t.Fatal("restart executed a command")
		return publictool.CommandResult{}, nil
	}), publictool.CommandOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.Read(context.Background(), "session", "old", "", "", publictool.CommandReadOptions{})
	if err != nil || result.Command.Status != publictool.CommandInterrupted || result.Output != "saved" {
		t.Fatalf("restart=%+v error=%v", result, err)
	}
	m, _ = managerForTest(t, executorFunc(func(context.Context, publictool.CommandRequest) (publictool.CommandResult, error) {
		panic("secret panic")
	}), publictool.CommandOptions{})
	result, err = m.Start(identityContext(), publictool.CommandRequest{Command: "panic"}, 0, time.Second)
	if err != nil || result.Command.Status != publictool.CommandFailed || strings.Contains(result.Error, "secret") {
		t.Fatalf("panic=%+v error=%v", result, err)
	}
}

func TestCommandAgentScopeStopsDescendantsAndCanBeReused(t *testing.T) {
	m, _ := managerForTest(t, executorFunc(func(ctx context.Context, _ publictool.CommandRequest) (publictool.CommandResult, error) {
		<-ctx.Done()
		return publictool.CommandResult{}, ctx.Err()
	}), publictool.CommandOptions{})
	for _, agent := range []string{"lead", "lead/worker", "lead/worker/nested"} {
		identity := publictool.CallIdentityFromContext(identityContext())
		identity.AgentID = agent
		ctx := publictool.WithCallIdentity(context.Background(), identity)
		if _, err := m.Start(ctx, publictool.CommandRequest{Command: "long"}, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.StopScope(context.Background(), "execution", "lead/worker"); err != nil {
		t.Fatal(err)
	}
	if !m.Active("execution", "lead") || m.Active("execution", "lead/worker") || m.Active("execution", "lead/worker/nested") {
		t.Fatal("branch cleanup affected the wrong agents")
	}
	identity := publictool.CallIdentityFromContext(identityContext())
	identity.AgentID = "lead/worker"
	ctx := publictool.WithCallIdentity(context.Background(), identity)
	if _, err := m.Start(ctx, publictool.CommandRequest{Command: "closed branch"}, 0, 0); err == nil {
		t.Fatal("closed branch accepted work")
	}
	m.OpenAgent("execution", "lead/worker")
	if _, err := m.Start(ctx, publictool.CommandRequest{Command: "new delegation"}, 0, 0); err != nil {
		t.Fatal(err)
	}
}

func TestCommandRejectsOutputAfterCompletion(t *testing.T) {
	var callback func([]byte) error
	m, _ := managerForTest(t, executorFunc(func(_ context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
		callback = request.OnOutput
		return publictool.CommandResult{}, nil
	}), publictool.CommandOptions{})
	if _, err := m.Start(identityContext(), publictool.CommandRequest{Command: "short"}, 0, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := callback([]byte("late output")); err == nil {
		t.Fatal("late output accepted")
	}
}
