package agentgraph

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	"github.com/afterlune/luneCore/internal/tool"
	"github.com/afterlune/luneCore/memory"
	publicmodel "github.com/afterlune/luneCore/model"
	graph "github.com/afterlune/luneGraph"
	checkpointmemory "github.com/afterlune/luneGraph/checkpoint/memory"
)

type retryRequester struct {
	calls    int
	failures int
	err      error
	stream   bool
}

func (r *retryRequester) CreateResponse(context.Context, model.ModelRequest) (*model.ModelResponse, error) {
	r.calls++
	if r.calls <= r.failures {
		return nil, r.err
	}
	return &model.ModelResponse{Status: "completed"}, nil
}
func (r *retryRequester) StreamResponse(ctx context.Context, request model.ModelRequest, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	if r.stream {
		if err := emit(model.ModelStreamEvent{Type: model.ModelStreamToolStarted}); err != nil {
			return nil, err
		}
	}
	return r.CreateResponse(ctx, request)
}

func retryTestAgent() (*Agent, *[]time.Duration) {
	now := time.Now()
	delays := []time.Duration{}
	return &Agent{options: AgentOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), retry: &retryTiming{
		now: func() time.Time { return now }, jitter: func() time.Duration { return 0 },
		wait: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			now = now.Add(delay)
			return nil
		},
	}}}, &delays
}

func TestModelRetryPolicy(t *testing.T) {
	for _, test := range []struct {
		name                           string
		max, failures, attempts, waits int
		err                            error
		paused                         bool
	}{
		{"default success", 0, 2, 3, 2, &publicmodel.RequestError{Category: publicmodel.ErrorUnavailable}, false},
		{"exhausted", 0, 9, 3, 2, &publicmodel.RequestError{Category: publicmodel.ErrorNetwork}, true},
		{"disabled", 1, 9, 1, 0, &publicmodel.RequestError{Category: publicmodel.ErrorTimeout}, true},
		{"quota", 0, 9, 1, 0, &publicmodel.RequestError{Category: publicmodel.ErrorQuota}, false},
		{"unknown", 0, 9, 1, 0, errors.New("unclassified failure"), false},
		{"long retry after", 0, 9, 1, 0, &publicmodel.RequestError{Category: publicmodel.ErrorRateLimit, RetryAfter: time.Minute}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, waits := retryTestAgent()
			r := &retryRequester{failures: test.failures, err: test.err}
			response, err := a.sendModel(context.Background(), State{Execution: ExecutionOptions{ModelMaxAttempts: test.max}}, r, model.ModelRequest{}, nil)
			var paused *attemptFailure
			if errors.As(err, &paused) != test.paused || r.calls != test.attempts || len(*waits) != test.waits {
				t.Fatalf("response=%v error=%v calls=%d waits=%v", response, err, r.calls, *waits)
			}
			if test.name == "default success" && ((*waits)[0] != time.Second || (*waits)[1] != 2*time.Second) {
				t.Fatalf("backoff=%v", *waits)
			}
			if paused != nil && paused.failure.Attempts != test.attempts {
				t.Fatalf("failure=%+v", paused.failure)
			}
		})
	}
}

func TestModelRetryDeadlineAndCancellation(t *testing.T) {
	a, waits := retryTestAgent()
	r := &retryRequester{failures: 10, err: &publicmodel.RequestError{Category: publicmodel.ErrorRateLimit}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := a.sendModel(ctx, State{}, r, model.ModelRequest{}, nil)
	var failure *attemptFailure
	if !errors.As(err, &failure) || r.calls != 1 || len(*waits) != 0 {
		t.Fatalf("error=%v calls=%d waits=%v", err, r.calls, *waits)
	}
	ctx, cancelWait := context.WithCancel(context.Background())
	a.options.retry.wait = func(ctx context.Context, _ time.Duration) error { cancelWait(); return ctx.Err() }
	r.calls = 0
	_, err = a.sendModel(ctx, State{}, r, model.ModelRequest{}, nil)
	if !errors.Is(err, context.Canceled) || r.calls != 1 {
		t.Fatalf("error=%v calls=%d", err, r.calls)
	}
}

func TestModelRetryStreamingBoundary(t *testing.T) {
	for _, started := range []bool{false, true} {
		a, _ := retryTestAgent()
		r := &retryRequester{failures: 2, err: &publicmodel.RequestError{Category: publicmodel.ErrorNetwork}, stream: started}
		_, err := a.sendModel(context.Background(), State{}, r, model.ModelRequest{}, func(model.ModelStreamEvent) error { return nil })
		if !started && (err != nil || r.calls != 3) {
			t.Fatalf("before stream: error=%v calls=%d", err, r.calls)
		}
		var failure *attemptFailure
		if started && (!errors.As(err, &failure) || r.calls != 1 || !failure.failure.StreamStarted) {
			t.Fatalf("after stream: error=%v calls=%d", err, r.calls)
		}
	}
}

func TestModelRetryAfterMinimumAndBackoffCap(t *testing.T) {
	a, waits := retryTestAgent()
	r := &retryRequester{failures: 10, err: &publicmodel.RequestError{Category: publicmodel.ErrorRateLimit, RetryAfter: 3 * time.Second}}
	_, _ = a.sendModel(context.Background(), State{Execution: ExecutionOptions{ModelMaxAttempts: 6}}, r, model.ModelRequest{}, nil)
	want := []time.Duration{3 * time.Second, 3 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, delay := range *waits {
		if delay != want[i] {
			t.Fatalf("backoff=%v", *waits)
		}
	}
}

type transientTeam struct {
	scriptedTeamExecutor
	failed *retryRequester
}

func (t *transientTeam) Requester(ctx context.Context, state State) (Requester, error) {
	if state.TeamTaskID == "review" {
		return t.failed, nil
	}
	return t.scriptedTeamExecutor.Requester(ctx, state)
}

func TestModelRetryTeamFailureIsMerged(t *testing.T) {
	registry, err := tool.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	team := &transientTeam{scriptedTeamExecutor: scriptedTeamExecutor{limit: 2, profiles: map[string]model.ModelProfile{"worker": {Provider: "vendor", Model: "worker"}}}, failed: &retryRequester{failures: 9, err: &publicmodel.RequestError{Category: publicmodel.ErrorUnavailable}}}
	requester := &scriptedRequester{responses: []*model.Response{
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"function_call","call_id":"team","name":"delegate_team","arguments":"{\"tasks\":[{\"id\":\"research\",\"model\":\"worker\",\"task\":\"research\"},{\"id\":\"review\",\"model\":\"worker\",\"task\":\"review\"}]}"}]}`),
		mustDecodeResponse(t, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial result"}]}]}`),
	}}
	timing, _ := retryTestAgent()
	agent, err := NewWithOptions(requester, registry, AgentOptions{
		Team: team, retry: timing.options.retry,
		Execution: ExecutionOptions{MaxTeamChildrenPerAgent: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpointmemory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	input, _ := model.NewResponseInput("delegate research and review")
	_, result, err := agent.Start(context.Background(), "team-retry", nil, input, TaskOptions{}, store, graph.Options{MaxSteps: 6})
	if err != nil || result.Status != graph.StatusCompleted || result.Final == nil || result.Final.ModelFailure != nil || team.failed.calls != 3 {
		t.Fatalf("status=%s failure=%v attempts=%d error=%v", result.Status, result.Final, team.failed.calls, err)
	}
}

func TestModelRetryMemoryFailureKeepsTaskComplete(t *testing.T) {
	a, _ := retryTestAgent()
	r := &retryRequester{failures: 9, err: &publicmodel.RequestError{Category: publicmodel.ErrorUnavailable}}
	a.options.MemoryRequester = r
	var status string
	a.options.MemoryObserver = func(_ State, stage string, _ []string, _ error) { status = stage }
	a.options.MemoryWriter = func(context.Context, State, []memory.Candidate) ([]string, error) {
		t.Fatal("failed memory request reached writer")
		return nil, nil
	}
	workflow := graph.New[State](memoryNode)
	if err := workflow.AddNode(graph.NodeSpec[State]{Name: memoryNode, Run: a.extractMemory}); err != nil {
		t.Fatal(err)
	}
	runner, err := workflow.Compile(graph.Config[State]{MachineID: "memory-retry", Clone: graph.JSONClone[State]})
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpointmemory.New[State](graph.JSONClone[State])
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := runner.Start(context.Background(), "memory-retry", State{}, store, graph.Options{})
	if err != nil || result.Status != graph.StatusCompleted || status != "failed" || r.calls != 3 {
		t.Fatalf("status=%s memory=%s calls=%d error=%v", result.Status, status, r.calls, err)
	}
}

func TestModelRetryCompactionUsesSharedPolicy(t *testing.T) {
	a, _ := retryTestAgent()
	r := &retryRequester{failures: 9, err: &publicmodel.RequestError{Category: publicmodel.ErrorUnavailable}}
	a.requester = r
	_, err := a.summarizeHistoryPrefix(context.Background(), model.ModelRequest{}, nil, false, State{})
	var failure *attemptFailure
	if !errors.As(err, &failure) || r.calls != 3 {
		t.Fatalf("automatic compaction calls=%d error=%v", r.calls, err)
	}
	r.calls = 0
	a.options.Execution.ModelMaxAttempts = 1
	_, err = a.RetryResponse(context.Background(), r, model.ModelRequest{})
	if !errors.As(err, &failure) || r.calls != 1 {
		t.Fatalf("manual compaction calls=%d error=%v", r.calls, err)
	}
}
