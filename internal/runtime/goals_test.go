package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/model"
	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

type goalProvider struct { calls atomic.Int32; respond func(context.Context, int, publicmodel.ModelRequest) (*publicmodel.ModelResponse, error) }
func (*goalProvider) Name() string { return "host" }
func (p *goalProvider) CreateResponse(ctx context.Context, request publicmodel.ModelRequest, _ publicmodel.ModelProfile) (*publicmodel.ModelResponse,error) { return p.respond(ctx, int(p.calls.Add(1)), request) }
func (p *goalProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, _ func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse,error) { return p.CreateResponse(ctx,request,profile) }

func newGoalTestCore(t *testing.T, provider *goalProvider) (*Core, *SessionRuntime) {
	t.Helper()
	client, err := New(Config{Workspace:t.TempDir(), Memory:MemoryConfig{Disabled:true}, ModelConfig:&publicmodel.Config{
		Providers:map[string]publicmodel.ProviderConfig{"host":{Protocol:publicmodel.ProtocolResponses}},
		Profiles:map[string]publicmodel.ModelProfile{"main":{Provider:"host",Model:"test"}}, DefaultProfile:"main",
	}, ModelProviders:map[string]publicmodel.Provider{"host":provider}})
	if err != nil { t.Fatal(err) }
	saved, err := client.CreateSession(t.Context(), nil)
	if err != nil { t.Fatal(err) }
	runtime, err := client.OpenSessionRuntime(t.Context(), saved.ID)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func(){
		_, _ = runtime.PauseGoal(context.Background())
		client.queueDrainWG.Wait()
		_ = runtime.Close()
		if err := client.Close(); err != nil { t.Error(err) }
	})
	return client, runtime
}

func waitGoalStatus(t *testing.T, runtime *SessionRuntime, status GoalStatus) *Goal {
	t.Helper()
	deadline := time.After(5*time.Second)
	tick := time.NewTicker(5*time.Millisecond)
	defer tick.Stop()
	for {
		goal, err := runtime.GetGoal(t.Context())
		if err != nil { t.Fatal(err) }
		if goal != nil && goal.Status == status { return goal }
		select { case <-deadline: t.Fatalf("goal=%+v; want %s", goal, status); case <-tick.C: }
	}
}

func TestGoalContinuesAcrossTurnsAndCompletes(t *testing.T) {
	p := &goalProvider{respond:func(_ context.Context, call int, request publicmodel.ModelRequest) (*publicmodel.ModelResponse,error) {
		data, _ := json.Marshal(request)
		if !strings.Contains(string(data), "Implement the entire objective") { return nil, errors.New("goal missing from model context") }
		if call == 1 { return hostTextResponse("Made progress; more work remains."),nil }
		if call == 2 { return hostToolCallResponse("goal-done","update_goal",`{"status":"complete","reason":"All requirements verified"}`),nil }
		return hostTextResponse("All done."),nil
	}}
	client, runtime := newGoalTestCore(t,p)
	goal, err := runtime.CreateGoal(t.Context(),GoalCreateOptions{Objective:"Implement the entire objective"})
	if err != nil { t.Fatal(err) }
	waitGoalStatus(t,runtime,GoalComplete)
	client.queueDrainWG.Wait()
	if p.calls.Load() != 3 { t.Fatalf("model calls=%d, want 3 across two turns",p.calls.Load()) }
	events, err := client.sessions.RuntimeEventsAfter(t.Context(),runtime.sessionID,0,200)
	if err != nil { t.Fatal(err) }
	started := 0
	for _, stored := range events { var event Event; _=json.Unmarshal(stored.Payload,&event); if event.Kind==EventTaskStarted { started++; if event.GoalID != goal.ID { t.Fatalf("goal association lost: %+v",event) } } }
	if started != 2 { t.Fatalf("turns=%d, want 2",started) }
}

func TestGoalBudgetExcludesCachedInputAndStopsContinuation(t *testing.T) {
	p := &goalProvider{respond:func(context.Context,int,publicmodel.ModelRequest)(*publicmodel.ModelResponse,error){
		response := hostTextResponse("Progress")
		response.Usage = publicmodel.ResponseUsage{InputTokens:100,InputTokensDetails:publicmodel.TokenInputDetails{CachedTokens:90},OutputTokens:5,TotalTokens:105}
		return response,nil
	}}
	client,runtime := newGoalTestCore(t,p)
	budget:=int64(20)
	if _,err:=runtime.CreateGoal(t.Context(),GoalCreateOptions{Objective:"Work until the goal budget is exhausted",TokenBudget:&budget});err!=nil { t.Fatal(err) }
	waitGoalStatus(t,runtime,GoalBudgetLimited)
	client.queueDrainWG.Wait()
	goal,_:=runtime.GetGoal(t.Context())
	if goal.TokensUsed!=30 || goal.CachedTokens!=180 || p.calls.Load()!=2 { t.Fatalf("goal=%+v, calls=%d",goal,p.calls.Load()) }
	if _,err:=runtime.ResumeGoal(t.Context());err==nil { t.Fatal("exhausted goal resumed") }
	if _,err:=runtime.SetGoalBudget(t.Context(),nil);err!=nil { t.Fatal(err) }
	goal,_=runtime.GetGoal(t.Context())
	if goal.Status!=GoalBudgetLimited { t.Fatal("budget change unexpectedly resumed the goal") }
}

func TestGoalPauseResumeAndEditCheckpoint(t *testing.T) {
	started:=make(chan struct{})
	p:=&goalProvider{respond:func(ctx context.Context,call int,_ publicmodel.ModelRequest)(*publicmodel.ModelResponse,error){
		if call==1 { close(started); <-ctx.Done(); return nil,ctx.Err() }
		if call==2 { return hostToolCallResponse("done","update_goal",`{"status":"complete","reason":"verified"}`),nil }
		return hostTextResponse("Done"),nil
	}}
	client,runtime:=newGoalTestCore(t,p)
	goal,err:=runtime.CreateGoal(t.Context(),GoalCreateOptions{Objective:"Original objective"})
	if err!=nil { t.Fatal(err) }
	select { case <-started: case <-time.After(5*time.Second): t.Fatal("goal never started") }
	if _,err:=runtime.PauseGoal(t.Context());err!=nil { t.Fatal(err) }
	client.queueDrainWG.Wait()
	state,err:=runtime.State(t.Context())
	if err!=nil || state.Task.Status!=TaskCancelled || state.Goal.Status!=GoalPaused { t.Fatalf("state=%+v err=%v",state,err) }
	if _,err:=runtime.EditGoal(t.Context(),"Updated objective");err!=nil { t.Fatal(err) }
	edited,_:=runtime.GetGoal(t.Context())
	if edited.Revision!=goal.Revision+1 || edited.Status!=GoalPaused { t.Fatalf("edited=%+v",edited) }
	if _,err:=runtime.ResumeGoal(t.Context());err!=nil { t.Fatal(err) }
	waitGoalStatus(t,runtime,GoalComplete)
	client.queueDrainWG.Wait()
}

func TestGoalUsageIsIdempotentAndOldToolsCannotChangeNewGoal(t *testing.T) {
	p:=&goalProvider{respond:func(context.Context,int,publicmodel.ModelRequest)(*publicmodel.ModelResponse,error){return hostTextResponse("unused"),nil}}
	client,runtime:=newGoalTestCore(t,p)
	client.queueDrainWG.Wait()
	goal,err:=client.changeGoal(t.Context(),runtime.sessionID,"",func(*Goal)(*Goal,error){return &Goal{ID:"current",Revision:2,Status:GoalActive,Objective:"Current objective"},nil})
	if err!=nil { t.Fatal(err) }
	client.goalMu.Lock(); client.goalRuns=map[string]goalExecution{"execution":{ID:goal.ID,Revision:1}}; client.goalMu.Unlock()
	state:=TaskState{SessionID:runtime.sessionID,ExecutionID:"execution"}
	usage:=Usage{InputTokens:100,InputTokensDetails:TokenInputDetails{CachedTokens:90},OutputTokens:5}
	for range 2 { client.accountGoalUsage(state,usage,"lead",0,model.RequestTrace{ID:"request",Attempt:1}) }
	goal,_=runtime.GetGoal(t.Context())
	if goal.TokensUsed!=15 { t.Fatalf("duplicate usage charged: %+v",goal) }
	ctx:=publictool.WithCallIdentity(t.Context(),publictool.CallIdentity{SessionID:runtime.sessionID,ExecutionID:"execution",AgentID:"lead"})
	result:=(goalTool{core:client,update:true}).Run(ctx,json.RawMessage(`{"status":"complete","reason":"old completion"}`))
	if result.Err==nil { t.Fatal("stale task completed a revised goal") }
	ctx=publictool.WithCallIdentity(t.Context(),publictool.CallIdentity{SessionID:runtime.sessionID,ExecutionID:"execution",AgentID:"worker"})
	if (goalTool{core:client,update:true}).Run(ctx,json.RawMessage(`{"status":"complete","reason":"worker"}`)).Err==nil { t.Fatal("worker changed goal") }
}
