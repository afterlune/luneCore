package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	projectconfig "github.com/afterlune/luneCore/internal/config"
)

func TestHTTPTransportAcceptsRequestScopedSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		var message testRPCRequest
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(writer, ": keep-alive\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[]}}\n\n", message.ID)
	}))
	defer server.Close()
	client := newHTTPTransport("sse", projectconfig.MCPServerConfig{URL: server.URL})
	defer client.Close()
	response, err := client.Request(context.Background(), "tools/list", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(response, &result); err != nil || len(result.Tools) != 0 {
		t.Fatalf("SSE result = %s, error = %v", response, err)
	}
}

func TestHTTPTransportRejectsMismatchedResponseID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"jsonrpc":"2.0","id":999,"result":{}}`)
	}))
	defer server.Close()
	client := newHTTPTransport("mismatch", projectconfig.MCPServerConfig{URL: server.URL})
	defer client.Close()
	if _, err := client.Request(context.Background(), "tools/list", nil, "", nil); err == nil {
		t.Fatal("mismatched JSON-RPC ID was accepted")
	}
}
