package agentgraph

import "testing"

func TestQuestionBatchRequiresUniqueIDsAndNonemptyContent(t *testing.T) {
	for _, input := range []string{
		`{"questions":[]}`,
		`{"questions":[{"id":"x","question":"Q","options":[]},{"id":"x","question":"Q2","options":[]}]}`,
		`{"questions":[{"id":"x","question":" ","options":[]}]}`,
		`{"questions":[{"id":"x","question":"Q","options":[{"label":" ","description":""}]}]}`,
		`{"question":"legacy","options":[]}`,
	} {
		if _, err := parseUserQuestion(input); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	got, err := parseUserQuestion(`{"questions":[{"id":"one","question":"First?","options":[]},{"id":"two","question":"Second?","options":[{"label":"Yes","description":"Proceed"}]}]}`)
	if err != nil || len(got) != 2 || got[1].ID != "two" {
		t.Fatalf("batch=%#v, %v", got, err)
	}
}
