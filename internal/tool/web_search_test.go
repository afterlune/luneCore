package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebSearchToolParsesDuckDuckGoLiteResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/lite/" {
			t.Errorf("request = %s %s, want GET /lite/", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); got != "weather & forecast" {
			t.Errorf("query = %q, want decoded search query", got)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><table>
			<tr><td><a class='result-link' href='https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fguide&amp;rut=abc'>A <b>guide</b></a></td></tr>
			<tr><td class='result-snippet'>Useful &amp; <b>clear</b> description.</td></tr>
			<tr><td><a class='result-link' href='https://duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2F&amp;rut=def'>Second result</a></td></tr>
			<tr><td class='result-snippet'>Second snippet.</td></tr>
		</table>`)
	}))
	defer server.Close()

	result := (WebSearchTool{httpClient: server.Client(), endpoint: server.URL + "/lite/"}).Run(
		context.Background(), json.RawMessage(`{"query":"weather & forecast","max_results":1}`),
	)
	if result.Err != nil {
		t.Fatalf("web_search: %v", result.Err)
	}
	var results []webSearchResult
	if err := json.Unmarshal([]byte(result.Output), &results); err != nil {
		t.Fatalf("decode results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1: %s", len(results), result.Output)
	}
	if results[0].Title != "A guide" || results[0].URL != "https://example.com/guide" || results[0].Snippet != "Useful & clear description." {
		t.Fatalf("unexpected result: %#v", results[0])
	}
}

func TestWebSearchToolValidatesArgumentsAndHTTPStatus(t *testing.T) {
	for _, raw := range []string{
		`{"query":"topic","max_results":0}`,
		`{"query":"topic","max_results":11}`,
		`{"query":"  ","max_results":5}`,
	} {
		if result := (WebSearchTool{}).Run(context.Background(), json.RawMessage(raw)); result.Err == nil {
			t.Errorf("web_search(%s) should fail", raw)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	result := (WebSearchTool{httpClient: server.Client(), endpoint: server.URL}).Run(
		context.Background(), json.RawMessage(`{"query":"topic","max_results":5}`),
	)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "503") {
		t.Fatalf("error = %v, want HTTP 503", result.Err)
	}
}

func TestWebSearchToolReturnsEmptyArrayWhenNoResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "<html><body>No results</body></html>")
	}))
	defer server.Close()

	result := (WebSearchTool{httpClient: server.Client(), endpoint: server.URL}).Run(
		context.Background(), json.RawMessage(`{"query":"nothing","max_results":5}`),
	)
	if result.Err != nil {
		t.Fatalf("web_search: %v", result.Err)
	}
	if result.Output != "[]" {
		t.Fatalf("output = %q, want []", result.Output)
	}
}
