package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	core "github.com/afterlune/luneCore"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/memory"
	publicmodel "github.com/afterlune/luneCore/model"
)

type benchmarkCase struct {
	ID                  string              `json:"id"`
	Input               string              `json:"input"`
	History             json.RawMessage     `json:"history,omitempty"`
	Files               map[string]string   `json:"files,omitempty"`
	Memories            []memory.Input      `json:"memories,omitempty"`
	ResponseMustContain []string            `json:"responseMustContain,omitempty"`
	FilesMustContain    map[string][]string `json:"filesMustContain,omitempty"`
}

func runBenchmark(configPath, casesPath, profile, outputPath string, repeat int) error {
	project, err := projectconfig.Load(configPath)
	if err != nil {
		return err
	}
	modelConfig := project.ModelConfig()
	if profile == "" {
		profile = modelConfig.DefaultModel
	}
	if _, ok := modelConfig.Models[profile]; !ok {
		return fmt.Errorf("model profile %q is not configured", profile)
	}
	casesData, err := os.ReadFile(casesPath)
	if err != nil {
		return fmt.Errorf("read benchmark cases: %w", err)
	}
	var cases []benchmarkCase
	if err := json.Unmarshal(casesData, &cases); err != nil {
		return fmt.Errorf("decode benchmark cases: %w", err)
	}
	if len(cases) == 0 {
		return errors.New("benchmark case list is empty")
	}
	if err := validateCases(cases); err != nil {
		return err
	}
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	modelProfile := modelConfig.Models[profile]
	protocol := modelConfig.Providers[modelProfile.Provider].Protocol
	report := benchmarkReport{
		Version: 1, Profile: profile, Provider: modelProfile.Provider, Protocol: protocol, Model: modelProfile.Model,
		CreatedAt: time.Now().UTC(),
	}
	for run := 1; run <= repeat; run++ {
		for _, testCase := range cases {
			result, err := runCase(configData, modelProfile, profile, protocol, testCase)
			if err != nil {
				return fmt.Errorf("case %s run %d: %w", testCase.ID, run, err)
			}
			result.Repeat = run
			report.Cases = append(report.Cases, result)
		}
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode benchmark report: %w", err)
	}
	if err := os.WriteFile(outputPath, append(encoded, '\n'), 0600); err != nil {
		return fmt.Errorf("write benchmark report: %w", err)
	}
	fmt.Printf("wrote %d runs across %d cases for %s (%s) to %s\n", len(report.Cases), len(cases), profile, modelProfile.Model, outputPath)
	return nil
}

func validateCases(cases []benchmarkCase) error {
	seen := make(map[string]struct{}, len(cases))
	for _, testCase := range cases {
		if strings.TrimSpace(testCase.ID) == "" || strings.TrimSpace(testCase.Input) == "" {
			return errors.New("every benchmark case needs an id and input")
		}
		if _, exists := seen[testCase.ID]; exists {
			return fmt.Errorf("duplicate benchmark case id %q", testCase.ID)
		}
		seen[testCase.ID] = struct{}{}
		if len(testCase.History) > 0 && !json.Valid(testCase.History) {
			return fmt.Errorf("case %q has invalid history JSON", testCase.ID)
		}
		for path := range testCase.Files {
			if _, err := safeRelativePath(path); err != nil {
				return fmt.Errorf("case %q file fixture: %w", testCase.ID, err)
			}
		}
		for path := range testCase.FilesMustContain {
			if _, err := safeRelativePath(path); err != nil {
				return fmt.Errorf("case %q file check: %w", testCase.ID, err)
			}
		}
	}
	return nil
}

func runCase(configData []byte, profileConfig publicmodel.ModelProfile, profile, protocol string, testCase benchmarkCase) (caseReport, error) {
	workspace, err := os.MkdirTemp("", "lune-tokenbench-workspace-")
	if err != nil {
		return caseReport{}, err
	}
	defer os.RemoveAll(workspace)
	stateDir, err := os.MkdirTemp("", "lune-tokenbench-state-")
	if err != nil {
		return caseReport{}, err
	}
	defer os.RemoveAll(stateDir)
	if err := os.WriteFile(filepath.Join(workspace, projectconfig.FileName), configData, 0600); err != nil {
		return caseReport{}, err
	}
	for name, content := range testCase.Files {
		path, err := safeRelativePath(name)
		if err != nil {
			return caseReport{}, err
		}
		fullPath := filepath.Join(workspace, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
			return caseReport{}, err
		}
		if err := os.WriteFile(fullPath, []byte(content), 0600); err != nil {
			return caseReport{}, err
		}
	}
	client, err := core.New(core.Config{Workspace: workspace, StateDir: stateDir})
	if err != nil {
		return caseReport{}, err
	}
	defer client.Close()
	if len(testCase.Memories) > 0 {
		for _, item := range testCase.Memories {
			if _, err := client.AddMemory(context.Background(), item); err != nil {
				return caseReport{}, fmt.Errorf("seed memory: %w", err)
			}
		}
	}
	history := testCase.History
	if len(history) == 0 {
		history = json.RawMessage("null")
	}
	session, err := client.CreateSession(context.Background(), history)
	if err != nil {
		return caseReport{}, err
	}
	if err := client.SetSessionSettings(context.Background(), session.ID, core.SessionSettings{ModelProfile: profile}); err != nil {
		return caseReport{}, err
	}
	input, err := core.NewTaskInput(testCase.Input)
	if err != nil {
		return caseReport{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	started := time.Now()
	result, runErr := client.RunTask(ctx, core.TaskRequest{
		SessionID: session.ID, Input: input,
		Execution: core.ExecutionOptions{MaxSteps: 20},
	})
	duration := time.Since(started)
	saved, getErr := client.GetSession(context.Background(), session.ID)
	if getErr != nil {
		return caseReport{}, getErr
	}
	checks := evaluateCase(workspace, testCase, finalText(result.Response))
	caseResult := caseReport{
		ID: testCase.ID, Status: string(result.Status), DurationMillis: duration.Milliseconds(),
		FinalText: finalText(result.Response), Usage: saved.TaskUsage, Protocol: protocol, Checks: checks,
		AcceptanceOutcome: acceptanceOutcome(result.AcceptanceReports),
	}
	recordedRequests := 0
	measuredRequests := 0
	totalTokens := int64(0)
	for _, request := range saved.UsageRequests {
		if request.Request > 0 {
			recordedRequests++
			purpose := requestPurpose(request)
			caseResult.PurposeUsage.add(purpose, request.Usage, request.Available)
			if request.Available {
				measuredRequests++
				requestTotal := request.Usage.TotalTokens
				if requestTotal <= 0 {
					requestTotal = request.Usage.InputTokens + request.Usage.OutputTokens
				}
				if protocol == publicmodel.ProtocolAnthropicMessages && purpose == "" {
					requestTotal += request.Usage.InputTokensDetails.CachedTokens + request.Usage.InputTokensDetails.CacheCreationTokens
				}
				totalTokens += requestTotal
			}
		}
	}
	caseResult.Measured = saved.TaskUsage.Requests > 0 && recordedRequests == saved.TaskUsage.Requests && measuredRequests == saved.TaskUsage.Requests
	caseResult.RequestCount = saved.TaskUsage.Requests
	caseResult.EstimatedInput = saved.TaskUsage.EstimatedInput
	caseResult.Usage.Total = totalTokens
	caseResult.Provider = profileConfig.Provider
	caseResult.Model = profileConfig.Model
	if runErr != nil {
		caseResult.ErrorType = fmt.Sprintf("%T", runErr)
	}
	return caseResult, nil
}

func requestPurpose(request core.UsageRecord) string {
	data, err := json.Marshal(request)
	if err != nil {
		return ""
	}
	var metadata struct {
		Purpose string `json:"purpose"`
	}
	if json.Unmarshal(data, &metadata) != nil {
		return ""
	}
	return metadata.Purpose
}

func safeRelativePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" || filepath.IsAbs(value) || filepath.VolumeName(value) != "" {
		return "", fmt.Errorf("path %q must be relative", value)
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the temporary workspace", value)
	}
	return clean, nil
}

func evaluateCase(workspace string, testCase benchmarkCase, response string) []checkResult {
	checks := make([]checkResult, 0, len(testCase.ResponseMustContain)+len(testCase.FilesMustContain))
	for _, expected := range testCase.ResponseMustContain {
		checks = append(checks, checkResult{Description: "response contains " + expected, Passed: strings.Contains(response, expected)})
	}
	for name, expectedValues := range testCase.FilesMustContain {
		path, err := safeRelativePath(name)
		if err != nil {
			checks = append(checks, checkResult{Description: "read expected file " + name, Passed: false})
			continue
		}
		content, err := os.ReadFile(filepath.Join(workspace, path))
		for _, expected := range expectedValues {
			checks = append(checks, checkResult{Description: "file " + name + " contains " + expected, Passed: err == nil && strings.Contains(string(content), expected)})
		}
	}
	return checks
}

func finalText(raw json.RawMessage) string {
	var response publicmodel.Response
	if json.Unmarshal(raw, &response) != nil {
		return ""
	}
	var builder strings.Builder
	for _, item := range response.Output {
		message, ok := item.(publicmodel.OutputMessage)
		if !ok {
			continue
		}
		for _, part := range message.Content {
			if output, ok := part.(publicmodel.OutputText); ok {
				builder.WriteString(output.Text)
			}
		}
	}
	return builder.String()
}

func acceptanceOutcome(reports []core.AcceptanceReport) string {
	if len(reports) == 0 {
		return ""
	}
	latest := reports[len(reports)-1]
	for _, result := range latest.Results {
		if result.Outcome != core.AcceptancePassed {
			return string(result.Outcome)
		}
	}
	return "passed"
}
