package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/afterlune/luneCore"
)

func TestCompareReportsRequiresLowerTokensNoLatencyOrRequestRegression(t *testing.T) {
	dir := t.TempDir()
	baselinePath := filepath.Join(dir, "baseline.json")
	candidatePath := filepath.Join(dir, "candidate.json")
	baseline := benchmarkReport{Version: 1, Profile: "test", Provider: "provider", Model: "model", CreatedAt: time.Now(), Cases: []caseReport{{
		ID: "answer", Repeat: 1, Status: "completed", RequestCount: 2, Measured: true, DurationMillis: 100,
		Usage: core.UsageTotals{Requests: 2, Measured: 2, Total: 100}, Checks: []checkResult{{Description: "answer", Passed: true}},
	}}}
	candidate := baseline
	candidate.Cases = append([]caseReport(nil), baseline.Cases...)
	candidate.Cases[0].Usage.Total = 80
	candidate.Cases[0].DurationMillis = 90
	for path, report := range map[string]benchmarkReport{baselinePath: baseline, candidatePath: candidate} {
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := compareReports(baselinePath, candidatePath, &output); err != nil {
		t.Fatalf("compareReports: %v", err)
	}
	if !strings.Contains(output.String(), "total tokens: 100 -> 80") {
		t.Fatalf("comparison output = %q", output.String())
	}
	candidate.Cases[0].RequestCount = 3
	data, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidatePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := compareReports(baselinePath, candidatePath, &output); err == nil || !strings.Contains(err.Error(), "increased model requests") {
		t.Fatalf("compareReports error = %v, want request-count regression", err)
	}
}

func TestSafeRelativePathRejectsTraversal(t *testing.T) {
	for _, path := range []string{"../outside", "nested/../../outside", filepath.VolumeName(os.TempDir())} {
		if path == "" {
			continue
		}
		if _, err := safeRelativePath(path); err == nil {
			t.Errorf("safeRelativePath(%q) succeeded", path)
		}
	}
	if path, err := safeRelativePath(filepath.Join("src", "main.go")); err != nil || path == "" {
		t.Fatalf("safeRelativePath valid path = %q, %v", path, err)
	}
}
