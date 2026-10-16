package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// UserQuestionDraft stores unsubmitted answers without resuming the model.
type UserQuestionDraft struct {
	CurrentQuestionID string                      `json:"current_question_id"`
	Responses         []UserQuestionDraftResponse `json:"responses"`
}

type UserQuestionDraftResponse struct {
	QuestionID     string `json:"question_id"`
	SelectedOption int    `json:"selected_option"`
	Notes          string `json:"notes"`
	CustomAnswer   string `json:"custom_answer"`
	Confirmed      bool   `json:"confirmed"`
}

func validateQuestionAnswers(questions []UserQuestionItem, answers []UserQuestionResponse) error {
	if len(answers) != len(questions) || len(questions) == 0 {
		return errors.New("answer every question exactly once")
	}
	for i, q := range questions {
		if answers[i].QuestionID != q.ID || strings.TrimSpace(answers[i].Answer) == "" {
			return errors.New("answers must be nonempty and in question order")
		}
	}
	return nil
}

func (r *SessionRuntime) SaveUserQuestionDraft(ctx context.Context, wait UserQuestionAnswer, draft UserQuestionDraft) error {
	if r == nil || r.core == nil || ctx == nil {
		return errors.New("session runtime and context are required")
	}
	c := r.core

	q, pending, err := c.runtime.PendingUserQuestion(ctx, r.sessionID)
	if err != nil {
		return err
	}
	if !pending || q.ExecutionID != wait.ExecutionID || q.RunID != wait.RunID || q.InvocationID != wait.InvocationID || q.CallID != wait.CallID {
		return errors.New("question draft belongs to a stale wait")
	}
	// Serialize draft admission with answer receipts and task startup.
	c.mu.Lock()
	defer c.mu.Unlock()
	current, stillPending, err := c.runtime.PendingUserQuestion(ctx, r.sessionID)
	if err != nil {
		return err
	}
	if !stillPending || current.ExecutionID != wait.ExecutionID || current.RunID != wait.RunID || current.InvocationID != wait.InvocationID || current.CallID != wait.CallID {
		return errors.New("question draft belongs to a stale wait")
	}
	if state, known := c.activeTasks[r.sessionID]; known && (state.ExecutionID != wait.ExecutionID || state.Status == TaskCompleted || state.Status == TaskFailed || state.Status == TaskDiscarded || state.Status == TaskCancelled) {
		return errors.New("question draft belongs to a stale execution")
	}
	recorded, err := c.sessions.HasGraphResumeInput(ctx, r.sessionID, wait.RunID, wait.InvocationID, wait.CallID)
	if err != nil {
		return err
	}
	if recorded {
		return errors.New("question batch has already been submitted")
	}

	ids := map[string]UserQuestionItem{}
	for _, item := range q.Questions {
		ids[item.ID] = item
	}
	if _, ok := ids[draft.CurrentQuestionID]; !ok {
		return errors.New("invalid current question")
	}
	seen := map[string]bool{}
	for _, a := range draft.Responses {
		item, ok := ids[a.QuestionID]
		if !ok || seen[a.QuestionID] || a.SelectedOption < 0 || a.SelectedOption > len(item.Options) {
			return errors.New("invalid question draft")
		}
		seen[a.QuestionID] = true
		if a.Confirmed && a.SelectedOption == len(item.Options) && strings.TrimSpace(a.CustomAnswer) == "" {
			return errors.New("confirmed custom answer must not be empty")
		}
	}
	encoded, err := json.Marshal(draft)
	if err != nil {
		return err
	}
	return c.sessions.SaveUserQuestionDraft(ctx, r.sessionID, wait.RunID, wait.InvocationID, wait.CallID, encoded)
}
