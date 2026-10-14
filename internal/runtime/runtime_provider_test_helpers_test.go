package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	publicmodel "github.com/afterlune/luneCore/model"
)

type scriptedHostProvider struct {
	mu               sync.Mutex
	responses        []*publicmodel.ModelResponse
	requests         int
	seenRequests     []publicmodel.ModelRequest
	manualAcceptance bool
}

func (p *scriptedHostProvider) Name() string { return "host" }

func (p *scriptedHostProvider) CreateResponse(_ context.Context, request publicmodel.ModelRequest, _ publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stage := acceptanceRequestStage(request)
	if !p.manualAcceptance && (stage == "criteria" || stage == "review") && !scriptHasRequestedTool(p.responses, request) {
		if stage == "criteria" {
			return scriptedCriteriaResponse(request), nil
		}
		return scriptedAcceptanceResponse(request), nil
	}
	if len(p.responses) == 0 {
		return nil, errors.New("scripted provider ran out of responses")
	}
	p.seenRequests = append(p.seenRequests, request)
	response := p.responses[0]
	p.responses = p.responses[1:]
	p.requests++
	return response, nil
}

func acceptanceRequestStage(request publicmodel.ModelRequest) string {
	var names []string
	for _, definition := range request.Tools {
		function, ok := definition.(publicmodel.FunctionTool)
		if !ok {
			continue
		}
		names = append(names, function.Name)
	}
	return acceptanceRequestStageNames(names)
}

func acceptanceRequestStageNames(names []string) string {
	for _, name := range names {
		switch name {
		case "prepare_task_criteria":
			return "criteria"
		case "submit_acceptance", "request_acceptance_evidence":
			return "review"
		}
	}
	return "execution"
}

func scriptHasRequestedTool(responses []*publicmodel.ModelResponse, request publicmodel.ModelRequest) bool {
	if len(responses) == 0 {
		return false
	}
	allowed := make(map[string]bool)
	for _, definition := range request.Tools {
		if function, ok := definition.(publicmodel.FunctionTool); ok {
			allowed[function.Name] = true
		}
	}
	for _, item := range responses[0].Output {
		if call, ok := item.(publicmodel.OutputFunctionCall); ok && allowed[call.Name] {
			return true
		}
	}
	return false
}

func scriptedCriteriaResponse(request publicmodel.ModelRequest) *publicmodel.ModelResponse {
	text := scriptedCurrentTaskInput(request)
	kind := "answer"
	lower := strings.ToLower(text)
	if strings.Contains(lower, "plan") || strings.Contains(text, "计划") || strings.Contains(text, "规划") {
		kind = "plan"
	} else if strings.Contains(lower, "implement") || strings.Contains(lower, "build") || strings.Contains(lower, "fix") || strings.Contains(text, "实现") || strings.Contains(text, "修改") || strings.Contains(text, "修复") {
		kind = "execution"
	}
	if text == "" {
		text = "current user turn"
	}
	arguments, _ := json.Marshal(map[string]any{
		"kind":     kind,
		"criteria": []map[string]string{{"id": "current_request", "description": "Satisfy the current user request within its stated scope.", "basis": text}},
	})
	return &publicmodel.ModelResponse{Status: "completed", Output: []publicmodel.ResponseOutputItem{
		publicmodel.OutputFunctionCall{CallID: "test-criteria", Name: "prepare_task_criteria", Arguments: string(arguments)},
	}}
}

func scriptedCurrentTaskInput(request publicmodel.ModelRequest) string {
	if request.Input == nil {
		return scriptedCurrentTask(request.Instructions)
	}
	if request.Input.Text != nil {
		return strings.TrimSpace(*request.Input.Text)
	}
	for _, item := range request.Input.Items {
		message, ok := item.(publicmodel.InputMessage)
		if !ok || message.Role != publicmodel.RoleUser {
			continue
		}
		if message.Content.Text != nil {
			return strings.TrimSpace(*message.Content.Text)
		}
		for _, part := range message.Content.Parts {
			if text, ok := part.(publicmodel.InputText); ok {
				return strings.TrimSpace(text.Text)
			}
		}
	}
	return scriptedCurrentTask(request.Instructions)
}

func scriptedAcceptanceResponse(request publicmodel.ModelRequest) *publicmodel.ModelResponse {
	instructions := request.Instructions
	candidateID := "candidate-1"
	if marker := "Candidate ID: "; strings.Contains(instructions, marker) {
		rest := strings.SplitN(instructions[strings.Index(instructions, marker)+len(marker):], "\n", 2)[0]
		if strings.TrimSpace(rest) != "" {
			candidateID = strings.TrimSpace(rest)
		}
	}
	var criteria []struct {
		ID string `json:"id"`
	}
	if marker := "Fixed criteria: "; strings.Contains(instructions, marker) {
		rest := instructions[strings.Index(instructions, marker)+len(marker):]
		if end := strings.Index(rest, "\n\nCandidate ID:"); end >= 0 {
			_ = json.Unmarshal([]byte(rest[:end]), &criteria)
		}
	}
	results := make([]map[string]any, 0, len(criteria))
	for _, criterion := range criteria {
		results = append(results, map[string]any{
			"criterion_id": criterion.ID, "outcome": "passed", "evidence": []string{"candidate:" + candidateID},
			"reason": "The scripted test provider accepts this candidate.",
		})
	}
	arguments, _ := json.Marshal(map[string]any{"candidate_id": candidateID, "results": results})
	return &publicmodel.ModelResponse{Status: "completed", Output: []publicmodel.ResponseOutputItem{
		publicmodel.OutputFunctionCall{CallID: "test-acceptance", Name: "submit_acceptance", Arguments: string(arguments)},
	}}
}

func scriptedCurrentTask(instructions string) string {
	marker := "Current user turn text (attachments remain in the conversation input): "
	index := strings.Index(instructions, marker)
	if index < 0 {
		return ""
	}
	var text string
	text = strings.TrimSpace(instructions[index+len(marker):])
	if end := strings.Index(text, "\n\n"); end >= 0 {
		text = text[:end]
	}
	return strings.Trim(text, "\" ")
}

func (p *scriptedHostProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, emit func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	response, err := p.CreateResponse(ctx, request, profile)
	if err != nil {
		return nil, err
	}
	if emit != nil {
		if err := emit(publicmodel.ModelStreamEvent{Type: publicmodel.ModelStreamCompleted, Response: response}); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func hostTextResponse(text string) *publicmodel.ModelResponse {
	return &publicmodel.ModelResponse{
		Status: "completed",
		Output: []publicmodel.ResponseOutputItem{publicmodel.OutputMessage{
			Role: publicmodel.RoleAssistant, Content: []publicmodel.OutputContentPart{publicmodel.OutputText{Text: text}},
		}},
	}
}

func hostToolCallResponse(callID, name, arguments string) *publicmodel.ModelResponse {
	return &publicmodel.ModelResponse{
		Status: "completed",
		Output: []publicmodel.ResponseOutputItem{publicmodel.OutputFunctionCall{CallID: callID, Name: name, Arguments: arguments}},
	}
}
