package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type delayedCloseTransport struct {
	release <-chan struct{}
	calls   atomic.Int32
	err     error
}

func (*delayedCloseTransport) Request(context.Context, string, map[string]any, string, []headerBinding) (json.RawMessage, error) {
	return nil, errors.New("unused")
}
func (t *delayedCloseTransport) Close() error { t.calls.Add(1); <-t.release; return t.err }

func TestManagerCloseDeadlineRetainsCleanupAndSharesResult(t *testing.T) {
	for _, hostBlocked := range []bool{true, false} {
		t.Run(map[bool]string{true: "host operation", false: "transport"}[hostBlocked], func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			finished := make(chan struct{})
			failure := errors.New("transport cleanup failure")
			transport := &delayedCloseTransport{release: finished, err: failure}
			instance := &serverInstance{client: transport}
			manager := &Manager{instances: map[string]*serverInstance{"one": instance}}
			if hostBlocked {
				instance.hostOperationsWG.Add(1)
				go func() { <-release; instance.hostOperationsWG.Done(); close(finished) }()
			} else {
				go func() { <-release; close(finished) }()
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			if err := manager.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v", err)
			}
			manager.mu.Lock()
			owned := len(manager.instances)
			manager.mu.Unlock()
			if owned != 1 {
				t.Fatal("cleanup ownership released before completion")
			}
			if hostBlocked && transport.calls.Load() != 0 {
				t.Fatal("transport closed before operation joined")
			}
			// Concurrent callers must join one cleanup, including its final error.
			results := make(chan error, 2)
			for range 2 {
				go func() { results <- manager.Close(context.Background()) }()
			}
			unblock()
			for range 2 {
				if err := <-results; !errors.Is(err, failure) {
					t.Fatalf("lost cleanup error: %v", err)
				}
			}
			if transport.calls.Load() != 1 {
				t.Fatalf("duplicate close: %d", transport.calls.Load())
			}
			if err := manager.Close(t.Context()); !errors.Is(err, failure) {
				t.Fatalf("repeat error=%v", err)
			}
		})
	}
}

func TestManagerCloseJoinsEveryTransportOnce(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	manager := &Manager{instances: make(map[string]*serverInstance)}
	var transports []*delayedCloseTransport
	for _, name := range []string{"one", "two", "three"} {
		transport := &delayedCloseTransport{release: release}
		transports = append(transports, transport)
		manager.instances[name] = &serverInstance{client: transport}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := manager.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	unblock()
	if err := manager.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, transport := range transports {
		if transport.calls.Load() != 1 {
			t.Fatal("transport was lost or closed twice")
		}
	}
}
