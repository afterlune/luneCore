// Package command owns non-interactive processes within an agent task.
package command

import (
	"context"
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

func (m *Manager) OpenAgent(executionID, agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.scopes[executionID]; s != nil {
		delete(s.closed, agentID)
	}
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

func (m *Manager) observe(info publictool.CommandInfo) {
	if m.observer != nil {
		m.observer(cloneInfo(info))
	}
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
