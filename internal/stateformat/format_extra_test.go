package stateformat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAndUpgradeEdgeCases(t *testing.T) {
	dir := t.TempDir()
	lock, err := Open(dir)
	if err != nil {
		t.Fatalf("fresh Open: %v", err)
	}
	_ = lock.Close()

	// Wrong format version triggers ErrUpgradeRequired.
	badDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(badDir, markerFile), []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(badDir); !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("expected ErrUpgradeRequired for version 1, got %v", err)
	}

	// Non-empty legacy sessions.db without marker triggers ErrUpgradeRequired.
	legacyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacyDir, "sessions.db"), []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(legacyDir); !errors.Is(err, ErrUpgradeRequired) {
		t.Fatalf("expected ErrUpgradeRequired for legacy db, got %v", err)
	}

	// Upgrade validation errors.
	if _, err := Upgrade(nil, dir, func(r json.RawMessage) (json.RawMessage, error) { return r, nil }); err == nil {
		t.Fatal("expected error for nil context")
	}
	if _, err := Upgrade(context.Background(), dir, nil); err == nil {
		t.Fatal("expected error for nil transform")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Upgrade(canceled, dir, func(r json.RawMessage) (json.RawMessage, error) { return r, nil }); err == nil {
		t.Fatal("expected error for canceled context")
	}
	if _, err := Upgrade(context.Background(), filepath.Join(dir, "missing"), func(r json.RawMessage) (json.RawMessage, error) { return r, nil }); err == nil {
		t.Fatal("expected error for missing dir")
	}

	// Already current returns AlreadyCurrent=true.
	rep, err := Upgrade(context.Background(), dir, func(r json.RawMessage) (json.RawMessage, error) { return r, nil })
	if err != nil || !rep.AlreadyCurrent {
		t.Fatalf("expected AlreadyCurrent=true, got %#v err=%v", rep, err)
	}
}
