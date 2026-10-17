package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	publicmodel "github.com/afterlune/luneCore/model"
)

func TestCoreRunsDifferentSessionsConcurrentlyAndSerializesEachSession(t *testing.T) {
	provider := &barrierHostProvider{entered: make(chan string, 3), release: make(chan struct{}, 2)}
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
	go func() {
		input, _ := NewTaskInput("first session")
		_, err := client.RunTask(ctx, TaskRequest{SessionID: first.ID, Input: input})
		firstDone <- err
	}()
	select {
	case <-provider.entered:
	case <-ctx.Done():
		t.Fatal("first session did not reach the provider")
	}
	secondDone := make(chan error, 1)
	go func() {
		input, _ := NewTaskInput("second session")
		_, err := client.RunTask(ctx, TaskRequest{SessionID: second.ID, Input: input})
		secondDone <- err
	}()
	select {
	case <-provider.entered:
	case <-ctx.Done():
		t.Fatal("second session did not run while the first session was active")
	}
	input, _ := NewTaskInput("duplicate session task")
	if _, err := client.RunTask(ctx, TaskRequest{SessionID: first.ID, Input: input}); err == nil {
		t.Fatal("started a second task in the same session")
	}
	provider.release <- struct{}{}
	provider.release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatalf("first task: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second task: %v", err)
	}
	if firstState, _ := client.StateForSession(first.ID); firstState.Status != TaskCompleted {
		t.Fatalf("first task status = %q", firstState.Status)
	}
	if secondState, _ := client.StateForSession(second.ID); secondState.Status != TaskCompleted {
		t.Fatalf("second task status = %q", secondState.Status)
	}
	if running := client.ActiveTasks(); len(running) != 0 {
		t.Fatalf("ActiveTasks after completion = %#v", running)
	}
}

type barrierHostProvider struct {
	entered chan string
	release chan struct{}
}

func (p *barrierHostProvider) Name() string { return "host" }

func (p *barrierHostProvider) CreateResponse(ctx context.Context, request publicmodel.ModelRequest, _ publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	text := latestUserInputText(request.Input)
	select {
	case p.entered <- text:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return hostTextResponse("done"), nil
}

func latestUserInputText(input *publicmodel.ResponseInput) string {
	if input == nil {
		return ""
	}
	if input.Text != nil {
		return *input.Text
	}
	for index := len(input.Items) - 1; index >= 0; index-- {
		message, ok := input.Items[index].(publicmodel.InputMessage)
		if !ok || message.Role != publicmodel.RoleUser {
			continue
		}
		if message.Content.Text != nil {
			return *message.Content.Text
		}
		var text string
		for _, part := range message.Content.Parts {
			if inputText, ok := part.(publicmodel.InputText); ok {
				text += inputText.Text
			}
		}
		return text
	}
	return ""
}

func (p *barrierHostProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, emit func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	response, err := p.CreateResponse(ctx, request, profile)
	if err == nil && emit != nil {
		err = emit(publicmodel.ModelStreamEvent{Type: publicmodel.ModelStreamCompleted, Response: response})
	}
	return response, err
}
