package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func TestStoredTurnRequestPreservesPlanMode(t *testing.T) {
	var request storedTurnRequest
	if err := json.Unmarshal([]byte(`{"input":"make a plan","plan_mode":true}`), &request); err != nil {
		t.Fatalf("decode plan turn request: %v", err)
	}
	if string(request.Input) != `"make a plan"` || !request.PlanMode {
		t.Fatalf("decoded turn request = %#v, want input and PlanMode preserved", request)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("encode turn request: %v", err)
	}
	if !strings.Contains(string(encoded), `"plan_mode":true`) {
		t.Fatalf("stored turn request lost PlanMode: %s", encoded)
	}
}

func TestPlanModeHandoffImplementsInSameContextWithScopedPermission(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostTextResponse("<proposed_plan>Write the requested workspace file.</proposed_plan>"),
		hostToolCallResponse("select-host", "select_tool_category", `{"category":"host"}`),
		hostToolCallResponse("write-call", "host_echo", `{}`),
		hostTextResponse("Plan implemented."),
	}}
	writeTool := &hostEchoTool{}
	client := newProgrammaticCore(t, provider, publictool.Registration{
		Tool: writeTool,
		Metadata: publictool.Metadata{
			Category:        publictool.Category{Name: "host", Description: "Host supplied tools."},
			PermissionClass: publictool.PermissionWorkspaceWrite,
		},
	})
	if err := client.SetPermissionMode(PermissionWorkspace); err != nil {
		t.Fatal(err)
	}
	client.executionDefaults = ExecutionOptions{MaxSteps: 20}
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewTaskInput("Plan a workspace change")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: input, PlanMode: true}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	planResult, err := runtime.WaitTurn(context.Background(), planned.ExecutionID)
	if err != nil || planResult.Status != TaskCompleted || planResult.ProposedPlan == nil {
		t.Fatalf("plan result = %#v, error=%v", planResult, err)
	}
	if planResult.ProposedPlan.Text != "Write the requested workspace file." {
		t.Fatalf("proposed plan = %q", planResult.ProposedPlan.Text)
	}
	client.executionDefaults = ExecutionOptions{MaxSteps: 1}
	provider.mu.Lock()
	planningRequest := provider.seenRequests[0]
	provider.mu.Unlock()
	if !strings.Contains(requestHostText(planningRequest), "Plan mode") || !strings.Contains(requestHostText(planningRequest), "<proposed_plan>") || !strings.Contains(requestHostText(planningRequest), "workspace") {
		t.Fatalf("Plan mode instructions = %q", requestHostText(planningRequest))
	}
	for _, definition := range planningRequest.Tools {
		if function, ok := definition.(publicmodel.FunctionTool); ok && function.Name == "host_echo" {
			t.Fatal("workspace write tool was visible during read-only planning")
		}
	}
	foundAskUser := false
	for _, definition := range planningRequest.Tools {
		if function, ok := definition.(publicmodel.FunctionTool); ok && function.Name == "ask_user" {
			foundAskUser = true
		}
	}
	if !foundAskUser {
		t.Fatal("Plan mode hid the user-question tool")
	}

	if err := client.SetPermissionMode(PermissionAuto); err != nil {
		t.Fatal(err)
	}
	implementation, err := runtime.ImplementPlan(context.Background(), planned.ExecutionID, PlanImplementationOptions{Mode: PlanImplementSameContext})
	if err != nil {
		t.Fatalf("ImplementPlan: %v", err)
	}
	if implementation.SessionID != saved.ID {
		t.Fatalf("implementation session = %q, want source session %q", implementation.SessionID, saved.ID)
	}
	paused, err := runtime.WaitTurn(context.Background(), implementation.Submission.ExecutionID)
	if err != nil || paused.Status != TaskPaused {
		t.Fatalf("implementation pause = %#v, error=%v", paused, err)
	}
	pending, found, err := client.pendingState(context.Background(), saved.ID)
	if err != nil || !found || pending.PermissionOverride != string(PermissionAuto) {
		t.Fatalf("paused permission override = %q, found=%v, error=%v", pending.PermissionOverride, found, err)
	}
	queued, found, err := client.sessions.QueuedTurnByExecutionID(context.Background(), saved.ID, implementation.Submission.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("paused plan turn lost its queued request")
	}
	var stored storedTurnRequest
	if err := json.Unmarshal(queued.Request, &stored); err != nil || stored.PermissionOverride != string(PermissionAuto) {
		t.Fatalf("stored permission override = %q, error=%v", stored.PermissionOverride, err)
	}
	recovered, err := runtime.Recover(context.Background(), ExecutionOptions{MaxSteps: 10})
	if err != nil {
		t.Fatalf("recover implementation: %v", err)
	}
	result, err := runtime.WaitTurn(context.Background(), recovered.ExecutionID)
	if err != nil || result.Status != TaskCompleted {
		t.Fatalf("recovered implementation result = %#v, error=%v", result, err)
	}
	writeTool.mu.Lock()
	calls := writeTool.calls
	writeTool.mu.Unlock()
	if calls != 1 {
		t.Fatalf("workspace write tool calls = %d, want 1", calls)
	}
	if client.PermissionMode() != PermissionAuto {
		t.Fatalf("global permission mode = %q, want current auto permission preserved", client.PermissionMode())
	}
	provider.mu.Lock()
	executionRequest := provider.seenRequests[2]
	provider.mu.Unlock()
	foundWriteTool := false
	for _, definition := range executionRequest.Tools {
		if function, ok := definition.(publicmodel.FunctionTool); ok && function.Name == "host_echo" {
			foundWriteTool = true
		}
	}
	if !foundWriteTool {
		t.Fatal("workspace write tool was hidden during approved plan execution")
	}
	encodedInput, err := json.Marshal(executionRequest.Input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encodedInput), "Plan a workspace change") || !strings.Contains(string(encodedInput), "Implement the plan under the active permission mode.") {
		t.Fatalf("same-context implementation input = %s", encodedInput)
	}
}

func TestPlanImplementationUsesNormalApprovalFlow(t *testing.T) {
	for _, test := range []struct {
		name      string
		allow     bool
		wantCalls int
	}{
		{name: "approved", allow: true, wantCalls: 1},
		{name: "denied", allow: false, wantCalls: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
				hostTextResponse("<proposed_plan>Run the requested command.</proposed_plan>"),
				hostToolCallResponse("select-host", "select_tool_category", `{"category":"host"}`),
				hostToolCallResponse("command-call", "host_echo", `{}`),
				hostTextResponse("Plan implementation finished."),
			}}
			tool := &hostEchoTool{}
			client := newProgrammaticCore(t, provider, publictool.Registration{
				Tool: tool,
				Metadata: publictool.Metadata{
					Category:        publictool.Category{Name: "host", Description: "Host supplied tools."},
					PermissionClass: publictool.PermissionCommand,
				},
			})
			if err := client.SetPermissionMode(PermissionWorkspace); err != nil {
				t.Fatal(err)
			}
			saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
			if err != nil {
				t.Fatal(err)
			}
			runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
			if err != nil {
				t.Fatal(err)
			}
			planInput, err := NewTaskInput("Run a command")
			if err != nil {
				t.Fatal(err)
			}
			planned, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: planInput, PlanMode: true}, StartIfIdle)
			if err != nil {
				t.Fatal(err)
			}
			if result, err := runtime.WaitTurn(context.Background(), planned.ExecutionID); err != nil || result.ProposedPlan == nil {
				t.Fatalf("plan result = %#v, error=%v", result, err)
			}
			events := subscribeTestEvents(t, client, saved.ID)
			implementation, err := runtime.ImplementPlan(context.Background(), planned.ExecutionID, PlanImplementationOptions{
				Mode: PlanImplementSameContext, PermissionMode: PermissionWorkspace,
			})
			if err != nil {
				t.Fatalf("ImplementPlan: %v", err)
			}
			var approval *ApprovalRequest
			deadline := time.After(5 * time.Second)
			for approval == nil {
				select {
				case event := <-events:
					if event.Kind == EventApprovalRequired {
						approval = event.Approval
					}
				case <-deadline:
					t.Fatal("plan command did not request normal approval")
				}
			}
			decision := publictool.ApprovalDeny
			if test.allow {
				decision = publictool.ApprovalAllowOnce
			}
			if err := runtime.ResolveApprovalDecision(context.Background(), approval.ID, decision); err != nil {
				t.Fatalf("ResolveApproval: %v", err)
			}
			result, err := runtime.WaitTurn(context.Background(), implementation.Submission.ExecutionID)
			if err != nil || result.Status != TaskCompleted {
				t.Fatalf("implementation result = %#v, error=%v", result, err)
			}
			tool.mu.Lock()
			calls := tool.calls
			tool.mu.Unlock()
			if calls != test.wantCalls {
				t.Fatalf("tool calls = %d, want %d", calls, test.wantCalls)
			}
			if client.PermissionMode() != PermissionWorkspace {
				t.Fatalf("global permission mode = %q, want workspace preserved", client.PermissionMode())
			}
		})
	}
}

func TestPlanImplementationDoesNotPromoteReadOnlyPermission(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostTextResponse("<proposed_plan>Write the requested workspace file.</proposed_plan>"),
		hostToolCallResponse("select-host", "select_tool_category", `{"category":"host"}`),
		hostToolCallResponse("write-call", "host_echo", `{}`),
		hostTextResponse("The write was unavailable under the active permission mode."),
	}}
	tool := &hostEchoTool{}
	client := newProgrammaticCore(t, provider, publictool.Registration{
		Tool: tool,
		Metadata: publictool.Metadata{
			Category:        publictool.Category{Name: "host", Description: "Host supplied tools."},
			PermissionClass: publictool.PermissionWorkspaceWrite,
		},
	})
	if err := client.SetPermissionMode(PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	planInput, err := NewTaskInput("Write a file")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: planInput, PlanMode: true}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.WaitTurn(context.Background(), planned.ExecutionID); err != nil || result.ProposedPlan == nil {
		t.Fatalf("plan result = %#v, error=%v", result, err)
	}
	implementation, err := runtime.ImplementPlan(context.Background(), planned.ExecutionID, PlanImplementationOptions{
		Mode: PlanImplementSameContext, PermissionMode: PermissionReadOnly,
	})
	if err != nil {
		t.Fatalf("ImplementPlan: %v", err)
	}
	result, err := runtime.WaitTurn(context.Background(), implementation.Submission.ExecutionID)
	if err != nil || result.Status != TaskCompleted {
		t.Fatalf("implementation result = %#v, error=%v", result, err)
	}
	tool.mu.Lock()
	calls := tool.calls
	tool.mu.Unlock()
	if calls != 0 {
		t.Fatalf("workspace write tool calls = %d, want 0 in read-only mode", calls)
	}
	if client.PermissionMode() != PermissionReadOnly {
		t.Fatalf("global permission mode = %q, want read-only preserved", client.PermissionMode())
	}
	provider.mu.Lock()
	request := provider.seenRequests[2]
	provider.mu.Unlock()
	for _, definition := range request.Tools {
		if function, ok := definition.(publicmodel.FunctionTool); ok && function.Name == "host_echo" {
			t.Fatal("workspace write tool was exposed during read-only plan implementation")
		}
	}
}

func TestPlanModeHandoffStartsPlanOnlySession(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostTextResponse("<proposed_plan>Only update the README.</proposed_plan>"),
		hostTextResponse("Done."),
	}}
	client := newProgrammaticCore(t, provider)
	if err := client.SetPermissionMode(PermissionAuto); err != nil {
		t.Fatal(err)
	}
	source, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewTaskInput("Old conversation context must not be copied")
	if err != nil {
		t.Fatal(err)
	}
	planned, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: input, PlanMode: true}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.WaitTurn(context.Background(), planned.ExecutionID); err != nil || result.ProposedPlan == nil {
		t.Fatalf("plan result = %#v, error=%v", result, err)
	}
	if err := client.SetPermissionMode(PermissionFullAccess); err != nil {
		t.Fatal(err)
	}
	implementation, err := runtime.ImplementPlan(context.Background(), planned.ExecutionID, PlanImplementationOptions{Mode: PlanImplementNewContext})
	if err != nil {
		t.Fatalf("ImplementPlan in new context: %v", err)
	}
	if implementation.SessionID == source.ID {
		t.Fatal("new-context implementation reused the source session")
	}
	newRuntime, err := client.OpenSessionRuntime(context.Background(), implementation.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := newRuntime.WaitTurn(context.Background(), implementation.Submission.ExecutionID)
	if err != nil || result.Status != TaskCompleted {
		t.Fatalf("new-context implementation result = %#v, error=%v", result, err)
	}
	provider.mu.Lock()
	request := provider.seenRequests[1]
	provider.mu.Unlock()
	encodedInput, err := json.Marshal(request.Input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encodedInput), "Only update the README") || strings.Contains(string(encodedInput), "Old conversation context") {
		t.Fatalf("new-context input = %s", encodedInput)
	}
	if !strings.Contains(requestHostText(request), "Full-access mode permits") || client.PermissionMode() != PermissionFullAccess {
		t.Fatalf("new-context implementation did not use full-access: instructions=%q mode=%q", requestHostText(request), client.PermissionMode())
	}
}

func TestPlanHandoffRejectsOutdatedPlan(t *testing.T) {
	provider := &scriptedHostProvider{responses: []*publicmodel.ModelResponse{
		hostTextResponse("<proposed_plan>First plan.</proposed_plan>"),
		hostTextResponse("A newer answer."),
	}}
	client := newProgrammaticCore(t, provider)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "make a plan")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.WaitTurn(context.Background(), first.ExecutionID); err != nil || result.ProposedPlan == nil {
		t.Fatalf("first plan result = %#v, error=%v", result, err)
	}
	second, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "new request")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runtime.WaitTurn(context.Background(), second.ExecutionID); err != nil || result.Status != TaskCompleted {
		t.Fatalf("newer result = %#v, error=%v", result, err)
	}
	if _, err := runtime.ImplementPlan(context.Background(), first.ExecutionID, PlanImplementationOptions{Mode: PlanImplementSameContext}); !errors.Is(err, ErrPlanOutdated) {
		t.Fatalf("ImplementPlan for stale source = %v, want ErrPlanOutdated", err)
	}
}

func TestProposedPlanRequiresOneCompleteNonEmptyBlock(t *testing.T) {
	for _, text := range []string{
		"<proposed_plan>unfinished",
		"<proposed_plan></proposed_plan>",
		"<proposed_plan>one</proposed_plan><proposed_plan>two</proposed_plan>",
		"<proposed_plan>one</proposed_plan><proposed_plan_end/>",
		"</proposed_plan>orphaned",
		"<proposed_plan_end/>orphaned",
	} {
		response, err := json.Marshal(hostTextResponse(text))
		if err != nil {
			t.Fatal(err)
		}
		if plan := extractProposedPlan(response); plan != nil {
			t.Errorf("extractProposedPlan(%q) = %#v, want nil", text, plan)
		}
	}
}

func TestProposedPlanAcceptsObservedCompatCloseTag(t *testing.T) {
	response, err := json.Marshal(hostTextResponse("Before.\n<proposed_plan>\n## Steps\n1. Inspect\n<proposed_plan_end/>\nAfter."))
	if err != nil {
		t.Fatal(err)
	}
	plan := extractProposedPlan(response)
	if plan == nil {
		t.Fatal("expected the observed compatibility close tag to produce a plan")
	}
	if plan.Text != "## Steps\n1. Inspect" {
		t.Fatalf("compatibility plan = %q", plan.Text)
	}
}

func TestProposedPlanPreservesInlineCodeTags(t *testing.T) {
	text := "Here is the plan:\n\n<proposed_plan>\n# Analysis\nNotice that `<proposed_plan>` in backticks is preserved.\n```markdown\n<proposed_plan>inner code</proposed_plan>\n```\nStep 1: Done\n</proposed_plan>\n\nLet me know!"
	response, err := json.Marshal(hostTextResponse(text))
	if err != nil {
		t.Fatal(err)
	}
	plan := extractProposedPlan(response)
	if plan == nil {
		t.Fatal("expected plan to be extracted, got nil")
	}
	if !strings.Contains(plan.Text, "`<proposed_plan>`") {
		t.Errorf("expected plan to preserve `<proposed_plan>`, got: %s", plan.Text)
	}
	if !strings.Contains(plan.Text, "<proposed_plan>inner code</proposed_plan>") {
		t.Errorf("expected plan to preserve fenced code block, got: %s", plan.Text)
	}
}
