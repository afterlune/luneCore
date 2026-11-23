package rpcserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/model"
	"github.com/afterlune/luneCore/tool"
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
	var secondQueued core.TurnSubmission
	decodeResult(t, second.call(t, "turn.submit", map[string]any{"sessionId": id, "submissionId": "queued-2", "input": "second queued work", "mode": "queue_input"}), &secondQueued)
	var replaced struct {
		Submission            core.TurnSubmission `json:"submission"`
		CancelledExecutionIDs []string            `json:"cancelledExecutionIds"`
	}
	decodeResult(t, second.call(t, "turn.replaceQueued", map[string]any{
		"sessionId": id, "executionId": queued.ExecutionID, "input": "updated queued work",
	}), &replaced)
	if replaced.Submission.ExecutionID != queued.ExecutionID || len(replaced.CancelledExecutionIDs) != 1 || replaced.CancelledExecutionIDs[0] != secondQueued.ExecutionID {
		t.Fatalf("turn.replaceQueued = %+v", replaced)
	}
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

	var pendingActive, pendingQueued core.TurnSubmission
	decodeResult(t, first.call(t, "turn.submit", map[string]any{"sessionId": id, "submissionId": "pending-active", "input": "active to cancel"}), &pendingActive)
	awaitProviderCalls(t, provider.entered, 1)
	decodeResult(t, first.call(t, "turn.submit", map[string]any{"sessionId": id, "submissionId": "pending-queued", "input": "queued to cancel", "mode": "queue_input"}), &pendingQueued)
	var cancelledPending struct {
		CancelledExecutionIDs []string `json:"cancelledExecutionIds"`
	}
	decodeResult(t, first.call(t, "turn.cancelPending", map[string]any{"sessionId": id}), &cancelledPending)
	if len(cancelledPending.CancelledExecutionIDs) != 2 {
		t.Fatalf("turn.cancelPending = %+v", cancelledPending)
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

type scriptedRPCProvider struct {
	mu        sync.Mutex
	responses []*model.ModelResponse
	requests  []model.ModelRequest
}

func (*scriptedRPCProvider) Name() string { return "fixture" }

func (p *scriptedRPCProvider) CreateResponse(_ context.Context, req model.ModelRequest, _ model.ModelProfile) (*model.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.responses) == 0 {
		return rpcTextResponse(), nil
	}
	next := p.responses[0]
	p.responses = p.responses[1:]
	return next, nil
}

func (p *scriptedRPCProvider) StreamResponse(ctx context.Context, req model.ModelRequest, profile model.ModelProfile, emit func(model.ModelStreamEvent) error) (*model.ModelResponse, error) {
	resp, err := p.CreateResponse(ctx, req, profile)
	if err == nil && emit != nil {
		err = emit(model.ModelStreamEvent{Type: model.ModelStreamCompleted, Response: resp})
	}
	return resp, err
}

type backgroundCommandExecutor struct {
	started chan struct{}
}

func (e *backgroundCommandExecutor) Execute(ctx context.Context, req tool.CommandRequest) (tool.CommandResult, error) {
	if req.OnOutput != nil {
		_ = req.OnOutput([]byte("daemon-ready\n"))
	}
	select {
	case e.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	code := 0
	return tool.CommandResult{Output: "daemon-ready\n", ExitCode: &code}, ctx.Err()
}

func TestExtendedRuntimeProtocolCapabilities(t *testing.T) {
	var mcpRequestMu sync.Mutex
	mcpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpcReq struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params map[string]any    `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpcReq); err != nil {
			t.Errorf("decode MCP request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeResult := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      rpcReq.ID,
				"result":  result,
			})
		}
		switch rpcReq.Method {
		case "tools/list":
			writeResult(map[string]any{"tools": []any{}})
		case "prompts/list":
			writeResult(map[string]any{"prompts": []any{
				map[string]any{"name": "review", "title": "Code Review", "description": "Review code", "arguments": []any{
					map[string]any{"name": "focus", "required": true},
				}},
			}})
		case "prompts/get":
			mcpRequestMu.Lock()
			_, hasResponses := rpcReq.Params["inputResponses"]
			mcpRequestMu.Unlock()
			if !hasResponses {
				writeResult(map[string]any{
					"resultType": "input_required",
					"inputRequests": map[string]any{
						"ask": map[string]any{
							"method": "elicitation/create",
							"params": map[string]any{"mode": "form"},
						},
					},
				})
				return
			}
			writeResult(map[string]any{
				"description": "Reviewed prompt",
				"messages": []any{
					map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "Prompt body"}},
				},
			})
		case "resources/list":
			writeResult(map[string]any{"resources": []any{
				map[string]any{"uri": "urn:doc:1", "name": "doc1", "mimeType": "text/plain", "size": 12},
			}})
		case "resources/templates/list":
			writeResult(map[string]any{"resourceTemplates": []any{
				map[string]any{"uriTemplate": "urn:doc:{id}", "name": "doc-template", "mimeType": "text/plain"},
			}})
		case "resources/read":
			writeResult(map[string]any{"contents": []any{
				map[string]any{"uri": "urn:doc:1", "mimeType": "text/plain", "text": "Resource content"},
			}})
		default:
			t.Errorf("unexpected MCP method %q", rpcReq.Method)
		}
	}))
	t.Cleanup(mcpServer.Close)

	cmdExec := &backgroundCommandExecutor{started: make(chan struct{}, 1)}
	provider := &scriptedRPCProvider{
		responses: []*model.ModelResponse{
			// 1. Plan turn response
			{Status: "completed", Output: []model.ResponseOutputItem{
				model.OutputMessage{Role: model.RoleAssistant, Content: []model.OutputContentPart{
					model.OutputText{Text: "<proposed_plan>\n1. Inspect files\n2. Run tests\n</proposed_plan>"},
				}},
			}},
			// 2. Plan implementation turn response
			rpcTextResponse(),
			// 3. Question turn response (for question.saveDraft)
			{Status: "completed", Output: []model.ResponseOutputItem{
				model.OutputFunctionCall{CallID: "draft-call", Name: "ask_user", Arguments: `{"questions":[{"id":"q1","question":"Pick DB?","options":[{"label":"SQLite"},{"label":"Postgres"}]}]}`},
			}},
			// 4. Resume after question resolved
			rpcTextResponse(),
			// 5. Background command turn response
			{Status: "completed", Output: []model.ResponseOutputItem{
				model.OutputFunctionCall{CallID: "bg-cmd", Name: "command", Arguments: `{"command":"echo daemon","background":true,"yield_time_ms":50}`},
			}},
			rpcTextResponse(),
		},
	}

	projectCfg := core.DefaultProjectConfig()
	projectCfg.Models = model.Config{
		Providers:      map[string]model.ProviderConfig{"fixture": {Protocol: model.ProtocolResponses}},
		Profiles:       map[string]model.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
		DefaultProfile: "fixture",
	}
	projectCfg.MCPServers = map[string]core.MCPServerConfig{
		"docs": {Transport: core.MCPTransportStreamableHTTP, URL: mcpServer.URL},
	}

	runtimeCore, err := core.New(core.Config{
		Workspace:       t.TempDir(),
		StateDir:        t.TempDir(),
		Memory:          core.MemoryConfig{Disabled: true},
		ProjectConfig:   &projectCfg,
		ModelProviders:  map[string]model.Provider{"fixture": provider},
		CommandExecutor: cmdExec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeCore.SetPermissionMode(core.PermissionFullAccess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeCore.Close(context.Background()) })

	service, err := New(runtimeCore, "ext-token")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(service)
	t.Cleanup(server.Close)
	client := dialTestRPC(t, server.URL, "ext-token", server.Certificate())
	t.Cleanup(client.close)

	var session sessionSummary
	decodeResult(t, client.call(t, "session.create", map[string]any{}), &session)

	// 1. Test Plan turn + turn.implementPlan (new_context)
	var planSub core.TurnSubmission
	decodeResult(t, client.call(t, "turn.submit", map[string]any{
		"sessionId": session.ID, "submissionId": "plan-1", "input": "Create a plan", "planMode": true,
	}), &planSub)
	var planRes core.TaskResult
	decodeResult(t, client.call(t, "turn.wait", map[string]any{
		"sessionId": session.ID, "executionId": planSub.ExecutionID,
	}), &planRes)
	if planRes.ProposedPlan == nil || !strings.Contains(planRes.ProposedPlan.Text, "Inspect files") {
		t.Fatalf("proposed plan = %#v", planRes.ProposedPlan)
	}
	var impl core.PlanImplementation
	decodeResult(t, client.call(t, "turn.implementPlan", map[string]any{
		"sessionId": session.ID, "sourceExecutionId": planSub.ExecutionID, "mode": "new_context",
	}), &impl)
	if impl.SessionID == "" || impl.SessionID == session.ID {
		t.Fatalf("implementPlan session = %q, source = %q", impl.SessionID, session.ID)
	}
	var implRes core.TaskResult
	decodeResult(t, client.call(t, "turn.wait", map[string]any{
		"sessionId": impl.SessionID, "executionId": impl.Submission.ExecutionID,
	}), &implRes)
	if implRes.Status != core.TaskCompleted {
		t.Fatalf("implemented plan status = %q", implRes.Status)
	}

	// 2. Test question.saveDraft + runtime.pending draft round-trip
	var qSub core.TurnSubmission
	decodeResult(t, client.call(t, "turn.submit", map[string]any{
		"sessionId": session.ID, "submissionId": "q-turn", "input": "Ask me a question",
	}), &qSub)
	var qWait core.UserQuestionAnswer
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pending []core.PendingRequest
		decodeResult(t, client.call(t, "runtime.pending", map[string]any{"sessionId": session.ID}), &pending)
		if len(pending) == 1 && pending[0].UserQuestion != nil {
			uq := pending[0].UserQuestion
			qWait = core.UserQuestionAnswer{
				ExecutionID: uq.ExecutionID, RunID: uq.RunID, InvocationID: uq.InvocationID, CallID: uq.CallID,
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if qWait.CallID == "" {
		t.Fatal("user question did not become pending")
	}
	var draftSaved map[string]bool
	decodeResult(t, client.call(t, "question.saveDraft", map[string]any{
		"sessionId": session.ID,
		"wait":      qWait,
		"draft": core.UserQuestionDraft{
			CurrentQuestionID: "q1",
			Responses: []core.UserQuestionDraftResponse{
				{QuestionID: "q1", SelectedOption: 1, Notes: "prefer sqlite"},
			},
		},
	}), &draftSaved)
	if !draftSaved["saved"] {
		t.Fatal("question.saveDraft did not report saved=true")
	}
	var pendingWithDraft []core.PendingRequest
	decodeResult(t, client.call(t, "runtime.pending", map[string]any{"sessionId": session.ID}), &pendingWithDraft)
	if len(pendingWithDraft) != 1 || pendingWithDraft[0].UserQuestion == nil || pendingWithDraft[0].UserQuestion.Draft == nil ||
		pendingWithDraft[0].UserQuestion.Draft.CurrentQuestionID != "q1" {
		t.Fatalf("pending question draft = %#v", pendingWithDraft)
	}
	qWait.Answers = []core.UserQuestionResponse{{QuestionID: "q1", Answer: "SQLite"}}
	decodeResult(t, client.call(t, "question.resolve", map[string]any{
		"sessionId": session.ID, "answer": qWait,
	}), & map[string]bool{})
	decodeResult(t, client.call(t, "turn.wait", map[string]any{
		"sessionId": session.ID, "executionId": qSub.ExecutionID,
	}), &planRes)

	// 3. Test background command control (command.list, command.read, command.stop)
	var cmdSub core.TurnSubmission
	decodeResult(t, client.call(t, "turn.submit", map[string]any{
		"sessionId": session.ID, "submissionId": "cmd-turn", "input": "Run background daemon",
	}), &cmdSub)
	select {
	case <-cmdExec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("background command did not start")
	}
	decodeResult(t, client.call(t, "turn.wait", map[string]any{
		"sessionId": session.ID, "executionId": cmdSub.ExecutionID,
	}), &planRes)
	var commands []core.CommandInfo
	decodeResult(t, client.call(t, "command.list", map[string]any{"sessionId": session.ID}), &commands)
	if len(commands) != 1 || !commands[0].Background {
		t.Fatalf("command.list = %#v", commands)
	}
	var cmdOut core.CommandOutput
	decodeResult(t, client.call(t, "command.read", map[string]any{
		"sessionId": session.ID, "commandId": commands[0].ID, "cursor": 0, "waitMs": 100,
	}), &cmdOut)
	if !strings.Contains(cmdOut.Output, "daemon-ready") {
		t.Fatalf("command.read output = %q", cmdOut.Output)
	}
	decodeResult(t, client.call(t, "command.stop", map[string]any{
		"sessionId": session.ID, "commandId": commands[0].ID,
	}), &cmdOut)
	if cmdOut.Command.Status.Active() {
		t.Fatalf("command status after stop = %q", cmdOut.Command.Status)
	}

	// 4. Test Goal lifecycle (goal.create, goal.get, goal.pause, goal.edit, goal.setBudget, goal.clear)
	var goalSession sessionSummary
	decodeResult(t, client.call(t, "session.create", map[string]any{}), &goalSession)
	budget := int64(50000)
	var createdGoal core.Goal
	decodeResult(t, client.call(t, "goal.create", map[string]any{
		"sessionId": goalSession.ID, "objective": "Initial goal", "tokenBudget": budget,
	}), &createdGoal)
	if createdGoal.Objective != "Initial goal" || createdGoal.TokenBudget == nil || *createdGoal.TokenBudget != budget {
		t.Fatalf("goal.create = %#v", createdGoal)
	}
	var pausedGoal core.Goal
	decodeResult(t, client.call(t, "goal.pause", map[string]any{"sessionId": goalSession.ID}), &pausedGoal)
	if pausedGoal.Status != core.GoalPaused {
		t.Fatalf("goal.pause status = %q", pausedGoal.Status)
	}
	var editedGoal core.Goal
	decodeResult(t, client.call(t, "goal.edit", map[string]any{
		"sessionId": goalSession.ID, "objective": "Updated goal objective",
	}), &editedGoal)
	if editedGoal.Objective != "Updated goal objective" {
		t.Fatalf("goal.edit = %#v", editedGoal)
	}
	newBudget := int64(80000)
	decodeResult(t, client.call(t, "goal.setBudget", map[string]any{
		"sessionId": goalSession.ID, "tokenBudget": newBudget,
	}), &editedGoal)
	if editedGoal.TokenBudget == nil || *editedGoal.TokenBudget != newBudget {
		t.Fatalf("goal.setBudget = %#v", editedGoal.TokenBudget)
	}
	var gotGoal struct {
		Goal *core.Goal `json:"goal"`
	}
	decodeResult(t, client.call(t, "goal.get", map[string]any{"sessionId": goalSession.ID}), &gotGoal)
	if gotGoal.Goal == nil || gotGoal.Goal.Objective != "Updated goal objective" {
		t.Fatalf("goal.get = %#v", gotGoal)
	}
	var cleared map[string]bool
	decodeResult(t, client.call(t, "goal.clear", map[string]any{"sessionId": goalSession.ID}), &cleared)
	if !cleared["cleared"] {
		t.Fatal("goal.clear did not return cleared=true")
	}

	// 5. Test MCP inspection, prompts/resources, and subscription-gated user interaction
	var servers []core.MCPServerStatus
	decodeResult(t, client.call(t, "mcp.servers", map[string]any{}), &servers)
	if len(servers) != 1 || servers[0].Name != "docs" || servers[0].Enabled {
		t.Fatalf("mcp.servers = %#v", servers)
	}
	var enabledMap map[string]bool
	decodeResult(t, client.call(t, "mcp.enable", map[string]any{"serverName": "docs"}), &enabledMap)
	if !enabledMap["enabled"] {
		t.Fatal("mcp.enable did not enable docs server")
	}
	var prompts []core.MCPPrompt
	decodeResult(t, client.call(t, "mcp.listPrompts", map[string]any{"sessionId": session.ID, "serverName": "docs"}), &prompts)
	if len(prompts) != 1 || prompts[0].Name != "review" {
		t.Fatalf("mcp.listPrompts = %#v", prompts)
	}
	var resources []core.MCPResource
	decodeResult(t, client.call(t, "mcp.listResources", map[string]any{"sessionId": session.ID, "serverName": "docs"}), &resources)
	if len(resources) != 1 || resources[0].URI != "urn:doc:1" {
		t.Fatalf("mcp.listResources = %#v", resources)
	}
	var templates []core.MCPResourceTemplate
	decodeResult(t, client.call(t, "mcp.listResourceTemplates", map[string]any{"sessionId": session.ID, "serverName": "docs"}), &templates)
	if len(templates) != 1 || templates[0].URITemplate != "urn:doc:{id}" {
		t.Fatalf("mcp.listResourceTemplates = %#v", templates)
	}
	var resourceInput json.RawMessage
	decodeResult(t, client.call(t, "mcp.readResource", map[string]any{
		"sessionId": session.ID, "serverName": "docs", "uri": "urn:doc:1",
	}), &resourceInput)
	if !strings.Contains(string(resourceInput), "Resource content") {
		t.Fatalf("mcp.readResource = %s", resourceInput)
	}

	// Without an active subscription, MCP elicitation during mcp.getPrompt must fail with user_interaction_unsupported.
	unsubscribedPrompt := client.call(t, "mcp.getPrompt", map[string]any{
		"sessionId": session.ID, "serverName": "docs", "promptName": "review",
	})
	if unsubscribedPrompt.Error == nil || !strings.Contains(string(unsubscribedPrompt.Error.Data), "user_interaction_unsupported") {
		t.Fatalf("unsubscribed mcp.getPrompt error = %#v", unsubscribedPrompt.Error)
	}

	// Subscribe to the session so SetUserInteractionAvailable(true) is activated automatically.
	decodeResult(t, client.call(t, "runtime.subscribe", map[string]any{
		"sessionId": session.ID, "subscriptionId": "mcp-sub", "afterCursor": 0,
	}), &map[string]string{})

	secondClient := dialTestRPC(t, server.URL, "ext-token", server.Certificate())
	t.Cleanup(secondClient.close)
	promptReplyCh := make(chan testRPCReply, 1)
	go func() {
		promptReplyCh <- client.call(t, "mcp.getPrompt", map[string]any{
			"sessionId": session.ID, "serverName": "docs", "promptName": "review",
		})
	}()

	var interactionID string
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pending []core.PendingRequest
		decodeResult(t, secondClient.call(t, "runtime.pending", map[string]any{"sessionId": session.ID}), &pending)
		for _, item := range pending {
			if item.Kind == core.PendingUserInteraction && item.Interaction != nil {
				interactionID = item.Interaction.ID
				break
			}
		}
		if interactionID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if interactionID == "" {
		t.Fatal("MCP user interaction did not appear in runtime.pending")
	}
	decodeResult(t, secondClient.call(t, "interaction.resolve", map[string]any{
		"sessionId": session.ID,
		"id":        interactionID,
		"response":  map[string]any{"action": "accept", "content": map[string]any{"focus": "security"}},
	}), &map[string]bool{})

	promptReply := <-promptReplyCh
	var promptResult core.MCPPromptResult
	decodeResult(t, promptReply, &promptResult)
	if promptResult.Description != "Reviewed prompt" {
		t.Fatalf("mcp.getPrompt result = %#v", promptResult)
	}

	// Unsubscribing disables MCP user interaction for the session again.
	decodeResult(t, client.call(t, "runtime.unsubscribe", map[string]any{"subscriptionId": "mcp-sub"}), &map[string]bool{})
	afterUnsubPrompt := client.call(t, "mcp.getPrompt", map[string]any{
		"sessionId": session.ID, "serverName": "docs", "promptName": "review",
	})
	if afterUnsubPrompt.Error == nil || !strings.Contains(string(afterUnsubPrompt.Error.Data), "user_interaction_unsupported") {
		t.Fatalf("mcp.getPrompt after unsubscribe error = %#v", afterUnsubPrompt.Error)
	}
	decodeResult(t, client.call(t, "mcp.disable", map[string]any{"serverName": "docs"}), &enabledMap)
	if enabledMap["enabled"] {
		t.Fatal("mcp.disable did not report enabled=false")
	}
}
