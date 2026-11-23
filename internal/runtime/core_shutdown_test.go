package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
)

type shutdownBarrierProvider struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
}

func (p *shutdownBarrierProvider) Name() string { return "host" }

func (p *shutdownBarrierProvider) CreateResponse(ctx context.Context, _ publicmodel.ModelRequest, _ publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		close(p.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return hostTextResponse("completed after restart"), nil
}

func (p *shutdownBarrierProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, emit func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	response, err := p.CreateResponse(ctx, request, profile)
	if err == nil && emit != nil {
		err = emit(publicmodel.ModelStreamEvent{Type: publicmodel.ModelStreamCompleted, Response: response})
	}
	return response, err
}

func TestShutdownPreservesActiveCheckpointAndQueuedTurns(t *testing.T) {
	provider := &shutdownBarrierProvider{entered: make(chan struct{})}
	workspace, stateDir := t.TempDir(), t.TempDir()
	config := runtimeTestConfig(workspace, stateDir, provider)
	client, err := New(config)
	if err != nil {
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
	active, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "active before shutdown")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("model request did not reach the shutdown barrier")
	}
	queued, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "queued before shutdown")}, QueueInput)
	if err != nil {
		t.Fatal(err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err = client.Shutdown(shutdownCtx)
	cancel()
	if err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	reopened, err := New(config)
	if err != nil {
		t.Fatalf("reopen Core: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(context.Background()); err != nil {
			t.Errorf("close reopened Core: %v", err)
		}
	})
	reopenedRuntime, err := reopened.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopenedRuntime.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Queue) != 2 || state.Queue[0].ExecutionID != active.ExecutionID || state.Queue[0].Status != "paused" ||
		state.Queue[1].ExecutionID != queued.ExecutionID || state.Queue[1].Status != "queued" {
		t.Fatalf("queue after shutdown and restart = %#v", state.Queue)
	}

	recovered, err := reopenedRuntime.Recover(context.Background())
	if err != nil || recovered.ExecutionID != active.ExecutionID {
		t.Fatalf("Recover active turn = %#v, error=%v", recovered, err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	firstResult, err := reopenedRuntime.WaitTurn(waitCtx, active.ExecutionID)
	if err != nil || firstResult.Status != TaskCompleted {
		t.Fatalf("recovered turn = %#v, error=%v", firstResult, err)
	}
	queuedResult, err := reopenedRuntime.WaitTurn(waitCtx, queued.ExecutionID)
	if err != nil || queuedResult.Status != TaskCompleted {
		t.Fatalf("queued turn after recovery = %#v, error=%v", queuedResult, err)
	}
}
