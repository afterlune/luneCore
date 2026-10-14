// Package command owns non-interactive processes within an agent task.
package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	publictool "github.com/afterlune/luneCore/tool"
)

const OutputLimit = 1 << 20
const ReadLimit = 64 << 10
const MaxWait = 5 * time.Second

type Record struct {
	Info    publictool.CommandInfo    `json:"info"`
	Request publictool.CommandRequest `json:"request"`
	Output  []byte                    `json:"output,omitempty"`
	End     int64                     `json:"end"`
	Error   string                    `json:"error,omitempty"`
}

type Store interface {
	SaveCommand(context.Context, Record) error
	GetCommand(context.Context, string, string) (Record, error)
	ListCommands(context.Context, string, string) ([]Record, error)
	InterruptCommands(context.Context) error
}

type scope struct {
	ctx    context.Context
	closed map[string]bool
}
type job struct {
	mu       sync.Mutex
	record   Record
	cancel   context.CancelFunc
	done     chan struct{}
	changed  chan struct{}
	streamed bool
}

type Manager struct {
	mu       sync.Mutex
	store    Store
	executor publictool.CommandExecutor
	options  publictool.CommandOptions
	scopes   map[string]*scope
	jobs     map[string]*job
	observer func(publictool.CommandInfo)
}

func New(ctx context.Context, store Store, executor publictool.CommandExecutor, options publictool.CommandOptions, observer func(publictool.CommandInfo)) (*Manager, error) {
	if store == nil || executor == nil {
		return nil, errors.New("command store and executor are required")
	}
	if options.Timeout < 0 || options.MaxConcurrent < 0 {
		return nil, errors.New("command limits must not be negative")
	}
	if options.Timeout == 0 {
		options.Timeout = 10 * time.Minute
	}
	if options.MaxConcurrent == 0 {
		options.MaxConcurrent = 4
	}
	if err := store.InterruptCommands(ctx); err != nil {
		return nil, err
	}
	return &Manager{store: store, executor: executor, options: options, scopes: map[string]*scope{}, jobs: map[string]*job{}, observer: observer}, nil
}

func (m *Manager) OpenScope(executionID string, ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if executionID == "" || ctx == nil {
		return errors.New("command execution scope is required")
	}
	if _, exists := m.scopes[executionID]; exists {
		return errors.New("command scope already exists")
	}
	m.scopes[executionID] = &scope{ctx: ctx, closed: map[string]bool{}}
	return nil
}

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
	j := &job{record: Record{Info: info, Request: request}, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{})}
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
	close(j.done)
	close(j.changed)
	j.mu.Unlock()
	if persistErr == nil {
		m.mu.Lock()
		delete(m.jobs, info.ID)
		m.mu.Unlock()
		m.observe(info)
	}
}

func cloneInfo(info publictool.CommandInfo) publictool.CommandInfo {
	if info.ExitCode != nil {
		code := *info.ExitCode
		info.ExitCode = &code
	}
	return info
}

func owned(record Record, sessionID, executionID, agentID string) bool {
	return record.Info.SessionID == sessionID && (executionID == "" || record.Info.ExecutionID == executionID) && (agentID == "" || record.Info.AgentID == agentID)
}

func (m *Manager) Read(ctx context.Context, sessionID, id, executionID, agentID string, options publictool.CommandReadOptions) (publictool.CommandOutput, error) {
	if ctx == nil || options.Cursor < 0 || options.Wait < 0 || options.Wait > MaxWait {
		return publictool.CommandOutput{}, errors.New("invalid command read options")
	}
	m.mu.Lock()
	j := m.jobs[id]
	m.mu.Unlock()
	if j != nil {
		j.mu.Lock()
		if !owned(j.record, sessionID, executionID, agentID) {
			j.mu.Unlock()
			return publictool.CommandOutput{}, errors.New("command does not belong to this owner")
		}
		if options.Cursor > j.record.End {
			j.mu.Unlock()
			return publictool.CommandOutput{}, errors.New("command cursor exceeds output")
		}
		changed := j.changed
		wait := j.record.Info.Status.Active() && j.record.End <= options.Cursor && options.Wait > 0
		j.mu.Unlock()
		if wait {
			timer := time.NewTimer(options.Wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return publictool.CommandOutput{}, ctx.Err()
			case <-changed:
			case <-timer.C:
			}
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		if err := m.store.SaveCommand(ctx, j.record); err != nil {
			return publictool.CommandOutput{}, err
		}
		return output(j.record, options.Cursor)
	}
	record, err := m.store.GetCommand(ctx, sessionID, id)
	if err != nil {
		return publictool.CommandOutput{}, err
	}
	if !owned(record, sessionID, executionID, agentID) {
		return publictool.CommandOutput{}, errors.New("command does not belong to this owner")
	}
	return output(record, options.Cursor)
}

func output(record Record, cursor int64) (publictool.CommandOutput, error) {
	if cursor > record.End {
		return publictool.CommandOutput{}, errors.New("command cursor exceeds output")
	}
	base := record.End - int64(len(record.Output))
	truncated := cursor < base
	if truncated {
		cursor = base
	}
	end := cursor + ReadLimit
	if end > record.End {
		end = record.End
	}
	return publictool.CommandOutput{Command: cloneInfo(record.Info), Output: strings.ToValidUTF8(string(record.Output[cursor-base:end-base]), "\uFFFD"), Cursor: cursor, NextCursor: end, EndCursor: record.End, Truncated: truncated, Error: record.Error}, nil
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

func (m *Manager) List(ctx context.Context, sessionID, executionID string) ([]publictool.CommandInfo, error) {
	records, err := m.store.ListCommands(ctx, sessionID, executionID)
	if err != nil {
		return nil, err
	}
	result := make([]publictool.CommandInfo, 0, len(records))
	for _, record := range records {
		m.mu.Lock()
		j := m.jobs[record.Info.ID]
		m.mu.Unlock()
		if j != nil {
			j.mu.Lock()
			record.Info = cloneInfo(j.record.Info)
			j.mu.Unlock()
		}
		result = append(result, record.Info)
	}
	return result, nil
}

func (m *Manager) Active(executionID, agentID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		j.mu.Lock()
		active := j.record.Info.ExecutionID == executionID && j.record.Info.AgentID == agentID && j.record.Info.Status.Active()
		j.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

// StopScope closes admission before cancelling every process, then waits for
// durable termination. The caller must not advance its queue on cleanup errors.
func (m *Manager) StopScope(ctx context.Context, executionID, agentID string) error {
	m.mu.Lock()
	if s := m.scopes[executionID]; s != nil {
		s.closed[agentID] = true
		if agentID == "" {
			delete(m.scopes, executionID)
		}
	}
	var jobs []*job
	for _, j := range m.jobs {
		j.mu.Lock()
		match := j.record.Info.ExecutionID == executionID && (agentID == "" || j.record.Info.AgentID == agentID || strings.HasPrefix(j.record.Info.AgentID, agentID+"/"))
		if match && j.record.Info.Status.Active() {
			j.record.Info.Status = publictool.CommandStopping
			j.cancel()
		}
		j.mu.Unlock()
		if match {
			jobs = append(jobs, j)
		}
	}
	m.mu.Unlock()
	for _, j := range jobs {
		select {
		case <-ctx.Done():
			return fmt.Errorf("command cleanup: %w", ctx.Err())
		case <-j.done:
		}
		j.mu.Lock()
		err := m.store.SaveCommand(ctx, j.record)
		info := j.record.Info
		j.mu.Unlock()
		if err != nil {
			return fmt.Errorf("persist command cleanup: %w", err)
		}
		m.mu.Lock()
		delete(m.jobs, info.ID)
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) observe(info publictool.CommandInfo) {
	if m.observer != nil {
		m.observer(cloneInfo(info))
	}
}

func (m *Manager) OpenAgent(executionID, agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.scopes[executionID]; s != nil {
		delete(s.closed, agentID)
	}
}

func executeSafely(ctx context.Context, executor publictool.CommandExecutor, request publictool.CommandRequest) (result publictool.CommandResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("command executor panicked (%T)", recovered)
		}
	}()
	return executor.Execute(ctx, request)
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	ids := map[string]bool{}
	for id := range m.scopes {
		ids[id] = true
	}
	for _, j := range m.jobs {
		ids[j.record.Info.ExecutionID] = true
	}
	m.mu.Unlock()
	for id := range ids {
		if err := m.StopScope(ctx, id, ""); err != nil {
			return err
		}
	}
	return nil
}
func (m *Manager) StopSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	ids := map[string]bool{}
	for _, j := range m.jobs {
		if j.record.Info.SessionID == sessionID {
			ids[j.record.Info.ExecutionID] = true
		}
	}
	m.mu.Unlock()
	for id := range ids {
		if err := m.StopScope(ctx, id, ""); err != nil {
			return err
		}
	}
	return nil
}
