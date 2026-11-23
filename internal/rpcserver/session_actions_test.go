package rpcserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/model"
)

func TestDesktopSessionActions(t *testing.T) {
	provider := &rpcBarrierProvider{tools: make(chan []model.ResponseTool, 1), entered: make(chan struct{}, 4), release: make(chan struct{}, 4), cancelled: make(chan struct{}, 4)}
	runtimeCore, err := core.New(core.Config{
		Workspace: t.TempDir(), StateDir: t.TempDir(), Memory: core.MemoryConfig{Disabled: true},
		ModelConfig: &model.Config{
			Providers: map[string]model.ProviderConfig{"fixture": {Protocol: model.ProtocolResponses}},
			Profiles: map[string]model.ModelProfile{
				"fixture": {Provider: "fixture", Model: "fixture-model"},
				"vision":  {Provider: "fixture", Model: "fixture-model", SupportsInputImages: true},
			},
			DefaultProfile: "fixture",
		},
		ModelProviders: map[string]model.Provider{"fixture": provider},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeCore.Close(context.Background()) })
	service, err := New(runtimeCore, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(service)
	t.Cleanup(server.Close)
	first := dialTestRPC(t, server.URL, "test-token", server.Certificate())
	second := dialTestRPC(t, server.URL, "test-token", server.Certificate())
	t.Cleanup(first.close)
	t.Cleanup(second.close)

	var original sessionSummary
	decodeResult(t, first.call(t, "session.create", map[string]any{"history": []map[string]string{
		{"type": "message", "role": "user", "content": "Earlier searchable conversation"},
	}}), &original)
	id := original.ID
	var source map[string]any
	decodeResult(t, first.call(t, "session.setArchived", map[string]any{"sessionId": id, "archived": true}), &source)
	var results []sessionSummary
	decodeResult(t, second.call(t, "session.search", map[string]any{"query": "SEARCHABLE", "includeArchived": true}), &results)
	if len(results) != 1 || !results[0].Archived || results[0].ID != id {
		t.Fatalf("search = %+v", results)
	}
	decodeResult(t, second.call(t, "session.search", map[string]any{"query": "SEARCHABLE"}), &results)
	if len(results) != 0 {
		t.Fatal("archived conversation appeared in active search")
	}
	var fork sessionSummary
	decodeResult(t, second.call(t, "session.fork", map[string]any{"sessionId": id}), &fork)
	if fork.ID == id {
		t.Fatal("fork reused the session ID")
	}

	var preview struct {
		Digest  string
		Removed int
	}
	decodeResult(t, first.call(t, "session.rewindPreview", map[string]any{"sessionId": id, "turns": 1}), &preview)
	if preview.Removed != 1 {
		t.Fatalf("preview = %+v", preview)
	}
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aFioAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	submit := map[string]any{"sessionId": id, "submissionId": "attachment-turn", "input": "with text", "planMode": true,
		"attachments": []attachment{
			{Name: "notes.md", MediaType: "text/markdown", Data: []byte("local attachment notes")},
			{Name: "pixel.png", MediaType: "image/png", Data: png},
		},
	}
	if rejected := first.call(t, "turn.submit", submit); rejected.Error == nil || !strings.Contains(rejected.Error.Message, "not configured") {
		t.Fatalf("non-vision model rejection = %+v", rejected)
	}
	for _, invalid := range []attachment{
		{Name: "archive.zip", MediaType: "application/zip", Data: []byte("no")},
		{Name: "huge.txt", MediaType: "text/plain", Data: make([]byte, maxAttachmentTextBytes+1)},
		{Name: "spoofed.png", MediaType: "image/png", Data: []byte("not an image")},
	} {
		reply := first.call(t, "turn.submit", map[string]any{"sessionId": id, "submissionId": "bad", "input": "invalid", "attachments": []attachment{invalid}})
		if reply.Error == nil {
			t.Fatalf("accepted invalid %s", invalid.Name)
		}
	}
	decodeResult(t, first.call(t, "session.setSettings", map[string]any{"sessionId": id, "settings": map[string]string{"modelProfile": "vision"}}), &source)
	var active core.TurnSubmission
	decodeResult(t, first.call(t, "turn.submit", submit), &active)
	awaitProviderCalls(t, provider.entered, 1)
	readable := false
	for _, spec := range <-provider.tools {
		if tool, ok := spec.(model.FunctionTool); ok {
			if tool.Name == "write_file" || tool.Name == "apply_patch" {
				t.Fatalf("Plan turn exposed writable tool %s", tool.Name)
			}
			readable = readable || tool.Name == "read_file"
		}
	}
	if !readable {
		t.Fatal("Plan turn did not expose the read tool")
	}
	// A repeated request retrieves its receipt even after the user's model changes.
	decodeResult(t, first.call(t, "session.setSettings", map[string]any{"sessionId": id, "settings": map[string]string{"modelProfile": "fixture"}}), &source)

	var retry core.TurnSubmission
	decodeResult(t, second.call(t, "turn.submit", submit), &retry)
	if retry.ExecutionID != active.ExecutionID {
		t.Fatal("retried attachment started another turn")
	}

	var queued core.TurnSubmission
	decodeResult(t, second.call(t, "turn.submit", map[string]any{"sessionId": id, "submissionId": "queued", "input": "queued work", "mode": "queue_input"}), &queued)
	if reply := first.call(t, "session.compact", map[string]any{"sessionId": id}); reply.Error == nil {
		t.Fatal("compaction admitted with active and queued turns")
	}
	decodeResult(t, second.call(t, "turn.cancelQueued", map[string]any{"sessionId": id, "executionId": queued.ExecutionID}), &source)
	if source["cancelled"] != true {
		t.Fatalf("cancellation returned %v", source)
	}
	if reply := first.call(t, "turn.cancelQueued", map[string]any{"sessionId": id, "executionId": active.ExecutionID}); reply.Error == nil {
		t.Fatal("queue cancellation interrupted active turn")
	}
	provider.release <- struct{}{}
	var finished core.TaskResult
	decodeResult(t, first.call(t, "turn.wait", map[string]any{"sessionId": id, "executionId": active.ExecutionID}), &finished)
	if finished.Status != "completed" {
		t.Fatalf("active turn status = %q", finished.Status)
	}

	page := first.call(t, "session.historyPage", map[string]any{"sessionId": id})
	if page.Error != nil || (!strings.Contains(string(page.Result), "local attachment notes") || !strings.Contains(string(page.Result), "data:image/png;base64,")) {
		t.Fatalf("history page = %+v", page)
	}
	if reply := first.call(t, "session.rewind", map[string]any{"sessionId": id, "turns": 1, "digest": preview.Digest}); reply.Error == nil {
		t.Fatal("rewind accepted a stale preview")
	}
	decodeResult(t, first.call(t, "session.rewindPreview", map[string]any{"sessionId": id, "turns": 2}), &preview)
	decodeResult(t, first.call(t, "session.rewind", map[string]any{"sessionId": id, "turns": 2, "digest": preview.Digest}), &source)
	if source["removed"] != float64(2) {
		t.Fatalf("rewind = %v", source)
	}
	var forkHistory json.RawMessage
	decodeResult(t, second.call(t, "session.history", map[string]any{"sessionId": fork.ID}), &forkHistory)
	if !strings.Contains(string(forkHistory), "searchable conversation") || strings.Contains(string(forkHistory), "local attachment notes") {
		t.Fatalf("fork history = %s", forkHistory)
	}
}
