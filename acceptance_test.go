package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
	publictool "github.com/afterlune/luneCore/tool"
)

type acceptanceVerifierFunc func(context.Context, AcceptanceVerificationInput) (AcceptanceReport, error)

func (f acceptanceVerifierFunc) VerifyAcceptance(ctx context.Context, input AcceptanceVerificationInput) (AcceptanceReport, error) {
	return f(ctx, input)
}

func newAcceptanceCore(t *testing.T, provider *scriptedHostProvider, verifier AcceptanceVerifier, tools ...publictool.Registration) *Core {
	t.Helper()
	config := runtimeTestConfig(t.TempDir(), t.TempDir(), provider)
	config.AcceptanceVerifier = verifier
	config.Tools = tools
	client, err := New(config)
	if err != nil {
		t.Fatalf("New acceptance Core: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close acceptance Core: %v", err)
		}
	})
	return client
}

func criteriaToolResponse(kind AcceptanceTaskKind, id, description, basis string) *publicmodel.ModelResponse {
	arguments, _ := json.Marshal(map[string]any{
		"kind":     kind,
		"criteria": []map[string]string{{"id": id, "description": description, "basis": basis}},
	})
	return hostToolCallResponse("criteria-call", "prepare_task_criteria", string(arguments))
}

func acceptanceToolResponse(candidateID, criterionID string, outcome AcceptanceOutcome, evidence string) *publicmodel.ModelResponse {
	arguments, _ := json.Marshal(map[string]any{
		"candidate_id": candidateID,
		"results": []map[string]any{{
			"criterion_id": criterionID, "outcome": outcome,
			"evidence": []string{evidence}, "reason": "Checked against the saved candidate and evidence.",
		}},
	})
	return hostToolCallResponse("acceptance-call-"+candidateID, "submit_acceptance", string(arguments))
}

func TestAcceptanceCoversAnswerPlanAndExecution(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		kind        AcceptanceTaskKind
		candidate   string
		description string
	}{
		{name: "answer", input: "What is 2 + 2?", kind: AcceptanceTaskAnswer, candidate: "4", description: "Answer the arithmetic question."},
		{name: "plan", input: "Plan a workspace change", kind: AcceptanceTaskPlan, candidate: "<proposed_plan>Update the README.</proposed_plan>", description: "Give a complete plan; do not require implementation."},
		{name: "execution", input: "Implement the requested change", kind: AcceptanceTaskExecution, candidate: "Implemented the requested change.", description: "Complete the requested implementation."},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
				criteriaToolResponse(test.kind, "current_request", test.description, test.input),
				hostTextResponse(test.candidate),
				acceptanceToolResponse("candidate-1", "current_request", AcceptancePassed, "candidate:candidate-1"),
			}}
			client := newAcceptanceCore(t, provider, nil)
			session, err := client.CreateSession(context.Background(), json.RawMessage("null"))
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.RunTask(context.Background(), TaskRequest{SessionID: session.ID, Input: mustSettingsTaskInput(t, test.input)})
			if err != nil || result.Status != TaskCompleted || len(result.AcceptanceReports) != 1 || len(result.AcceptanceCriteria) != 1 || len(result.AcceptanceEvidence) != 1 {
				t.Fatalf("RunTask = %#v, error=%v", result, err)
			}
			report := result.AcceptanceReports[0]
			if report.Source != AcceptanceSourceModel || report.Results[0].Outcome != AcceptancePassed {
				t.Fatalf("acceptance report = %#v", report)
			}
			if test.kind == AcceptanceTaskPlan && result.ProposedPlan == nil {
				t.Fatal("accepted plan was not exposed for handoff")
			}
			provider.mu.Lock()
			requests := append([]publicmodel.ModelRequest(nil), provider.seenRequests...)
			provider.mu.Unlock()
			if len(requests) != 3 || acceptanceRequestStage(requests[0]) != "criteria" || acceptanceRequestStage(requests[2]) != "review" {
				t.Fatalf("model request stages = %#v", requests)
			}
			for _, definition := range requests[2].Tools {
				if tool, ok := definition.(publicmodel.FunctionTool); ok && tool.Name != "submit_acceptance" && tool.Name != "request_acceptance_evidence" {
					t.Fatalf("business tool %q was exposed to acceptance model", tool.Name)
				}
			}
		})
	}
}

func TestAcceptanceCanRequestEvidenceWithoutSpendingRepairAttempts(t *testing.T) {
	input := "Explain and verify the selected value"
	provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskAnswer, "value", "Explain and verify the selected value.", input),
		hostTextResponse("The selected value is 42."),
		hostToolCallResponse("request-evidence", "request_acceptance_evidence", `{"question":"Verify the value using the source file."}`),
		hostTextResponse("The source file confirms the value is 42."),
		acceptanceToolResponse("candidate-2", "value", AcceptancePassed, "candidate:candidate-2"),
	}}
	client := newAcceptanceCore(t, provider, nil)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
	if err != nil || result.Status != TaskCompleted || len(result.AcceptanceReports) != 1 || result.AcceptanceReports[0].CandidateID != "candidate-2" {
		t.Fatalf("RunTask = %#v, error=%v", result, err)
	}
	provider.mu.Lock()
	requests := append([]publicmodel.ModelRequest(nil), provider.seenRequests...)
	provider.mu.Unlock()
	if len(requests) != 5 || acceptanceRequestStage(requests[2]) != "review" || acceptanceRequestStage(requests[3]) != "execution" {
		t.Fatalf("request-evidence stages = %#v", requests)
	}
}

func TestExecutionCannotReplacePreparedAcceptanceCriteria(t *testing.T) {
	input := "Answer the exact current question"
	provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskAnswer, "fixed", "Answer the exact current question.", input),
		hostToolCallResponse("rewrite-criteria", "prepare_task_criteria", `{"kind":"answer","criteria":[{"id":"weakened","description":"Say anything.","basis":"Answer the exact current question"}]}`),
		hostTextResponse("The exact answer is 23."),
		acceptanceToolResponse("candidate-1", "fixed", AcceptancePassed, "candidate:candidate-1"),
	}}
	client := newAcceptanceCore(t, provider, nil)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
	if err != nil || result.Status != TaskCompleted || len(result.AcceptanceCriteria) != 1 || result.AcceptanceCriteria[0].ID != "fixed" {
		t.Fatalf("fixed criteria were changed during execution: result=%#v error=%v", result, err)
	}
}

func TestHostAcceptanceOverridesModelAndRepairsInSameExecution(t *testing.T) {
	input := "Implement one bounded change"
	var calls int
	verifier := acceptanceVerifierFunc(func(_ context.Context, request AcceptanceVerificationInput) (AcceptanceReport, error) {
		calls++
		if request.Goal != "background goal" || len(request.Criteria) != 1 || request.Criteria[0].Basis != input {
			t.Errorf("verifier input = %#v", request)
		}
		outcome := AcceptancePassed
		reason := "Host confirms the corrected candidate."
		if calls == 1 {
			outcome, reason = AcceptanceFailed, "The first candidate did not include the requested behavior."
		}
		return AcceptanceReport{Results: []AcceptanceCriterionResult{{CriterionID: "change", Outcome: outcome, Evidence: []string{"candidate:" + candidateIDFromText(request.Candidate)}, Reason: reason}}}, nil
	})
	provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskExecution, "change", "Implement the bounded change.", input),
		hostTextResponse("First candidate."),
		acceptanceToolResponse("candidate-1", "change", AcceptancePassed, "candidate:candidate-1"),
		hostTextResponse("Corrected candidate."),
		acceptanceToolResponse("candidate-2", "change", AcceptancePassed, "candidate:candidate-2"),
	}}
	client := newAcceptanceCore(t, provider, verifier)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetSessionSettings(context.Background(), saved.ID, SessionSettings{ModelProfile: "host", Goal: "background goal"}); err != nil {
		t.Fatal(err)
	}
	result, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
	if err != nil || result.Status != TaskCompleted || calls != 2 || len(result.AcceptanceReports) != 2 {
		t.Fatalf("RunTask = %#v, verifier calls=%d, error=%v", result, calls, err)
	}
	if result.AcceptanceReports[0].Source != AcceptanceSourceHost || result.AcceptanceReports[0].Results[0].Outcome != AcceptanceFailed ||
		result.AcceptanceReports[1].Source != AcceptanceSourceHost || result.AcceptanceReports[1].Results[0].Outcome != AcceptancePassed {
		t.Fatalf("host acceptance history = %#v", result.AcceptanceReports)
	}
}

func TestHostOmissionAndInvalidVerdictBecomeUnknown(t *testing.T) {
	for _, test := range []struct {
		name   string
		result AcceptanceReport
	}{
		{name: "omitted criterion", result: AcceptanceReport{}},
		{name: "invalid verdict", result: AcceptanceReport{Results: []AcceptanceCriterionResult{{
			CriterionID: "answer", Outcome: AcceptanceOutcome("complete"), Evidence: []string{"candidate:candidate-1"}, Reason: "invalid status",
		}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := "Answer the current question"
			provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
				criteriaToolResponse(AcceptanceTaskAnswer, "answer", "Answer the current question.", input),
				hostTextResponse("The answer is ready."),
				acceptanceToolResponse("candidate-1", "answer", AcceptancePassed, "candidate:candidate-1"),
			}}
			verifier := acceptanceVerifierFunc(func(context.Context, AcceptanceVerificationInput) (AcceptanceReport, error) { return test.result, nil })
			client := newAcceptanceCore(t, provider, verifier)
			saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
			if err != nil {
				t.Fatal(err)
			}
			result, runErr := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
			if !errors.Is(runErr, ErrTaskPaused) || result.Status != TaskPaused || len(result.AcceptanceReports) != 1 ||
				result.AcceptanceReports[0].Source != AcceptanceSourceHost || result.AcceptanceReports[0].Results[0].Outcome != AcceptanceUnknown {
				t.Fatalf("RunTask = %#v, error=%v", result, runErr)
			}
		})
	}
}

func TestHostVerifierTimeoutPausesAsUnknown(t *testing.T) {
	input := "Answer the current question"
	provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskAnswer, "answer", "Answer the current question.", input),
		hostTextResponse("The answer is ready."),
		acceptanceToolResponse("candidate-1", "answer", AcceptancePassed, "candidate:candidate-1"),
	}}
	verifier := acceptanceVerifierFunc(func(ctx context.Context, _ AcceptanceVerificationInput) (AcceptanceReport, error) {
		<-ctx.Done()
		return AcceptanceReport{}, ctx.Err()
	})
	client := newAcceptanceCore(t, provider, verifier)
	client.executionDefaults = ExecutionOptions{ToolTimeout: 100 * time.Millisecond}
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
	if !errors.Is(runErr, ErrTaskPaused) || result.Status != TaskPaused || len(result.AcceptanceReports) != 1 ||
		result.AcceptanceReports[0].Source != AcceptanceSourceHost || result.AcceptanceReports[0].Results[0].Outcome != AcceptanceUnknown {
		t.Fatalf("RunTask = %#v, error=%v", result, runErr)
	}
}

func TestHostVerifierCanJudgeWhenAcceptanceModelFails(t *testing.T) {
	input := "Answer the current question"
	provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskAnswer, "answer", "Answer the current question.", input),
		hostTextResponse("The answer is ready."),
	}}
	checks := 0
	verifier := acceptanceVerifierFunc(func(_ context.Context, request AcceptanceVerificationInput) (AcceptanceReport, error) {
		checks++
		if request.Candidate != "The answer is ready." {
			t.Errorf("host candidate = %q", request.Candidate)
		}
		return AcceptanceReport{Results: []AcceptanceCriterionResult{{
			CriterionID: "answer", Outcome: AcceptancePassed, Evidence: []string{"candidate:candidate-1"}, Reason: "Host verified the candidate.",
		}}}, nil
	})
	client := newAcceptanceCore(t, provider, verifier)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
	if err != nil || result.Status != TaskCompleted || checks != 1 || len(result.AcceptanceReports) != 1 || result.AcceptanceReports[0].Source != AcceptanceSourceHost {
		t.Fatalf("RunTask = %#v, host checks=%d, error=%v", result, checks, err)
	}
}

func candidateIDFromText(candidate string) string {
	if candidate == "First candidate." {
		return "candidate-1"
	}
	return "candidate-2"
}

func TestAcceptanceRepairLimitAndExplicitContinue(t *testing.T) {
	input := "Implement this bounded request"
	responses := []*publicmodel.ModelResponse{criteriaToolResponse(AcceptanceTaskExecution, "bounded", "Implement this request.", input)}
	for i := 1; i <= 3; i++ {
		candidateID := fmt.Sprintf("candidate-%d", i)
		responses = append(responses,
			hostTextResponse(fmt.Sprintf("Candidate %d.", i)),
			acceptanceToolResponse(candidateID, "bounded", AcceptanceFailed, "candidate:"+candidateID),
		)
	}
	responses = append(responses, hostTextResponse("Candidate 4, corrected."), acceptanceToolResponse("candidate-4", "bounded", AcceptancePassed, "candidate:candidate-4"))
	provider := &scriptedHostProvider{manualAcceptance: true, responses: responses}
	client := newAcceptanceCore(t, provider, nil)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
	if !errors.Is(runErr, ErrTaskPaused) || result.Status != TaskPaused || len(result.AcceptanceReports) != 3 {
		t.Fatalf("RunTask = %#v, error=%v; want pause after three reports", result, runErr)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := runtime.PendingAcceptance(context.Background())
	if err != nil || !found || pending.Repairs != 2 || pending.Reason != AcceptancePauseRepairLimit || len(pending.Reports) != 3 {
		t.Fatalf("pending acceptance = %#v, found=%v, error=%v", pending, found, err)
	}
	if err := runtime.ResolveAcceptance(context.Background(), AcceptanceResolution{
		RunID: pending.RunID, InvocationID: pending.InvocationID, CallID: pending.CallID, Decision: AcceptanceContinue,
	}); err != nil {
		t.Fatalf("continue acceptance: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completed, err := runtime.WaitTurn(ctx, result.ExecutionID)
	if err != nil || completed.Status != TaskCompleted || completed.ExecutionID != result.ExecutionID || len(completed.AcceptanceReports) != 4 {
		t.Fatalf("resumed result = %#v, error=%v", completed, err)
	}
	if completed.AcceptanceReports[0].CandidateID != "candidate-1" || completed.AcceptanceReports[3].CandidateID != "candidate-4" {
		t.Fatalf("acceptance history after continue = %#v", completed.AcceptanceReports)
	}
}

func TestAcceptancePauseBlocksLaterSessionTurns(t *testing.T) {
	provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskAnswer, "first", "Answer the first question.", "first question"),
		hostTextResponse("First answer."),
		acceptanceToolResponse("candidate-1", "first", AcceptancePassed, "candidate:candidate-1"),
		criteriaToolResponse(AcceptanceTaskAnswer, "second", "Answer the second question.", "second question"),
		hostTextResponse("Second answer."),
		acceptanceToolResponse("candidate-2", "second", AcceptancePassed, "candidate:candidate-2"),
	}}
	var verifierCalls int
	verifier := acceptanceVerifierFunc(func(_ context.Context, request AcceptanceVerificationInput) (AcceptanceReport, error) {
		verifierCalls++
		outcome := AcceptancePassed
		reason := "The host verified the second answer."
		if verifierCalls == 1 {
			outcome, reason = AcceptanceUnknown, "The first answer needs host confirmation."
		}
		return AcceptanceReport{Results: []AcceptanceCriterionResult{{
			CriterionID: request.Criteria[0].ID, Outcome: outcome, Evidence: []string{"candidate:candidate-1"}, Reason: reason,
		}}}, nil
	})
	client := newAcceptanceCore(t, provider, verifier)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "first question")}, StartIfIdle)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := runtime.WaitTurn(context.Background(), first.ExecutionID)
	if err != nil || paused.Status != TaskPaused {
		t.Fatalf("first turn = %#v, error=%v", paused, err)
	}
	second, err := runtime.SubmitTurn(context.Background(), TurnRequest{Input: mustSettingsTaskInput(t, "second question")}, QueueInput)
	if err != nil || second.Status != SubmissionQueued {
		t.Fatalf("second turn submission = %#v, error=%v", second, err)
	}
	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if requests != 3 {
		t.Fatalf("provider ran later turn while acceptance was paused: %d model requests", requests)
	}
	pending, found, err := runtime.PendingAcceptance(context.Background())
	if err != nil || !found {
		t.Fatalf("pending first acceptance = %#v, found=%v, error=%v", pending, found, err)
	}
	if err := runtime.ResolveAcceptance(context.Background(), AcceptanceResolution{
		RunID: pending.RunID, InvocationID: pending.InvocationID, CallID: pending.CallID, Decision: AcceptanceConfirm,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completedFirst, err := runtime.WaitTurn(ctx, first.ExecutionID)
	if err != nil || completedFirst.Status != TaskCompleted {
		t.Fatalf("confirmed first turn = %#v, error=%v", completedFirst, err)
	}
	completedSecond, err := runtime.WaitTurn(ctx, second.ExecutionID)
	if err != nil || completedSecond.Status != TaskCompleted || verifierCalls != 2 {
		t.Fatalf("second turn after acceptance = %#v, verifier calls=%d, error=%v", completedSecond, verifierCalls, err)
	}
}

func TestHostUnknownPausesAndExplicitConfirmCompletesAsUser(t *testing.T) {
	input := "Answer the current question"
	provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskAnswer, "answer", "Answer the current question.", input),
		hostTextResponse("The answer is ready."),
		acceptanceToolResponse("candidate-1", "answer", AcceptancePassed, "candidate:candidate-1"),
	}}
	verifier := acceptanceVerifierFunc(func(context.Context, AcceptanceVerificationInput) (AcceptanceReport, error) {
		return AcceptanceReport{}, errors.New("checker unavailable")
	})
	client := newAcceptanceCore(t, provider, verifier)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
	if !errors.Is(runErr, ErrTaskPaused) || result.Status != TaskPaused || len(result.AcceptanceReports) != 1 || result.AcceptanceReports[0].Source != AcceptanceSourceHost || result.AcceptanceReports[0].Results[0].Outcome != AcceptanceUnknown {
		t.Fatalf("RunTask = %#v, error=%v", result, runErr)
	}
	runtime, err := client.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := runtime.PendingRequests(context.Background())
	if err != nil || len(requests) == 0 || requests[0].Acceptance == nil {
		t.Fatalf("pending requests = %#v, error=%v", requests, err)
	}
	pending := requests[0].Acceptance
	if pending.Reason != AcceptancePauseUnknown || pending.Reports[0].Source != AcceptanceSourceHost {
		t.Fatalf("pending acceptance = %#v", pending)
	}
	if _, err := runtime.Recover(context.Background()); err == nil || !strings.Contains(err.Error(), "explicit continue or confirm") {
		t.Fatalf("generic recovery bypassed acceptance decision: %v", err)
	}
	eventContext, stopEvents := context.WithCancel(context.Background())
	events, err := runtime.SubscribeEvents(eventContext, 0)
	if err != nil {
		stopEvents()
		t.Fatal(err)
	}
	seenAcceptanceRequired := false
	deadline := time.After(5 * time.Second)
	for !seenAcceptanceRequired {
		select {
		case event := <-events:
			if event.Kind != EventAcceptanceRequired {
				continue
			}
			seenAcceptanceRequired = true
			if event.AcceptanceSource != AcceptanceSourceHost || event.AcceptanceOutcome != AcceptanceUnknown || event.AcceptanceReason != AcceptancePauseUnknown || len(event.AcceptanceCriterionIDs) != 1 {
				t.Fatalf("acceptance event = %#v", event)
			}
			encoded, _ := json.Marshal(event)
			if strings.Contains(string(encoded), "The answer is ready") || strings.Contains(string(encoded), "Answer the current question") {
				t.Fatalf("durable acceptance event leaked body text: %s", encoded)
			}
		case <-deadline:
			t.Fatal("missing acceptance-required lifecycle event")
		}
	}
	stopEvents()
	for range events {
	}
	if err := runtime.ResolveAcceptance(context.Background(), AcceptanceResolution{
		RunID: pending.RunID, InvocationID: pending.InvocationID, CallID: pending.CallID, Decision: AcceptanceConfirm,
	}); err != nil {
		t.Fatalf("confirm acceptance: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completed, err := runtime.WaitTurn(ctx, result.ExecutionID)
	if err != nil || completed.Status != TaskCompleted || len(completed.AcceptanceReports) != 2 || completed.AcceptanceReports[1].Source != AcceptanceSourceUser {
		t.Fatalf("confirmed result = %#v, error=%v", completed, err)
	}
}

func TestAcceptancePauseAndReportsSurviveCoreRestart(t *testing.T) {
	input := "Implement the change using the selected source"
	tool := &hostEchoTool{}
	provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskExecution, "change", "Implement the requested change.", input),
		hostToolCallResponse("select-host", "select_tool_category", `{"category":"host"}`),
		hostToolCallResponse("write-call", "host_echo", `{}`),
		hostTextResponse("The selected source was applied."),
		acceptanceToolResponse("candidate-1", "change", AcceptancePassed, "candidate:candidate-1"),
	}}
	verifier := acceptanceVerifierFunc(func(context.Context, AcceptanceVerificationInput) (AcceptanceReport, error) {
		return AcceptanceReport{Results: []AcceptanceCriterionResult{{
			CriterionID: "change", Outcome: AcceptanceUnknown, Evidence: []string{"candidate:candidate-1"}, Reason: "Needs human confirmation.",
		}}}, nil
	})
	workspace, stateDir := t.TempDir(), t.TempDir()
	config := runtimeTestConfig(workspace, stateDir, provider)
	config.AcceptanceVerifier = verifier
	config.Tools = []publictool.Registration{{Tool: tool, Metadata: publictool.Metadata{
		Category:        publictool.Category{Name: "host", Description: "Host supplied tools."},
		PermissionClass: publictool.PermissionWorkspaceWrite,
	}}}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := true
	t.Cleanup(func() {
		if cleanup {
			if err := client.Close(); err != nil {
				t.Errorf("close first Core: %v", err)
			}
		}
	})
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	paused, runErr := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
	if !errors.Is(runErr, ErrTaskPaused) || paused.Status != TaskPaused || len(paused.AcceptanceReports) != 1 || len(paused.AcceptanceCriteria) != 1 || len(paused.AcceptanceEvidence) < 2 {
		t.Fatalf("paused result = %#v, error=%v", paused, runErr)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	cleanup = false

	reopened, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened Core: %v", err)
		}
	})
	runtime, err := reopened.OpenSessionRuntime(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := runtime.PendingAcceptance(context.Background())
	if err != nil || !found || len(pending.Criteria) != 1 || len(pending.Reports) != 1 || pending.Reports[0].Source != AcceptanceSourceHost {
		t.Fatalf("recovered pending acceptance = %#v, found=%v, error=%v", pending, found, err)
	}
	if err := runtime.ResolveAcceptance(context.Background(), AcceptanceResolution{
		RunID: pending.RunID, InvocationID: pending.InvocationID, CallID: pending.CallID, Decision: AcceptanceConfirm,
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completed, err := runtime.WaitTurn(ctx, paused.ExecutionID)
	if err != nil || completed.Status != TaskCompleted || len(completed.AcceptanceReports) != 2 || completed.AcceptanceReports[1].Source != AcceptanceSourceUser || len(completed.AcceptanceCriteria) != 1 || len(completed.AcceptanceEvidence) < 2 {
		t.Fatalf("completed result after restart = %#v, error=%v", completed, err)
	}
	tool.mu.Lock()
	calls := tool.calls
	tool.mu.Unlock()
	if calls != 1 {
		t.Fatalf("completed tool ran %d times across acceptance pause recovery; want once", calls)
	}
}

func TestAcceptanceInvalidEvidenceGetsOneFormatCorrection(t *testing.T) {
	input := "Answer with the cited result"
	bad := acceptanceToolResponse("candidate-1", "answer", AcceptancePassed, "tool:missing")
	provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskAnswer, "answer", "Answer with the cited result.", input),
		hostTextResponse("The result is 7."),
		bad,
		acceptanceToolResponse("candidate-1", "answer", AcceptancePassed, "candidate:candidate-1"),
	}}
	client := newAcceptanceCore(t, provider, nil)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
	if err != nil || result.Status != TaskCompleted || len(result.AcceptanceReports) != 1 || result.AcceptanceReports[0].Source != AcceptanceSourceModel {
		t.Fatalf("RunTask = %#v, error=%v", result, err)
	}
}

func TestAcceptanceModelFailurePausesAsUnknown(t *testing.T) {
	input := "Answer the current question"
	provider := &scriptedHostProvider{manualAcceptance: true, responses: []*publicmodel.ModelResponse{
		criteriaToolResponse(AcceptanceTaskAnswer, "answer", "Answer the current question.", input),
		hostTextResponse("The answer is ready."),
	}}
	client := newAcceptanceCore(t, provider, nil)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: mustSettingsTaskInput(t, input)})
	if !errors.Is(runErr, ErrTaskPaused) || result.Status != TaskPaused || len(result.AcceptanceReports) != 1 ||
		result.AcceptanceReports[0].Source != AcceptanceSourceModel || result.AcceptanceReports[0].Results[0].Outcome != AcceptanceUnknown {
		t.Fatalf("RunTask = %#v, error=%v", result, runErr)
	}
}
