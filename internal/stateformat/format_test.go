package stateformat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFutureFormatAndCancelledUpgradeNeverTransform(t *testing.T) {
	for _, test := range []string{"future", "cancelled"} {
		t.Run(test, func(t *testing.T) {
			dir := t.TempDir()
			ctx := t.Context()
			if test == "future" {
				if err := os.WriteFile(filepath.Join(dir, markerFile), []byte("999\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			_, err := Upgrade(ctx, dir, func(json.RawMessage) (json.RawMessage, error) { t.Fatal("transformation ran"); return nil, nil })
			if err == nil {
				t.Fatal("upgrade accepted")
			}
			if test == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "sessions.db")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unexpected database created")
			}
		})
	}
}

func TestSharedLifecycleLockExcludesUpgrade(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := Upgrade(t.Context(), dir, func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }); err == nil {
		t.Fatal("upgrade ignored live readers")
	}
}
