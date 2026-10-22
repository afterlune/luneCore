// Package agentruntime adapts luneGraph execution for the core agent.
package agentruntime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
)

const activeRunID = "lune-cli-active"

type Responder struct {
	logger                *slog.Logger
	agent                 *agentgraph.Agent
	store                 graph.Store[agentgraph.State]
	limiter               *graph.Limiter
	execution             *graph.Execution[agentgraph.State]
	lastState             *agentgraph.State
	requester             *UsageTrackingRequester
	historyMu             sync.RWMutex
	history               *model.ResponseInput // provider-specific model context
	transcript            *model.ResponseInput
	contextKey            string
	sessionID             string
	taskOptions           agentgraph.TaskOptions
	modelName             string
	reasoningEffort       string
	historySaver          func(string, *model.ResponseInput, *model.ResponseInput, string, json.RawMessage) error
	deferCompletedCleanup bool
}

func (r *Responder) SetTaskOptions(options agentgraph.TaskOptions) { r.taskOptions = options }

func (r *Responder) SetRunModel(agent *agentgraph.Agent, requester *UsageTrackingRequester, modelName, reasoningEffort string) {
	r.agent = agent
	r.requester = requester
	r.modelName = modelName
	r.reasoningEffort = reasoningEffort
}

func (r *Responder) SetHistorySaver(saver func(string, *model.ResponseInput, *model.ResponseInput, string, json.RawMessage) error) {
	r.historyMu.Lock()
	r.historySaver = saver
	r.historyMu.Unlock()
}

func (r *Responder) DeferCompletedCleanup(deferCleanup bool) {
	r.deferCompletedCleanup = deferCleanup
}

func (r *Responder) SetSession(id string, history *model.ResponseInput) {
	r.SetSessionState(id, history, history, "")
}

func (r *Responder) SetSessionState(id string, transcript, modelContext *model.ResponseInput, contextKey string) {
	r.historyMu.Lock()
	defer r.historyMu.Unlock()
	r.sessionID = id
	r.lastState = nil
	if transcript == nil {
		r.transcript = nil
	} else {
		r.transcript = model.NewResponseInputItems(responseHistoryItems(transcript)...)
	}
	if modelContext == nil {
		modelContext = transcript
	}
	if modelContext == nil {
		r.history = nil
	} else {
		r.history = model.NewResponseInputItems(responseHistoryItems(modelContext)...)
	}
	r.contextKey = contextKey
}

func (r *Responder) SessionHistory() *model.ResponseInput {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	if r.transcript == nil {
		return nil
	}
	return model.NewResponseInputItems(responseHistoryItems(r.transcript)...)
}

func (r *Responder) ModelContextHistory() *model.ResponseInput {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	if r.history == nil {
		return nil
	}
	return model.NewResponseInputItems(responseHistoryItems(r.history)...)
}

func (r *Responder) ContextKey() string {
	r.historyMu.RLock()
	defer r.historyMu.RUnlock()
	return r.contextKey
}

func (r *Responder) persistSessionHistory() error {
	r.historyMu.RLock()
	id, saver, contextKey := r.sessionID, r.historySaver, r.contextKey
	r.historyMu.RUnlock()
	if saver == nil || id == "" {
		return nil
	}
	return saver(id, r.SessionHistory(), r.ModelContextHistory(), contextKey, r.ConversationState())
}

// PendingQuestion describes a durable lead-agent clarification wait.
type PendingQuestion struct {
	ID           string
	AgentID      string
	RunID        string
	InvocationID string
	CallID       string
	ExecutionID  string
	Questions    []model.UserQuestionItem
}

func NewResponder(store graph.Store[agentgraph.State], limiter *graph.Limiter, logger *slog.Logger) *Responder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Responder{store: store, limiter: limiter, logger: logger}
}

func (r *Responder) SetExecution(execution *graph.Execution[agentgraph.State]) {
	r.execution = execution
}

func (r *Responder) Execution() *graph.Execution[agentgraph.State] {
	return r.execution
}

func (r *Responder) LastState() (agentgraph.State, bool) {
	if r == nil || r.lastState == nil {
		return agentgraph.State{}, false
	}
	return *r.lastState, true
}

func (r *Responder) Fork(ctx context.Context, newRunID string, source graph.Checkpoint[agentgraph.State]) (graph.Checkpoint[agentgraph.State], error) {
	return r.agent.Fork(ctx, newRunID, source)
}

// ConversationState returns host metadata, separate from user-authored history.
func (r *Responder) ConversationState() json.RawMessage {
	state := r.taskOptions.Conversation
	if r.lastState != nil {
		state = r.lastState.ConversationState()
	}
	data, _ := json.Marshal(state)
	return data
}
