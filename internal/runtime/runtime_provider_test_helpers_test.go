package runtime

import (
	"context"
	"errors"
	"sync"

	publicmodel "github.com/afterlune/luneCore/model"
)

type scriptedHostProvider struct {
	mu           sync.Mutex
	responses    []*publicmodel.ModelResponse
	requests     int
	seenRequests []publicmodel.ModelRequest
}

func (p *scriptedHostProvider) Name() string { return "host" }

func (p *scriptedHostProvider) CreateResponse(_ context.Context, request publicmodel.ModelRequest, _ publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.responses) == 0 {
		return nil, errors.New("scripted provider ran out of responses")
	}
	p.seenRequests = append(p.seenRequests, request)
	response := p.responses[0]
	p.responses = p.responses[1:]
	p.requests++
	return response, nil
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
