package runtime

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/afterlune/luneCore/internal/session"
)

func BenchmarkCoreEmitDurableEvent(b *testing.B) {
	store, err := session.Open(filepath.Join(b.TempDir(), "events.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	core := &Core{
		sessions: store,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	event := Event{
		Kind: EventToolFinished, SessionID: "benchmark-session", TaskID: "benchmark-task",
		ExecutionID: "benchmark-execution", AgentID: "lead", AgentName: "Lead",
		Tool: ToolCall{ExecutionID: "benchmark-execution", ID: "call-1", AgentID: "lead", Name: "read_file", Duration: 3 * time.Millisecond},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		core.emit(event)
	}
}

func BenchmarkCoreReadPersistedRuntimeEvents(b *testing.B) {
	store, err := session.Open(filepath.Join(b.TempDir(), "events.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	core := &Core{
		sessions: store,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for range 200 {
		core.emit(Event{Kind: EventTaskStarted, SessionID: "benchmark-session", TaskID: "task"})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := store.RuntimeEventsAfter(context.Background(), "benchmark-session", 0, 200); err != nil {
			b.Fatal(err)
		}
	}
}
