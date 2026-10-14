package runtime

import "github.com/afterlune/luneCore/internal/agentruntime"

func publicUserQuestionRequest(question agentruntime.PendingQuestion) UserQuestionRequest {
	return UserQuestionRequest{
		ID: question.ID, AgentID: question.AgentID, RunID: question.RunID,
		InvocationID: question.InvocationID, CallID: question.CallID, ExecutionID: question.ExecutionID,
		Question: question.Question, Options: append([]string(nil), question.Options...),
	}
}
