package session

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkAppendRuntimeEvent(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "events.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	payload := json.RawMessage(`{"kind":"tool_finished","executionId":"execution","agentId":"lead"}`)
	for _, variant := range []struct {
		name   string
		append func() error
	}{
		{name: "single_statement", append: func() error {
			_, err := store.AppendRuntimeEvent(ctx, "benchmark-session", payload)
			return err
		}},
		{name: "explicit_transaction", append: func() error {
			return appendRuntimeEventWithTransaction(ctx, store, "benchmark-session", payload)
		}},
	} {
		b.Run(variant.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := variant.append(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func appendRuntimeEventWithTransaction(ctx context.Context, store *Store, sessionID string, payload json.RawMessage) error {
	if store == nil || store.db == nil || sessionID == "" || !json.Valid(payload) {
		return fmt.Errorf("session store, session id, and valid event JSON are required")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin runtime event append: %w", err)
	}
	defer tx.Rollback()
	if _, err := appendRuntimeEventTx(ctx, tx, sessionID, payload, time.Now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit runtime event: %w", err)
	}
	return nil
}

func BenchmarkRuntimeEventsAfter_Scales(b *testing.B) {
	for _, eventCount := range []int{200, 1000} {
		b.Run(fmt.Sprintf("events=%d", eventCount), func(b *testing.B) {
			store, err := Open(filepath.Join(b.TempDir(), "events.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()

			ctx := context.Background()
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				b.Fatal(err)
			}
			payload := json.RawMessage(`{"kind":"task_started","taskId":"benchmark-task"}`)
			for i := range eventCount {
				if _, err := appendRuntimeEventTx(ctx, tx, "benchmark-session", payload, time.Unix(int64(i), 0)); err != nil {
					_ = tx.Rollback()
					b.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := store.RuntimeEventsAfter(ctx, "benchmark-session", 0, eventCount); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
