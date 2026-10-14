package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	publictool "github.com/afterlune/luneCore/tool"
)

func (m *Manager) Start(ctx context.Context, request publictool.CommandRequest, timeout, yield time.Duration) (publictool.CommandOutput, error) {
	if ctx == nil {
		return publictool.CommandOutput{}, errors.New("command context is required")
	}
	if timeout < 0 || timeout > m.options.Timeout || yield < 0 || yield > MaxWait {
		return publictool.CommandOutput{}, errors.New("command timeout or yield exceeds its limit")
	}
	if timeout == 0 {
		timeout = m.options.Timeout
	}
	identity := publictool.CallIdentityFromContext(ctx)
	if identity.SessionID == "" || identity.ExecutionID == "" || identity.AgentID == "" || identity.InvocationID == "" {
		return publictool.CommandOutput{}, errors.New("command owner identity is required")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return publictool.CommandOutput{}, err
	}
	id := hex.EncodeToString(random[:])
	m.mu.Lock()
	s, exists := m.scopes[identity.ExecutionID]
	if !exists || s.ctx.Err() != nil || ctx.Err() != nil {
		m.mu.Unlock()
		return publictool.CommandOutput{}, errors.New("command task is not running")
	}
	for agent, closed := range s.closed {
		if closed && (identity.AgentID == agent || strings.HasPrefix(identity.AgentID, agent+"/")) {
			m.mu.Unlock()
			return publictool.CommandOutput{}, errors.New("command agent has ended")
		}
	}
	active := 0
	for _, j := range m.jobs {
		j.mu.Lock()
		running := j.record.Info.Status.Active()
		j.mu.Unlock()
		if running {
			active++
		}
	}
	if active >= m.options.MaxConcurrent {
		m.mu.Unlock()
		return publictool.CommandOutput{}, errors.New("concurrent command limit reached")
	}
	commandCtx, cancel := context.WithTimeout(publictool.WithCallIdentity(s.ctx, identity), timeout)
	info := publictool.CommandInfo{Command: request.Command, ID: id, SessionID: identity.SessionID, ExecutionID: identity.ExecutionID, AgentID: identity.AgentID, InvocationID: identity.InvocationID, TaskID: identity.TaskID, Status: publictool.CommandRunning, StartedAt: time.Now()}
	request.OnOutput = nil
	j := &job{
		record: Record{Info: info, Request: request}, cancel: cancel,
		done: make(chan struct{}), observerDone: make(chan struct{}), changed: make(chan struct{}),
	}
	if err := m.store.SaveCommand(ctx, j.record); err != nil {
		cancel()
		m.mu.Unlock()
		return publictool.CommandOutput{}, err
	}
	m.jobs[id] = j
	m.mu.Unlock()
	m.observe(info)
	go m.run(commandCtx, j)
	if yield > 0 {
		timer := time.NewTimer(yield)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			cancel()
			return publictool.CommandOutput{}, ctx.Err()
		case <-j.done:
		case <-timer.C:
		}
	}
	output, err := m.Read(ctx, identity.SessionID, id, identity.ExecutionID, identity.AgentID, publictool.CommandReadOptions{})
	if err != nil {
		cancel()
	}
	return output, err
}

func appendOutput(j *job, data []byte) {
	j.record.End += int64(len(data))
	if len(data) >= OutputLimit {
		j.record.Output = append(j.record.Output[:0], data[len(data)-OutputLimit:]...)
	} else {
		j.record.Output = append(j.record.Output, data...)
		if len(j.record.Output) > OutputLimit {
			copy(j.record.Output, j.record.Output[len(j.record.Output)-OutputLimit:])
			j.record.Output = j.record.Output[:OutputLimit]
		}
	}
	close(j.changed)
	j.changed = make(chan struct{})
}

func (m *Manager) run(ctx context.Context, j *job) {
	defer j.cancel()
	request := j.record.Request
	request.OnOutput = func(data []byte) error {
		j.mu.Lock()
		defer j.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if !j.record.Info.Status.Active() {
			return errors.New("command already ended")
		}
		j.streamed = true
		appendOutput(j, data)
		return nil
	}
	result, err := executeSafely(ctx, m.executor, request)
	j.mu.Lock()
	if !j.streamed {
		appendOutput(j, []byte(result.Output))
	}
	if result.ExitCode != nil {
		code := *result.ExitCode
		j.record.Info.ExitCode = &code
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		j.record.Info.Status = publictool.CommandTimedOut
	case ctx.Err() != nil:
		j.record.Info.Status = publictool.CommandCancelled
	case err != nil || result.ExitCode != nil && *result.ExitCode != 0:
		j.record.Info.Status = publictool.CommandFailed
	default:
		j.record.Info.Status = publictool.CommandSucceeded
	}
	if err != nil {
		j.record.Error = err.Error()
	}
	j.record.Info.FinishedAt = time.Now()
	persistErr := m.store.SaveCommand(context.Background(), j.record)
	info := cloneInfo(j.record.Info)
	close(j.changed)
	j.mu.Unlock()
	if persistErr == nil {
		// The terminal observer persists lifecycle events through Core's session
		// store. Close must not treat the command as finished until that write
		// has returned.
		_ = m.observeTerminal(context.Background(), j, info)
		m.mu.Lock()
		delete(m.jobs, info.ID)
		m.mu.Unlock()
	}
	j.mu.Lock()
	close(j.done)
	j.mu.Unlock()
}

func (m *Manager) Stop(ctx context.Context, sessionID, id, executionID, agentID string) (publictool.CommandOutput, error) {
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j != nil {
		j.mu.Lock()
		if !owned(j.record, sessionID, executionID, agentID) {
			j.mu.Unlock()
			return publictool.CommandOutput{}, errors.New("command does not belong to this owner")
		}
		if j.record.Info.Status.Active() {
			j.record.Info.Status = publictool.CommandStopping
			j.cancel()
		}
		j.mu.Unlock()
		select {
		case <-ctx.Done():
			return publictool.CommandOutput{}, ctx.Err()
		case <-j.done:
		}
	}
	return m.Read(ctx, sessionID, id, executionID, agentID, publictool.CommandReadOptions{})
}

func executeSafely(ctx context.Context, executor publictool.CommandExecutor, request publictool.CommandRequest) (result publictool.CommandResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("command executor panicked (%T)", recovered)
		}
	}()
	return executor.Execute(ctx, request)
}
