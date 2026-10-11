package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebFetchToolExtractsHTMLText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><html><head><title>A &amp; title</title><script>head secret</script></head>
		<body><h1>Readable <b>page</b></h1><p>Text &amp; more.</p><script>body secret</script><style>hidden style</style></body></html>`)
	}))
	defer server.Close()

	result := (WebFetchTool{httpClient: server.Client()}).Run(
		context.Background(), json.RawMessage(`{"url":"`+server.URL+`/article"}`),
	)
	if result.Err != nil {
		t.Fatalf("web_fetch: %v", result.Err)
	}
	for _, want := range []string{"Title: A & title", "Readable page", "Text & more."} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("output %q does not contain %q", result.Output, want)
		}
	}
	for _, unwanted := range []string{"head secret", "body secret", "hidden style"} {
		if strings.Contains(result.Output, unwanted) {
			t.Errorf("output contains hidden content %q: %s", unwanted, result.Output)
		}
	}
}

func TestWebFetchToolSupportsTextAndJSONAndCapsOutput(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "plain text", contentType: "text/plain", body: "plain body"},
		{name: "json", contentType: "application/json", body: `{"ok":true}`},
		{name: "truncated text", contentType: "text/plain", body: strings.Repeat("界", maxWebTextRunes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				_, _ = fmt.Fprint(w, tt.body)
			}))
			defer server.Close()

			result := (WebFetchTool{httpClient: server.Client()}).Run(
				context.Background(), json.RawMessage(`{"url":"`+server.URL+`"}`),
			)
			if result.Err != nil {
				t.Fatalf("web_fetch: %v", result.Err)
			}
			if tt.name == "truncated text" {
				if !strings.Contains(result.Output, "[content truncated at 20000 characters]") {
					t.Fatalf("output was not marked as truncated")
				}
				return
			}
			if result.Output != tt.body {
				t.Fatalf("output = %q, want %q", result.Output, tt.body)
			}
		})
	}
}

func TestWebFetchToolRejectsInvalidURLsStatusesAndBinaryContent(t *testing.T) {
	if result := (WebFetchTool{}).Run(context.Background(), json.RawMessage(`{"url":"file:///tmp/secret"}`)); result.Err == nil {
		t.Fatal("expected non-HTTP URL to fail")
	}
	if result := (WebFetchTool{}).Run(context.Background(), json.RawMessage(`{"url":"http://user:pass@example.com/"}`)); result.Err == nil {
		t.Fatal("expected embedded credentials to fail")
	}

	for _, tt := range []struct {
		name        string
		status      int
		contentType string
		wantError   string
	}{
		{name: "not found", status: http.StatusNotFound, contentType: "text/html", wantError: "404"},
		{name: "pdf", status: http.StatusOK, contentType: "application/pdf", wantError: "unsupported content type"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprint(w, "body")
			}))
			defer server.Close()

			result := (WebFetchTool{httpClient: server.Client()}).Run(
				context.Background(), json.RawMessage(`{"url":"`+server.URL+`"}`),
			)
			if result.Err == nil || !strings.Contains(result.Err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want it to contain %q", result.Err, tt.wantError)
			}
		})
	}
}

func TestWebFetchToolRespectsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	result := (WebFetchTool{httpClient: &http.Client{Timeout: 20 * time.Millisecond}}).Run(
		context.Background(), json.RawMessage(`{"url":"`+server.URL+`"}`),
	)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "deadline exceeded") {
		t.Fatalf("error = %v, want request timeout", result.Err)
	}
}
