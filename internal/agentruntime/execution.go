package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	graph "github.com/afterlune/luneGraph"
	"github.com/afterlune/luneGraph/checkpoint"
)

func (r *Responder) Respond(ctx context.Context, input *model.ResponseInput) (*model.Response, error) {
	if pending, err := r.HasPending(); err != nil {
		return nil, fmt.Errorf("inspect pending task: %w", err)
	} else if pending {
		return nil, errors.New("a task is paused in this session; use /continue or start a new session")
	}
	runID := r.runID()
	transcript := r.SessionHistory()
	modelContext := r.ModelContextHistory()
	execution, result, err := r.agent.StartWithTranscript(ctx, runID, modelContext, transcript, input, r.taskOptions, r.store, r.options())
	r.execution = execution
	return r.finish(result, err)
}

func (r *Responder) Resume(ctx context.Context, inputs []graph.ResumeInput) (*model.Response, error) {
	runID := r.runID()
	var result graph.Result[agentgraph.State]
	var err error
	if r.execution != nil {
		result, err = r.execution.Advance(ctx, inputs, r.options())
	} else {
		r.execution, result, err = r.agent.Recover(ctx, runID, inputs, r.store, r.options())
	}
	return r.finish(result, err)
}

func (r *Responder) HasPending() (bool, error) {
	return r.HasPendingFor(r.sessionID, context.Background())
}

func (r *Responder) HasPendingFor(sessionID string, ctx context.Context) (bool, error) {
	cp, err := r.store.Load(ctx, r.CheckpointID(sessionID))
	if errors.Is(err, checkpoint.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !cp.Completed, nil
}

func (r *Responder) options() graph.Options {
	maxConcurrency := r.taskOptions.Execution.MaxConcurrency
	if maxConcurrency == 0 {
		maxConcurrency = r.agent.MaxConcurrency()
	}
	return graph.Options{
		Observer: graph.ObserverFunc(logGraphEvent), Limiter: r.limiter,
		MaxSteps: r.taskOptions.Execution.MaxSteps, MaxInvocations: r.taskOptions.Execution.MaxInvocations,
		MaxConcurrency: maxConcurrency,
	}
}

func logGraphEvent(ctx context.Context, event graph.Event) {
	level := slog.LevelDebug
	if event.Operation == graph.OperationStart || event.Operation == graph.OperationResume || event.Operation == graph.OperationAdvance || event.Operation == graph.OperationRecover {
		level = slog.LevelInfo
	}
	if event.Err != nil {
		level = slog.LevelError
	}
	attrs := []slog.Attr{
		slog.String("operation", string(event.Operation)),
		slog.String("phase", string(event.Phase)),
		slog.String("run_id", event.RunID),
		slog.String("node", event.Node),
		slog.String("status", string(event.Status)),
	}
	if event.CallID != "" {
		attrs = append(attrs, slog.String("call_id", event.CallID))
	}
	if event.Err != nil {
		attrs = append(attrs, slog.String("error_type", fmt.Sprintf("%T", event.Err)))
	}
	slog.LogAttrs(ctx, level, "graph execution", attrs...)
}

func (r *Responder) finish(result graph.Result[agentgraph.State], runErr error) (*model.Response, error) {
	if result.Status == graph.StatusCompleted || result.Status == graph.StatusCompletedWithFailures {
		if result.Final == nil {
			if !r.deferCompletedCleanup {
				if err := r.deleteCompletedRun(); err != nil {
					runErr = errors.Join(runErr, err)
				}
			}
			return nil, errors.Join(runErr, errors.New("agent graph completed without a final state"))
		}
		state := result.Final
		r.historyMu.Lock()
		r.history = model.NewResponseInputItems(state.History.Items...)
		transcript := state.Transcript
		if transcript.Items == nil && transcript.Text == nil {
			transcript = state.History
		}
		r.transcript = model.NewResponseInputItems(responseHistoryItems(&transcript)...)
		r.historyMu.Unlock()
		response := state.FinalResponse
		if !r.deferCompletedCleanup {
			if err := r.persistSessionHistory(); err != nil {
				return response, errors.Join(runErr, fmt.Errorf("persist completed session history: %w", err))
			}
		}
		if response == nil {
			if !r.deferCompletedCleanup {
				if err := r.deleteCompletedRun(); err != nil {
					runErr = errors.Join(runErr, err)
				}
			}
			if state.TaskError != "" {
				return nil, errors.Join(runErr, errors.New(state.TaskError))
			}
			return nil, errors.Join(runErr, errors.New("agent graph completed without a final response"))
		}
		if !r.deferCompletedCleanup {
			if err := r.deleteCompletedRun(); err != nil {
				return response, errors.Join(runErr, err)
			}
		}
		if state.TaskError != "" {
			return response, errors.Join(runErr, errors.New(state.TaskError))
		}
		return response, runErr
	}
	if result.Status == graph.StatusFailed {
		// Only remove a persisted graph failure. Store conflicts, ledger errors,
		// and unresolved tool outcomes can leave a valid checkpoint to recover.
		var recoveryRequired *ToolRecoveryRequiredError
		preserveCheckpoint := errors.Is(runErr, graph.ErrConflict) || errors.Is(runErr, ErrToolInvocationPersistence) || errors.As(runErr, &recoveryRequired)
		if result.Header.Failure != nil && !preserveCheckpoint && !r.deferCompletedCleanup {
			if err := r.deleteCompletedRun(); err != nil {
				runErr = errors.Join(runErr, err)
			}
		}
		if runErr == nil {
			runErr = errors.New("agent graph failed")
		}
	}
	if result.Status == graph.StatusCancelled || result.Status == graph.StatusBudget || result.Status == graph.StatusWaiting {
		attrs := []any{"run_id", r.runID(), "status", result.Status}
		if result.BudgetReason != "" {
			attrs = append(attrs, "budget_reason", string(result.BudgetReason))
		}
		slog.Warn("agent task remains resumable", attrs...)
		if result.Status == graph.StatusWaiting || result.Status == graph.StatusBudget {
			if result.Status == graph.StatusBudget {
				return nil, &TaskPausedError{BudgetReason: BudgetReason(result.BudgetReason)}
			}
			return nil, ErrTaskPaused
		}
	}
	return nil, runErr
}

func (r *Responder) deleteCompletedRun() error {
	err := r.store.Delete(context.Background(), r.runID())
	if errors.Is(err, checkpoint.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete completed task checkpoint: %w", err)
	}
	return nil
}
