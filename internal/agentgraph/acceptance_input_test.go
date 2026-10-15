package agentgraph

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/acceptance"
	"github.com/afterlune/luneCore/internal/model"
)

func TestAcceptancePhaseInputUsesOnlyTheLatestPriorExchange(t *testing.T) {
	current := model.NewResponseInputText("再")
	currentItems, err := responseInputItemsOrEmpty(current)
	if err != nil {
		t.Fatal(err)
	}
	taskInput, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	transcript := model.NewResponseInputItems(
		userText("old request"),
		assistantText("old response"),
		userText("Make a second banner with orange colors."),
		assistantText("The first banner version is ready."),
		currentItems[0],
	)

	input, err := acceptancePhaseInput(State{TaskInput: taskInput, Transcript: *transcript})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, expected := range []string{
		"Make a second banner with orange colors.",
		"The first banner version is ready.",
		"Current user request",
		"再",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("acceptance input is missing %q: %s", expected, text)
		}
	}
	if strings.Contains(text, "old request") || strings.Contains(text, "old response") {
		t.Fatalf("acceptance input included an older exchange: %s", text)
	}
}

func TestAcceptancePhaseInputDoesNotUseUnmatchedTranscriptTail(t *testing.T) {
	current := model.NewResponseInputText("continue")
	taskInput, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	transcript := model.NewResponseInputItems(userText("unrelated conversation"))

	input, err := acceptancePhaseInput(State{TaskInput: taskInput, Transcript: *transcript})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "unrelated conversation") {
		t.Fatalf("unmatched transcript context was used: %s", encoded)
	}
}

func TestAcceptanceCriteriaResolveShortFollowupsBeforeAsking(t *testing.T) {
	instructions := acceptanceCriteriaInstructions(State{})
	for _, expected := range []string{
		"preceding user-assistant exchange",
		"'再'",
		"Do not ask only because the current message is short",
		"materially change scope",
		"exact authorizing words",
	} {
		if !strings.Contains(instructions, expected) {
			t.Errorf("acceptance instructions are missing %q: %s", expected, instructions)
		}
	}
}

func TestFollowupCriteriaBasisMustStillComeFromCurrentTurn(t *testing.T) {
	currentInput, err := json.Marshal(model.NewResponseInputText("再"))
	if err != nil {
		t.Fatal(err)
	}
	state := State{TaskInput: currentInput}
	criteria := criteriaSubmission{
		Kind: acceptance.TaskAnswer,
		Criteria: []acceptance.Criterion{{
			ID: "another_version", Description: "Produce another version of the referenced banner.", Basis: "再",
		}},
	}
	if err := validateCriteria(state, criteria); err != nil {
		t.Fatalf("current follow-up basis was rejected: %v", err)
	}
	criteria.Criteria[0].Basis = "Create a second banner with orange colors."
	if err := validateCriteria(state, criteria); err == nil {
		t.Fatal("prior context was accepted as the current turn's authorization basis")
	}
}

func userText(text string) model.InputItem {
	return model.InputMessage{Role: model.RoleUser, Content: model.NewMessageText(text)}
}

func assistantText(text string) model.InputItem {
	return model.InputMessage{Role: model.RoleAssistant, Content: model.NewMessageText(text)}
}
