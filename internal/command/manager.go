// Package command owns task-scoped commands and session-managed services.
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
	mu              sync.Mutex
	record          Record
	cancel          context.CancelFunc
	done            chan struct{}
	observerDone    chan struct{}
	observerStarted bool
	changed         chan struct{}
	streamed        bool
}

type Manager struct {
	mu       sync.Mutex
	store    Store
	executor publictool.CommandExecutor
	options  publictool.CommandOptions
	scopes   map[string]*scope
	jobs     map[string]*job
	observer func(publictool.CommandInfo)
	closing  bool
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
	if m.closing {
		return errors.New("command manager is closing")
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
		active := !j.record.Info.Background && j.record.Info.ExecutionID == executionID && j.record.Info.AgentID == agentID && j.record.Info.Status.Active()
		j.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

// StopScope closes admission before cancelling task-scoped processes, then
// waits for durable termination. Session-managed background services survive.
// The caller must not advance its queue on cleanup errors.
func (m *Manager) StopScope(ctx context.Context, executionID, agentID string) error {
	if ctx == nil {
		return errors.New("command cleanup context is required")
	}
	m.mu.Lock()
	if s := m.scopes[executionID]; s != nil {
		s.closed[agentID] = true
		if agentID == "" {
			delete(m.scopes, executionID)
		}
	}
	m.mu.Unlock()
	return m.stopMatching(ctx, func(info publictool.CommandInfo) bool {
		return !info.Background && info.ExecutionID == executionID &&
			(agentID == "" || info.AgentID == agentID || strings.HasPrefix(info.AgentID, agentID+"/"))
	})
}

func (m *Manager) stopMatching(ctx context.Context, matches func(publictool.CommandInfo) bool) error {
	if ctx == nil {
		return errors.New("command cleanup context is required")
	}
	m.mu.Lock()
	var jobs []*job
	for _, j := range m.jobs {
		j.mu.Lock()
		match := matches(j.record.Info)
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
		if err := m.observeTerminal(ctx, j, info); err != nil {
			return err
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
	if m.closing {
		m.mu.Unlock()
		return errors.New("command manager is already closing")
	}
	m.closing = true
	m.scopes = map[string]*scope{}
	m.mu.Unlock()
	err := m.stopMatching(ctx, func(publictool.CommandInfo) bool { return true })
	m.mu.Lock()
	m.closing = false
	m.mu.Unlock()
	return err
}

func (m *Manager) observe(info publictool.CommandInfo) {
	if m.observer != nil {
		m.observer(cloneInfo(info))
	}
}

func (m *Manager) observeTerminal(ctx context.Context, j *job, info publictool.CommandInfo) error {
	j.mu.Lock()
	if !j.observerStarted {
		j.observerStarted = true
		done := j.observerDone
		j.mu.Unlock()
		m.observe(info)
		close(done)
		return nil
	}
	done := j.observerDone
	j.mu.Unlock()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait for command event observer: %w", ctx.Err())
	case <-done:
		return nil
	}
}

func (m *Manager) StopSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("command session id is required")
	}
	return m.stopMatching(ctx, func(info publictool.CommandInfo) bool { return info.SessionID == sessionID })
}
