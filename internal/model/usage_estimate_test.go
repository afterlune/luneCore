package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEstimateInputTokensCountsTextInstructionsAndToolSchemas(t *testing.T) {
	base := ModelRequest{
		Instructions: "Follow the project conventions.",
		Input: NewResponseInputItems(InputMessage{
			Role: RoleUser, Content: NewMessageText("Review the configuration flow."),
		}),
	}
	withoutTools := EstimateInputTokens(base)
	withTools := base
	withTools.Tools = []ResponseTool{FunctionTool{
		Name: "read_file", Description: "Read a project file.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}}
	if withoutTools == 0 || EstimateInputTokens(withTools) <= withoutTools {
		t.Fatalf("estimated tokens without/with tool schema = %d/%d", withoutTools, EstimateInputTokens(withTools))
	}
	chinese := ModelRequest{Input: NewResponseInputItems(InputMessage{
		Role: RoleUser, Content: NewMessageText("检查项目配置"),
	})}
	if got := EstimateInputTokens(chinese); got < 4 {
		t.Fatalf("Chinese text estimate = %d, want at least one token per Han character", got)
	}
}

func TestEstimateInputTokensExcludesImagesAndUnknownMultimodalItems(t *testing.T) {
	makeRequest := func(imagePayload string) ModelRequest {
		return ModelRequest{Input: NewResponseInputItems(
			InputMessage{Role: RoleUser, Content: NewMessageContentParts(
				InputText{Text: "What is in this picture?"},
				InputImage{ImageURL: "data:image/png;base64," + imagePayload},
			)},
			UnknownInputItem{Raw: json.RawMessage(`{"type":"input_audio","data":"` + strings.Repeat("a", len(imagePayload)) + `"}`)},
		)}
	}
	short := EstimateInputTokens(makeRequest("YQ=="))
	long := EstimateInputTokens(makeRequest(strings.Repeat("A", 1<<20)))
	if short == 0 || short != long {
		t.Fatalf("image payload affected text estimate: short=%d long=%d", short, long)
	}
}

func TestEstimateInputTokensIncludesFunctionCallOutputText(t *testing.T) {
	base := ModelRequest{Input: NewResponseInputItems(
		InputMessage{Role: RoleUser, Content: NewMessageText("Inspect this.")},
	)}
	withOutput := ModelRequest{Input: NewResponseInputItems(
		InputMessage{Role: RoleUser, Content: NewMessageText("Inspect this.")},
		InputFunctionCallOutput{CallID: "call-1", Output: NewToolOutputText(strings.Repeat("workspace result ", 40))},
	)}
	if EstimateInputTokens(withOutput) <= EstimateInputTokens(base) {
		t.Fatalf("tool output did not increase the estimate: base=%d with output=%d", EstimateInputTokens(base), EstimateInputTokens(withOutput))
	}
}
