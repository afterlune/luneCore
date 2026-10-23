package mcp

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryCredentialStorageCRUD(t *testing.T) {
	storage := NewMemoryCredentialStorage()

	// Get non-existent
	_, err := storage.Get("svc", "acc")
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("expected ErrCredentialNotFound, got %v", err)
	}

	// Set
	data := []byte(`{"access_token":"secret123"}`)
	if err := storage.Set("svc", "acc", data); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	// Get existent
	got, err := storage.Get("svc", "acc")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %s, want %s", got, data)
	}

	// Delete
	if err := storage.Delete("svc", "acc"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Get after delete
	_, err = storage.Get("svc", "acc")
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("expected ErrCredentialNotFound after delete, got %v", err)
	}
}

func TestHeadlessBrowserOpener(t *testing.T) {
	var opened string
	opener := HeadlessBrowserOpener{
		OnOpen: func(target string) {
			opened = target
		},
	}
	if err := opener.Open(context.Background(), "https://example.com/oauth"); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if opened != "https://example.com/oauth" {
		t.Fatalf("got %q, want https://example.com/oauth", opened)
	}
}
