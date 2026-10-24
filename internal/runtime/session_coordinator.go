package runtime

import (
	"context"
	"errors"
	"sync"

	publictool "github.com/afterlune/luneCore/tool"
)

// sessionCoordinator manages ephemeral concurrency state and synchronization
// primitives across active sessions.
type sessionCoordinator struct {
	mu                  sync.RWMutex
	activeTasks         map[string]TaskState
	taskCancels         map[string]context.CancelFunc
	approvals           map[string]pendingApproval
	interactions        map[string]pendingInteraction
	interactiveSessions map[string]bool
	turnWaiters         map[string]chan turnResult
	executionRuns       map[string]chan struct{}
	interruptRequests   map[string]bool
	queueDraining       map[string]bool
}

func newSessionCoordinator() *sessionCoordinator {
	return &sessionCoordinator{
		activeTasks:         make(map[string]TaskState),
		taskCancels:         make(map[string]context.CancelFunc),
		approvals:           make(map[string]pendingApproval),
		interactions:        make(map[string]pendingInteraction),
		interactiveSessions: make(map[string]bool),
		turnWaiters:         make(map[string]chan turnResult),
		executionRuns:       make(map[string]chan struct{}),
		interruptRequests:   make(map[string]bool),
		queueDraining:       make(map[string]bool),
	}
}

// Task State & Cancels

func (sc *sessionCoordinator) TaskState(sessionID string) (TaskState, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	state, ok := sc.activeTasks[sessionID]
	return state, ok
}

func (sc *sessionCoordinator) SetTaskState(sessionID string, state TaskState) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.activeTasks[sessionID] = state
}

func (sc *sessionCoordinator) HasRunningTasks() bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	for _, state := range sc.activeTasks {
		if state.Status == TaskRunning {
			return true
		}
	}
	return false
}

func (sc *sessionCoordinator) ActiveSessionIDs() []string {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	ids := make([]string, 0, len(sc.activeTasks))
	for id := range sc.activeTasks {
		ids = append(ids, id)
	}
	return ids
}

func (sc *sessionCoordinator) SetTaskCancel(sessionID string, cancel context.CancelFunc) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if cancel != nil {
		sc.taskCancels[sessionID] = cancel
	} else {
		delete(sc.taskCancels, sessionID)
	}
}

func (sc *sessionCoordinator) CancelTask(sessionID string) bool {
	sc.mu.Lock()
	cancel, ok := sc.taskCancels[sessionID]
	delete(sc.taskCancels, sessionID)
	sc.mu.Unlock()
	if ok && cancel != nil {
		cancel()
		return true
	}
	return false
}

// Approvals

func (sc *sessionCoordinator) AddApproval(id string, app pendingApproval) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.approvals[id] = app
}

func (sc *sessionCoordinator) GetApproval(id string) (pendingApproval, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	app, ok := sc.approvals[id]
	return app, ok
}

func (sc *sessionCoordinator) RemoveApproval(id string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	delete(sc.approvals, id)
}

func (sc *sessionCoordinator) ApprovalsForSession(sessionID string) []pendingApproval {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	var list []pendingApproval
	for _, app := range sc.approvals {
		if app.sessionID == sessionID {
			list = append(list, app)
		}
	}
	return list
}

// Interactions

func (sc *sessionCoordinator) AddInteraction(id string, inter pendingInteraction) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.interactions[id] = inter
}

func (sc *sessionCoordinator) GetInteraction(id string) (pendingInteraction, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	inter, ok := sc.interactions[id]
	return inter, ok
}

func (sc *sessionCoordinator) RemoveInteraction(id string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	delete(sc.interactions, id)
}

func (sc *sessionCoordinator) InteractionsForSession(sessionID string) []pendingInteraction {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	var list []pendingInteraction
	for _, inter := range sc.interactions {
		if inter.sessionID == sessionID {
			list = append(list, inter)
		}
	}
	return list
}

func (sc *sessionCoordinator) SetInteractive(sessionID string, available bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if available {
		sc.interactiveSessions[sessionID] = true
	} else {
		delete(sc.interactiveSessions, sessionID)
	}
}

func (sc *sessionCoordinator) IsInteractive(sessionID string) bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.interactiveSessions[sessionID]
}

// Turn Waiters

func (sc *sessionCoordinator) SetTurnWaiter(executionID string, waiter chan turnResult) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.turnWaiters[executionID] = waiter
}

func (sc *sessionCoordinator) TakeTurnWaiter(executionID string) chan turnResult {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	waiter := sc.turnWaiters[executionID]
	delete(sc.turnWaiters, executionID)
	return waiter
}

// Execution Runs

func (sc *sessionCoordinator) SetExecutionRun(executionID string, runDone chan struct{}) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.executionRuns[executionID] = runDone
}

func (sc *sessionCoordinator) GetExecutionRun(executionID string) chan struct{} {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.executionRuns[executionID]
}

func (sc *sessionCoordinator) ClearExecutionRun(executionID string, runDone chan struct{}) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.executionRuns[executionID] == runDone {
		delete(sc.executionRuns, executionID)
	}
}

// Interrupts

func (sc *sessionCoordinator) RequestInterrupt(executionID string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.interruptRequests[executionID] = true
}

func (sc *sessionCoordinator) TakeInterrupt(executionID string) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	req := sc.interruptRequests[executionID]
	delete(sc.interruptRequests, executionID)
	return req
}

func (sc *sessionCoordinator) ClearInterrupt(executionID string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	delete(sc.interruptRequests, executionID)
}

// Queue Draining

func (sc *sessionCoordinator) SetQueueDraining(sessionID string, draining bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if draining {
		sc.queueDraining[sessionID] = true
	} else {
		delete(sc.queueDraining, sessionID)
	}
}

func (sc *sessionCoordinator) IsQueueDraining(sessionID string) bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.queueDraining[sessionID]
}

func (sc *sessionCoordinator) HasQueueDraining() bool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return len(sc.queueDraining) > 0
}

// Close notifies and clears all pending waiters, approvals, and interactions.
func (sc *sessionCoordinator) Close(err error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if err == nil {
		err = errors.New("closed")
	}
	for executionID, waiter := range sc.turnWaiters {
		delete(sc.turnWaiters, executionID)
		waiter <- turnResult{err: err}
	}
	for id, pending := range sc.approvals {
		delete(sc.approvals, id)
		select {
		case pending.response <- publictool.ApprovalDeny:
		default:
		}
	}
	for id, pending := range sc.interactions {
		delete(sc.interactions, id)
		select {
		case pending.response <- userInteractionResult{Err: err}:
		default:
		}
	}
}
