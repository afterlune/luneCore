package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
)

func TestRunBenchmarkUsesMeasuredCoreUsageWithoutExternalProvider(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request publicmodel.ModelRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var names []string
		for _, definition := range request.Tools {
			if function, ok := definition.(publicmodel.FunctionTool); ok {
				names = append(names, function.Name)
			}
		}
		var response *publicmodel.ModelResponse
		switch {
		case containsName(names, "propose_workspace_memories"):
			response = tokenbenchToolResponse("propose_workspace_memories", `{"items":[]}`)
		default:
			response = &publicmodel.ModelResponse{Status: "completed", Output: []publicmodel.ResponseOutputItem{publicmodel.OutputMessage{
				Role: publicmodel.RoleAssistant, Content: []publicmodel.OutputContentPart{publicmodel.OutputText{Text: "4"}},
			}}}
		}
		response.Status = "completed"
		response.Usage = publicmodel.ResponseUsage{InputTokens: 100, OutputTokens: 2, TotalTokens: 102}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(response); err != nil {
				t.Errorf("encode response: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		data, err := json.Marshal(struct {
			Type     string                     `json:"type"`
			Response *publicmodel.ModelResponse `json:"response"`
		}{Type: "response.completed", Response: response})
		if err != nil {
			t.Errorf("encode response: %v", err)
			return
		}
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", data)
	}))
	defer server.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	config := fmt.Sprintf(`{"providers":{"host":{"protocol":"responses","baseURL":%q,"apiKey":"test"}},"models":{"primary":{"provider":"host","model":"test-model"}},"defaultModel":"primary"}`, server.URL)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	casesPath := filepath.Join(dir, "cases.json")
	if err := os.WriteFile(casesPath, []byte(`[{"id":"answer","input":"What is two plus two?","responseMustContain":["4"]}]`), 0600); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dir, "report.json")
	if err := runBenchmark(configPath, casesPath, "primary", reportPath, 1); err != nil {
		t.Fatalf("runBenchmark: %v", err)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report benchmarkReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Cases) != 1 || report.Cases[0].Status != "completed" || !report.Cases[0].Measured || report.Cases[0].Usage.Total != 204 {
		t.Fatalf("benchmark report = %#v", report)
	}
	if report.Cases[0].RequestCount != 2 || requests != 2 || !report.Cases[0].Checks[0].Passed {
		t.Fatalf("request count=%d provider requests=%d checks=%#v", report.Cases[0].RequestCount, requests, report.Cases[0].Checks)
	}
	if report.Cases[0].PurposeUsage["memory"].Requests != 1 {
		t.Fatalf("memory extraction usage = %#v", report.Cases[0].PurposeUsage["memory"])
	}
	if !strings.Contains(report.Cases[0].FinalText, "4") {
		t.Fatalf("final response = %q", report.Cases[0].FinalText)
	}
}

func containsName(names []string, expected string) bool {
	for _, name := range names {
		if name == expected {
			return true
		}
	}
	return false
}

func tokenbenchToolResponse(name, arguments string) *publicmodel.ModelResponse {
	return &publicmodel.ModelResponse{Output: []publicmodel.ResponseOutputItem{publicmodel.OutputFunctionCall{
		CallID: "tokenbench-" + name, Name: name, Arguments: arguments,
	}}}
}
