package agentgraph

import "testing"

func TestQuestionBatchRequiresUniqueIDsAndNonemptyContent(t *testing.T) {
	for _, input := range []string{
		`{"questions":[]}`,
		`{"questions":[{"id":"x","question":"Q","options":[]},{"id":"x","question":"Q2","options":[]}]}`,
		`{"questions":[{"id":"x","question":" ","options":[]}]}`,
		`{"questions":[{"id":"x","question":"Q","options":[{"label":" ","description":""}]}]}`,
		`{"questions":[{"id":"x","question":"Q","options":[{"label":"A","description":"","recommended":true},{"label":"B","description":"","recommended":true}]}]}`,
		`{"question":"legacy","options":[]}`,
	} {
		if _, err := parseUserQuestion(input); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	got, err := parseUserQuestion(`{"questions":[{"id":"one","question":"First?","options":[]},{"id":"two","question":"Second?","options":[{"label":"Yes","description":"Proceed","recommended":true}]}]}`)
	if err != nil || len(got) != 2 || got[1].ID != "two" || !got[1].Options[0].Recommended {
		t.Fatalf("batch=%#v, %v", got, err)
	}
	withoutRecommendation, err := parseUserQuestion(`{"questions":[{"id":"q","question":"Q","options":[{"label":"A","description":""},{"label":"B","description":""}]}]}`)
	if err != nil || withoutRecommendation[0].Options[0].Recommended || withoutRecommendation[0].Options[1].Recommended {
		t.Fatalf("question without recommendation was not accepted: %#v, %v", withoutRecommendation, err)
	}
}
