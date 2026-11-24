package model

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestStreamRequestClientUsesExecutionDeadline(t *testing.T) {
	client := &http.Client{Timeout: 2 * time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	streamClient := streamRequestClient(ctx, client)
	if streamClient == client {
		t.Fatal("stream request should use the longer execution deadline instead of the fallback timeout")
	}
	if streamClient.Timeout != 0 {
		t.Fatalf("stream timeout = %s, want no client timeout when the execution context has a longer deadline", streamClient.Timeout)
	}
	if streamClient.Transport != client.Transport {
		t.Fatal("stream request did not preserve the configured transport")
	}
}

func TestStreamRequestClientKeepsFallbackWithoutLongDeadline(t *testing.T) {
	client := &http.Client{Timeout: 2 * time.Minute}
	if got := streamRequestClient(context.Background(), client); got != client {
		t.Fatal("stream request without a deadline should retain the fallback client timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if got := streamRequestClient(ctx, client); got != client {
		t.Fatal("a shorter execution deadline should keep the client fallback timeout")
	}
}

func TestModelHTTPClientHasStageTimeouts(t *testing.T) {
	client := newModelHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if client.Timeout != requestTimeout || transport.TLSHandshakeTimeout != 25*time.Second || transport.ResponseHeaderTimeout != 90*time.Second {
		t.Fatalf("client timeouts = total %s, TLS %s, headers %s", client.Timeout, transport.TLSHandshakeTimeout, transport.ResponseHeaderTimeout)
	}
	if transport.DialContext == nil {
		t.Fatal("model transport must set a bounded dialer")
	}
}
