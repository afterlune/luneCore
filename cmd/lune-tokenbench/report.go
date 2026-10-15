package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	core "github.com/afterlune/luneCore"
)

type benchmarkReport struct {
	Version   int          `json:"version"`
	Profile   string       `json:"profile"`
	Provider  string       `json:"provider"`
	Protocol  string       `json:"protocol"`
	Model     string       `json:"model"`
	CreatedAt time.Time    `json:"createdAt"`
	Cases     []caseReport `json:"cases"`
}

type caseReport struct {
	ID             string           `json:"id"`
	Repeat         int              `json:"repeat"`
	Provider       string           `json:"provider"`
	Protocol       string           `json:"protocol"`
	Model          string           `json:"model"`
	Status         string           `json:"status"`
	ErrorType      string           `json:"errorType,omitempty"`
	DurationMillis int64            `json:"durationMillis"`
	RequestCount   int              `json:"requestCount"`
	Measured       bool             `json:"measured"`
	EstimatedInput int64            `json:"estimatedInput,omitempty"`
	Usage          core.UsageTotals `json:"usage"`
	PurposeUsage   purposeUsage     `json:"purposeUsage,omitempty"`
	FinalText      string           `json:"finalText,omitempty"`
	Checks         []checkResult    `json:"checks,omitempty"`
}

type checkResult struct {
	Description string `json:"description"`
	Passed      bool   `json:"passed"`
}

type purposeUsage map[string]core.UsageTotals

func (usage *purposeUsage) add(purpose string, value core.Usage, available bool) {
	if *usage == nil {
		*usage = make(map[string]core.UsageTotals)
	}
	key := purpose
	totals := (*usage)[key]
	totals.Requests++
	if available {
		totals.Measured++
		totals.Input += value.InputTokens
		totals.Output += value.OutputTokens
		totals.Total += value.TotalTokens
		totals.Cached += value.InputTokensDetails.CachedTokens
		totals.CacheCreation += value.InputTokensDetails.CacheCreationTokens
	}
	(*usage)[key] = totals
}

func compareReports(baselinePath, candidatePath string, output io.Writer) error {
	baseline, err := readReport(baselinePath)
	if err != nil {
		return err
	}
	candidate, err := readReport(candidatePath)
	if err != nil {
		return err
	}
	if baseline.Version != 1 || candidate.Version != 1 {
		return errors.New("unsupported tokenbench report version")
	}
	if baseline.Profile != candidate.Profile || baseline.Provider != candidate.Provider || baseline.Protocol != candidate.Protocol || baseline.Model != candidate.Model {
		return errors.New("baseline and candidate must use the same profile, provider, and model")
	}
	baseCases, err := indexCases(baseline.Cases)
	if err != nil {
		return err
	}
	candidateCases, err := indexCases(candidate.Cases)
	if err != nil {
		return err
	}
	if len(baseCases) != len(candidateCases) {
		return errors.New("baseline and candidate must contain the same number of case runs")
	}
	baseTokens, candidateTokens := int64(0), int64(0)
	baseLatency, candidateLatency := make([]int64, 0, len(baseCases)), make([]int64, 0, len(baseCases))
	for key, before := range baseCases {
		after, ok := candidateCases[key]
		if !ok {
			return fmt.Errorf("candidate report is missing case run %s", key)
		}
		if !before.Measured || !after.Measured {
			return fmt.Errorf("case run %s lacks measured usage; estimated tokens cannot be compared", key)
		}
		if before.RequestCount != after.RequestCount && after.RequestCount > before.RequestCount {
			return fmt.Errorf("case run %s increased model requests from %d to %d", key, before.RequestCount, after.RequestCount)
		}
		if before.Status != after.Status {
			return fmt.Errorf("case run %s changed task status (%s -> %s)", key, before.Status, after.Status)
		}
		if failedCheck(before.Checks) {
			return fmt.Errorf("baseline case run %s failed an objective check and cannot establish a quality baseline", key)
		}
		if failedCheck(after.Checks) {
			return fmt.Errorf("case run %s failed an objective candidate check", key)
		}
		baseTokens += before.Usage.Total
		candidateTokens += after.Usage.Total
		baseLatency = append(baseLatency, before.DurationMillis)
		candidateLatency = append(candidateLatency, after.DurationMillis)
	}
	baseP95, candidateP95 := percentile95(baseLatency), percentile95(candidateLatency)
	fmt.Fprintf(output, "profile: %s (%s / %s)\n", baseline.Profile, baseline.Provider, baseline.Model)
	fmt.Fprintf(output, "total tokens: %d -> %d\n", baseTokens, candidateTokens)
	fmt.Fprintf(output, "task p95: %d ms -> %d ms\n", baseP95, candidateP95)
	fmt.Fprintf(output, "runs compared: %d\n", len(baseCases))
	if candidateTokens >= baseTokens {
		return errors.New("candidate did not reduce measured total tokens")
	}
	if candidateP95 > baseP95 {
		return errors.New("candidate p95 task latency regressed")
	}
	fmt.Fprintln(output, "automated comparison passed; review finalText fields side by side before accepting the quality result")
	return nil
}

func readReport(path string) (benchmarkReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return benchmarkReport{}, err
	}
	var report benchmarkReport
	if err := json.Unmarshal(data, &report); err != nil {
		return benchmarkReport{}, fmt.Errorf("decode report %s: %w", path, err)
	}
	return report, nil
}

func indexCases(cases []caseReport) (map[string]caseReport, error) {
	result := make(map[string]caseReport, len(cases))
	for _, item := range cases {
		key := fmt.Sprintf("%s#%d", item.ID, item.Repeat)
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate case run %s", key)
		}
		result[key] = item
	}
	return result, nil
}

func failedCheck(checks []checkResult) bool {
	for _, check := range checks {
		if !check.Passed {
			return true
		}
	}
	return false
}

func percentile95(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(float64(len(sorted))*0.95)) - 1
	return sorted[index]
}
