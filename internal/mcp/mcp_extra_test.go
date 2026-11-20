package mcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMemoryCredentialStorageAndOAuthHelpers(t *testing.T) {
	store := NewMemoryCredentialStorage()
	if _, err := store.Get("svc", "acc"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("expected ErrCredentialNotFound, got %v", err)
	}
	if err := store.Set("svc", "acc", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("svc", "acc")
	if err != nil || string(got) != "secret" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := store.Delete("svc", "acc"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("svc", "acc"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("expected ErrCredentialNotFound on second delete, got %v", err)
	}

	// HeadlessBrowserOpener records opened URLs
	var opened string
	opener := &HeadlessBrowserOpener{OnOpen: func(target string) { opened = target }}
	if err := opener.Open(context.Background(), "https://example.com/auth"); err != nil {
		t.Fatal(err)
	}
	if opened != "https://example.com/auth" {
		t.Fatalf("unexpected opened URL: %q", opened)
	}

	// OAuth callback listener & token helpers
	ln, addr, err := openOAuthCallbackListener(false)
	if err != nil {
		t.Fatal(err)
	}
	uri := oauthRedirectURI(addr)
	if uri == "" {
		t.Fatal("expected non-empty redirect URI")
	}
	_ = ln.Close()

	tok, err := randomToken(16)
	if err != nil || len(tok) == 0 {
		t.Fatalf("randomToken = %q, %v", tok, err)
	}
	if !containsString([]string{"a", "b"}, "b") || containsString([]string{"a", "b"}, "c") {
		t.Fatal("unexpected containsString result")
	}
	if _, err := decodeOAuthToken(&http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`))}); err == nil {
		t.Fatal("expected error for 400 token response")
	}
	parsed, err := decodeOAuthToken(&http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))})
	if err != nil || parsed.AccessToken != "tok" {
		t.Fatalf("decodeOAuthToken = %#v, %v", parsed, err)
	}
}
