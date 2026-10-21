package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
)

// Explicitly opt in with a private config path. Normal test runs never call an API.
func TestMemoryRealAPISmoke(t *testing.T) {
	configPath := os.Getenv("LUNE_MEMORY_SMOKE_CONFIG")
	if configPath == "" {
		t.Skip("real API test is opt-in")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("read private model config failed")
	}
	var project projectconfig.ProjectConfig
	if err := json.Unmarshal(data, &project); err != nil {
		t.Fatal("decode private model config failed")
	}
	profileID := project.Models.DefaultProfile
	profile, ok := project.Models.Profiles[profileID]
	if !ok {
		t.Fatal("default model profile is missing")
	}
	profile.MaxOutputTokens = 512
	project.Models.Profiles = map[string]model.ModelProfile{profileID: profile}
	transport := http.DefaultTransport
	probe := &memorySmokeTransport{inner: transport, t: t}
	http.DefaultTransport = probe
	t.Cleanup(func() { http.DefaultTransport = transport })

	base := t.TempDir()
	marker := "LUNE-PROBE-" + rand.Text()
	if err := os.WriteFile(filepath.Join(base, "probe.txt"), []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	client, err := New(Config{
		Workspace: base, StateDir: filepath.Join(base, "state"), ModelConfig: &project.Models,
		Logger:    slog.New(slog.NewTextHandler(&logs, nil)),
		Execution: ExecutionOptions{ModelMaxAttempts: 1, TokenBudget: 10000, MaxSteps: 12, MaxInvocations: 12},
	})
	if err != nil {
		t.Fatal("create isolated Core failed")
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.SetPermissionMode(PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	saved, err := client.CreateSession(ctx, json.RawMessage("null"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := NewTaskInput("请使用 read_file 读取 probe.txt，只回复其中的标记。本工作区长期约定：回复使用简洁中文。只调用一次 read_file，不委派，也不调用其他工具。")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := client.RunTask(ctx, TaskRequest{SessionID: saved.ID, Input: input})
	t.Logf("MEMORY_SMOKE duration=%s http_requests=%d task_status=%s", time.Since(started).Round(time.Millisecond), probe.requests, result.Status)
	if err != nil || result.Status != TaskCompleted {
		t.Fatal("main task did not complete")
	}
	encoded, err := json.Marshal(result.Response)
	if err != nil || !bytes.Contains(encoded, []byte(marker)) {
		t.Fatal("response did not contain the file-only marker")
	}
	events, err := client.sessions.RuntimeEventsAfter(ctx, saved.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	status := ""
	for _, stored := range events {
		var e Event
		if err := json.Unmarshal(stored.Payload, &e); err != nil {
			t.Fatal(err)
		}
		if e.MemoryStatus != "" {
			status = e.MemoryStatus
			t.Logf("MEMORY_SMOKE status=%s diagnostic=%q failure=%+v", status, e.Err, e.ModelFailure)
		}
	}
	final, err := client.GetSession(ctx, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range final.UsageRequests {
		t.Logf("MEMORY_SMOKE usage purpose=%s agent=%s input=%d cached=%d output=%d", request.Purpose, request.AgentID,
			request.Usage.InputTokens, request.Usage.InputTokensDetails.CachedTokens, request.Usage.OutputTokens)
	}
	if status != "saved" {
		t.Fatalf("expected explicit workspace preference to be saved; memory status=%s", status)
	}
	items, err := client.ListMemories(ctx, 10)
	if err != nil || len(items) == 0 {
		t.Fatal("saved memory was not retrievable")
	}
}

// HTTP diagnostics retain only structured identifiers and allowlisted field names.
// Response bodies, provider messages and credentials are never logged.
type memorySmokeTransport struct {
	inner    http.RoundTripper
	t        *testing.T
	requests int
}

func (p *memorySmokeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	p.requests++
	if p.requests > 3 {
		return nil, fmt.Errorf("memory smoke HTTP request budget reached")
	}
	response, err := p.inner.RoundTrip(request)
	if err != nil || response.StatusCode < 400 {
		return response, err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return response, err
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	var fields []string
	for _, field := range []string{"tool_choice", "strict", "stream", "max_output_tokens", "reasoning", "schema", "tools", "required"} {
		if strings.Contains(strings.ToLower(payload.Error.Message+" "+payload.Error.Param), field) {
			fields = append(fields, field)
		}
	}
	p.t.Logf("MEMORY_SMOKE http_status=%d code=%q type=%q mentioned_fields=%v", response.StatusCode,
		memoryFailureIdentifier(payload.Error.Code), memoryFailureIdentifier(payload.Error.Type), fields)
	return response, nil
}
