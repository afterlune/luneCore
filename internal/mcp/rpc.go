package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
)

const protocolVersion = "2026-07-28"

const requestTimeout = 5 * time.Minute

type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      uint64         `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type transport interface {
	Request(context.Context, string, map[string]any, string, []headerBinding) (json.RawMessage, error)
	Close() error
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func requestParams(params map[string]any) map[string]any {
	if params == nil {
		params = make(map[string]any)
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion": protocolVersion,
		"io.modelcontextprotocol/clientInfo":      clientInfo{Name: "Lune Agent", Version: "0.1.0"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{
			"elicitation": map[string]any{"form": map[string]any{}, "url": map[string]any{}},
		},
	}
	return params
}

func responseResult(response rpcResponse) (json.RawMessage, error) {
	if response.Error != nil {
		return nil, fmt.Errorf("MCP request failed (%d): %s", response.Error.Code, response.Error.Message)
	}
	if len(response.Result) == 0 || !json.Valid(response.Result) {
		return nil, errors.New("MCP server returned an invalid response")
	}
	return append(json.RawMessage(nil), response.Result...), nil
}

var ErrAuthorizationRequired = errors.New("MCP server requires authorization")

type httpTransport struct {
	endpoint          string
	serverName        string
	clientMetadataURL string
	client            *http.Client
	browser           BrowserOpener
	credentials       CredentialStorage
	sequence          atomic.Uint64
	oauthMu           sync.Mutex
	oauth             *oauthRecord
	oauthLoaded       bool
}

func newHTTPTransport(serverName string, config projectconfig.MCPServerConfig, browser BrowserOpener, credentials CredentialStorage) *httpTransport {
	if browser == nil {
		browser = SystemBrowserOpener{}
	}
	if credentials == nil {
		credentials = KeyringStorage{}
	}
	return &httpTransport{
		endpoint: config.URL, serverName: serverName, clientMetadataURL: config.ClientMetadataURL,
		browser: browser, credentials: credentials,
		client: &http.Client{Timeout: requestTimeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

func (client *httpTransport) Request(ctx context.Context, method string, params map[string]any, name string, bindings []headerBinding) (json.RawMessage, error) {
	id := client.sequence.Add(1)
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: requestParams(params)})
	if err != nil {
		return nil, err
	}
	var token *oauthRecord
	client.oauthMu.Lock()
	if client.oauthLoaded && client.oauth != nil {
		copy := *client.oauth
		token = &copy
	}
	client.oauthMu.Unlock()
	response, err := client.post(ctx, body, method, name, bindings, params, token)
	if err != nil {
		return nil, fmt.Errorf("MCP HTTP request failed: %w", err)
	}
	if response.StatusCode == http.StatusUnauthorized {
		_ = response.Body.Close()
		if token == nil {
			token, err = client.loadOAuthRecord()
			if err != nil {
				return nil, fmt.Errorf("load MCP OAuth credentials from the system keyring: %w", err)
			}
		}
		if token == nil {
			return nil, fmt.Errorf("%w: server %q requires authorization", ErrAuthorizationRequired, client.serverName)
		}
		if token.ExpiresAt > 0 && time.Now().Unix() >= token.ExpiresAt {
			token, err = client.refresh(ctx, token)
			if err != nil {
				return nil, fmt.Errorf("refresh MCP OAuth credentials: %w", err)
			}
		}
		response, err = client.post(ctx, body, method, name, bindings, params, token)
		if err != nil {
			return nil, fmt.Errorf("retry authorized MCP request: %w", err)
		}
		if response.StatusCode == http.StatusUnauthorized && token.RefreshToken != "" {
			_ = response.Body.Close()
			token, err = client.refresh(ctx, token)
			if err != nil {
				return nil, fmt.Errorf("refresh MCP OAuth credentials: %w", err)
			}
			response, err = client.post(ctx, body, method, name, bindings, params, token)
			if err != nil {
				return nil, fmt.Errorf("retry refreshed MCP request: %w", err)
			}
		}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(response.Header.Get("WWW-Authenticate")), "insufficient_scope") {
			return nil, fmt.Errorf("MCP server requires additional OAuth scopes; run /mcp auth %s", client.serverName)
		}
		return nil, fmt.Errorf("MCP HTTP server returned status %d", response.StatusCode)
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	switch {
	case strings.Contains(contentType, "application/json"):
		var rpc rpcResponse
		if err := json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(&rpc); err != nil {
			return nil, fmt.Errorf("decode MCP HTTP response: %w", err)
		}
		if !rpcIDMatches(rpc.ID, id) {
			return nil, errors.New("MCP HTTP server returned a mismatched JSON-RPC ID")
		}
		return responseResult(rpc)
	case strings.Contains(contentType, "text/event-stream"):
		return readSSE(io.LimitReader(response.Body, 32<<20), id)
	default:
		return nil, fmt.Errorf("MCP HTTP server returned unsupported content type %q", contentType)
	}
}

func (client *httpTransport) post(ctx context.Context, body []byte, method, name string, bindings []headerBinding, params map[string]any, token *oauthRecord) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", protocolVersion)
	request.Header.Set("Mcp-Method", method)
	if name != "" {
		request.Header.Set("Mcp-Name", encodeHeaderValue(name))
	}
	if token != nil && token.AccessToken != "" {
		request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}
	if method == "tools/call" {
		var args map[string]any
		_ = json.Unmarshal(mustJSON(params["arguments"]), &args)
		for _, binding := range bindings {
			value, ok := argumentAt(args, binding.path)
			if !ok || value == nil {
				continue
			}
			encoded, ok := encodeHeaderParameter(value, binding.valueType)
			if !ok {
				return nil, fmt.Errorf("MCP tool argument at %s cannot be encoded as an HTTP header", strings.Join(binding.path, "."))
			}
			request.Header.Set("Mcp-Param-"+textproto.CanonicalMIMEHeaderKey(binding.headerName), encoded)
		}
	}
	return client.client.Do(request)
}

func readSSE(reader io.Reader, requestID uint64) (json.RawMessage, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	var data strings.Builder
	var final json.RawMessage
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				var response rpcResponse
				if json.Unmarshal([]byte(data.String()), &response) == nil && len(response.ID) > 0 {
					if !rpcIDMatches(response.ID, requestID) {
						data.Reset()
						continue
					}
					result, err := responseResult(response)
					if err != nil {
						return nil, err
					}
					final = result
				}
				data.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read MCP SSE response: %w", err)
	}
	if data.Len() > 0 {
		var response rpcResponse
		if json.Unmarshal([]byte(data.String()), &response) == nil && len(response.ID) > 0 {
			if !rpcIDMatches(response.ID, requestID) {
				return nil, errors.New("MCP SSE server returned a mismatched JSON-RPC ID")
			}
			return responseResult(response)
		}
	}
	if len(final) == 0 {
		return nil, errors.New("MCP SSE stream ended without a final response")
	}
	return final, nil
}

func rpcIDMatches(raw json.RawMessage, expected uint64) bool {
	var id uint64
	return json.Unmarshal(raw, &id) == nil && id == expected
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func encodeHeaderValue(value string) string {
	plain := true
	for _, char := range []byte(value) {
		if char != '\t' && (char < 0x20 || char > 0x7e) {
			plain = false
			break
		}
	}
	if strings.TrimSpace(value) != value || strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		plain = false
	}
	if plain {
		return value
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

func (client *httpTransport) Close() error { return nil }
