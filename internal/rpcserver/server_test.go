package rpcserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/afterlune/luneCore"
	publicmodel "github.com/afterlune/luneCore/model"
	"github.com/coder/websocket"
)

type rpcBarrierProvider struct {
	entered   chan struct{}
	release   chan struct{}
	cancelled chan struct{}
}

func (*rpcBarrierProvider) Name() string { return "fixture" }

func (p *rpcBarrierProvider) CreateResponse(ctx context.Context, _ publicmodel.ModelRequest, _ publicmodel.ModelProfile) (*publicmodel.ModelResponse, error) {
	p.entered <- struct{}{}
	select {
	case <-p.release:
		return rpcTextResponse(), nil
	case <-ctx.Done():
		select {
		case p.cancelled <- struct{}{}:
		default:
		}
		return nil, ctx.Err()
	}
}

func (p *rpcBarrierProvider) StreamResponse(ctx context.Context, request publicmodel.ModelRequest, profile publicmodel.ModelProfile, emit func(publicmodel.ModelStreamEvent) error) (*publicmodel.ModelResponse, error) {
	response, err := p.CreateResponse(ctx, request, profile)
	if err == nil && emit != nil {
		err = emit(publicmodel.ModelStreamEvent{Type: publicmodel.ModelStreamCompleted, Response: response})
	}
	return response, err
}

func rpcTextResponse() *publicmodel.ModelResponse {
	return &publicmodel.ModelResponse{Status: "completed", Output: []publicmodel.ResponseOutputItem{
		publicmodel.OutputMessage{Role: publicmodel.RoleAssistant, Content: []publicmodel.OutputContentPart{publicmodel.OutputText{Text: "fixture response"}}},
	}}
}

type testRPCClient struct {
	conn          *websocket.Conn
	ctx           context.Context
	mu            sync.Mutex
	nextID        int
	notifications []json.RawMessage
}

type testRPCReply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

func dialTestRPC(t *testing.T, serverURL, token string, cert *x509.Certificate) *testRPCClient {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	header := make(http.Header)
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	conn, _, err := websocket.Dial(context.Background(), "wss"+strings.TrimPrefix(serverURL, "https")+"/v1/runtime", &websocket.DialOptions{
		HTTPHeader: header,
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}},
	})
	if err != nil {
		t.Fatalf("dial WSS runtime: %v", err)
	}
	clientCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return &testRPCClient{conn: conn, ctx: clientCtx}
}

func (c *testRPCClient) close() { _ = c.conn.CloseNow() }

func (c *testRPCClient) call(t *testing.T, method string, params any) testRPCReply {
	t.Helper()
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.conn.Write(c.ctx, websocket.MessageText, request); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	for {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			t.Fatalf("read %s response: %v", method, err)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Fatalf("decode %s response: %v", method, err)
		}
		if _, ok := envelope["method"]; ok {
			c.mu.Lock()
			c.notifications = append(c.notifications, append(json.RawMessage(nil), data...))
			c.mu.Unlock()
			continue
		}
		var reply testRPCReply
		if err := json.Unmarshal(data, &reply); err != nil {
			t.Fatal(err)
		}
		var gotID int
		if err := json.Unmarshal(reply.ID, &gotID); err != nil || gotID != id {
			t.Fatalf("%s response id = %s, want %d", method, reply.ID, id)
		}
		return reply
	}
}

func (c *testRPCClient) drainNotifications() []json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := c.notifications
	c.notifications = nil
	return items
}

func TestWSSessionsDisconnectReplayAndShutdown(t *testing.T) {
	provider := &rpcBarrierProvider{entered: make(chan struct{}, 4), release: make(chan struct{}, 4), cancelled: make(chan struct{}, 4)}
	client, err := core.New(core.Config{
		Workspace: t.TempDir(), StateDir: t.TempDir(), Memory: core.MemoryConfig{Disabled: true},
		ModelConfig: &publicmodel.Config{
			Providers:      map[string]publicmodel.ProviderConfig{"fixture": {Protocol: "responses"}},
			Profiles:       map[string]publicmodel.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
		ModelProviders: map[string]publicmodel.Provider{"fixture": provider},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	service, err := New(client, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewTLSServer(service)
	t.Cleanup(httpServer.Close)
	certificate := httpServer.Certificate()

	unauthorizedHeader := make(http.Header)
	_, unauthorizedResponse, err := websocket.Dial(context.Background(), "wss"+strings.TrimPrefix(httpServer.URL, "https")+"/v1/runtime", &websocket.DialOptions{
		HTTPHeader: unauthorizedHeader,
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: certificatePool(certificate)}}},
	})
	if err == nil || unauthorizedResponse == nil || unauthorizedResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized WSS dial = response %v, error %v", unauthorizedResponse, err)
	}

	first := dialTestRPC(t, httpServer.URL, "test-token", certificate)
	second := dialTestRPC(t, httpServer.URL, "test-token", certificate)
	t.Cleanup(first.close)
	t.Cleanup(second.close)
	for _, rpc := range []*testRPCClient{first, second} {
		if reply := rpc.call(t, "runtime.hello", map[string]any{"clientName": "integration-test"}); reply.Error != nil {
			t.Fatalf("runtime.hello: %#v", reply.Error)
		}
	}

	var firstSession, secondSession struct {
		ID string `json:"id"`
	}
	decodeResult(t, first.call(t, "session.create", map[string]any{}), &firstSession)
	decodeResult(t, second.call(t, "session.create", map[string]any{}), &secondSession)
	if firstSession.ID == "" || secondSession.ID == "" || firstSession.ID == secondSession.ID {
		t.Fatalf("session IDs = %q, %q", firstSession.ID, secondSession.ID)
	}
	if reply := second.call(t, "runtime.subscribe", map[string]any{"sessionId": secondSession.ID, "subscriptionId": "second-session", "afterCursor": 0}); reply.Error != nil {
		t.Fatalf("subscribe response: %#v", reply.Error)
	}
	initialNotifications := second.drainNotifications()
	if len(initialNotifications) != 0 {
		t.Fatalf("notification arrived before subscription acknowledgement: %s", initialNotifications[0])
	}

	firstSubmit := map[string]any{"sessionId": firstSession.ID, "submissionId": "first-submit", "input": "first frontend turn"}
	var firstTurn struct {
		ExecutionID string `json:"executionId"`
	}
	decodeResult(t, first.call(t, "turn.submit", firstSubmit), &firstTurn)
	awaitProviderCalls(t, provider.entered, 1)
	var retriedTurn struct {
		ExecutionID string `json:"executionId"`
	}
	decodeResult(t, first.call(t, "turn.submit", firstSubmit), &retriedTurn)
	if retriedTurn.ExecutionID != firstTurn.ExecutionID {
		t.Fatalf("idempotent retry execution = %q, want %q", retriedTurn.ExecutionID, firstTurn.ExecutionID)
	}
	firstSubmit["input"] = "conflicting payload"
	conflict := first.call(t, "turn.submit", firstSubmit)
	if conflict.Error == nil || !strings.Contains(string(conflict.Error.Data), "submission_conflict") {
		t.Fatalf("conflicting retry error = %#v", conflict.Error)
	}

	var secondTurn struct {
		ExecutionID string `json:"executionId"`
	}
	decodeResult(t, second.call(t, "turn.submit", map[string]any{
		"sessionId": secondSession.ID, "submissionId": "second-submit", "input": "second frontend turn",
	}), &secondTurn)
	awaitProviderCalls(t, provider.entered, 1)
	foreignWait := second.call(t, "turn.wait", map[string]any{"sessionId": secondSession.ID, "executionId": firstTurn.ExecutionID})
	if foreignWait.Error == nil || !strings.Contains(string(foreignWait.Error.Data), "turn_not_found") {
		t.Fatalf("cross-session turn.wait error = %#v", foreignWait.Error)
	}
	sharedSession := second.call(t, "session.get", map[string]any{"sessionId": firstSession.ID})
	if sharedSession.Error != nil {
		t.Fatalf("second frontend could not access shared Core session: %#v", sharedSession.Error)
	}

	first.close()
	select {
	case <-provider.cancelled:
		t.Fatal("disconnect canceled an admitted turn")
	case <-time.After(100 * time.Millisecond):
	}
	provider.release <- struct{}{}
	provider.release <- struct{}{}
	var secondResult struct {
		Status string `json:"status"`
	}
	decodeResult(t, second.call(t, "turn.wait", map[string]any{"sessionId": secondSession.ID, "executionId": secondTurn.ExecutionID}), &secondResult)
	if secondResult.Status != "completed" {
		t.Fatalf("second session result status = %q", secondResult.Status)
	}

	reconnected := dialTestRPC(t, httpServer.URL, "test-token", certificate)
	t.Cleanup(reconnected.close)
	var firstResult struct {
		Status string `json:"status"`
	}
	decodeResult(t, reconnected.call(t, "turn.wait", map[string]any{"sessionId": firstSession.ID, "executionId": firstTurn.ExecutionID}), &firstResult)
	if firstResult.Status != "completed" {
		t.Fatalf("disconnected session result status = %q", firstResult.Status)
	}
	if reply := reconnected.call(t, "runtime.subscribe", map[string]any{"sessionId": firstSession.ID, "subscriptionId": "replay", "afterCursor": 0}); reply.Error != nil {
		t.Fatalf("replay subscribe: %#v", reply.Error)
	}
	if got := reconnected.drainNotifications(); len(got) != 0 {
		t.Fatalf("replay event arrived before subscription acknowledgement: %s", got[0])
	}
	if !readReplayComplete(t, reconnected, firstTurn.ExecutionID) {
		t.Fatal("reconnected client did not replay the completed turn before the replay marker")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("RPC service shutdown: %v", err)
	}
	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Core shutdown: %v", err)
	}
}

func TestConnectionSendQueueIsBoundedAndWriterJoins(t *testing.T) {
	server := &Server{requests: make(chan struct{}, maxServerRequests)}
	client := newConnection(server, context.Background())
	for range maxSendMessages {
		if err := client.send([]byte("message")); err != nil {
			t.Fatalf("send %d: %v", len(client.sendQueue), err)
		}
	}
	if err := client.send([]byte("overflow")); err == nil {
		t.Fatal("send queue admitted a 65th outstanding message")
	}
	select {
	case <-client.ctx.Done():
	default:
		t.Fatal("send queue overflow did not close the connection context")
	}

	byteBounded := newConnection(server, context.Background())
	if err := byteBounded.send(make([]byte, MaxMessageBytes)); err != nil {
		t.Fatalf("send maximum-size message: %v", err)
	}
	if err := byteBounded.send([]byte("overflow")); err == nil {
		t.Fatal("send queue admitted more than 32 MiB outstanding")
	}

	writerClient := newConnection(server, context.Background())
	writerStarted := make(chan struct{})
	writerDone := writerClient.startWriter(func(ctx context.Context, _ []byte) error {
		close(writerStarted)
		<-ctx.Done()
		return ctx.Err()
	})
	if err := writerClient.send([]byte("blocked")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writerStarted:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	writerClient.cancel()
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("writer did not join after connection cancellation")
	}
}

func TestSlowWSSClientDoesNotBlockHealthyConnection(t *testing.T) {
	client, err := core.New(core.Config{
		Workspace: t.TempDir(), StateDir: t.TempDir(), Memory: core.MemoryConfig{Disabled: true},
		ModelConfig: &publicmodel.Config{
			Providers:      map[string]publicmodel.ProviderConfig{"fixture": {Protocol: "responses", BaseURL: "https://fixture.invalid", APIKey: "fixture-key"}},
			Profiles:       map[string]publicmodel.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	sessionHistory, err := json.Marshal(strings.Repeat("x", 20<<20))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := client.CreateSession(context.Background(), sessionHistory)
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(client, "slow-client-token")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewTLSServer(service)
	t.Cleanup(httpServer.Close)
	certificate := httpServer.Certificate()
	slow := dialTestRPC(t, httpServer.URL, "slow-client-token", certificate)
	healthy := dialTestRPC(t, httpServer.URL, "slow-client-token", certificate)
	t.Cleanup(slow.close)
	t.Cleanup(healthy.close)
	for id := 1; id <= 2; id++ {
		request, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "session.history", "params": map[string]any{"sessionId": saved.ID},
		})
		if err := slow.conn.Write(slow.ctx, websocket.MessageText, request); err != nil {
			t.Fatalf("write slow-client request %d: %v", id, err)
		}
	}
	var hello map[string]any
	decodeResult(t, healthy.call(t, "runtime.hello", map[string]any{}), &hello)
	if hello["protocolVersion"] != "1" {
		t.Fatalf("healthy client hello = %#v", hello)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		service.mu.Lock()
		connections := len(service.clients)
		service.mu.Unlock()
		if connections == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slow connection remained active after overflowing its send budget (connections=%d)", connections)
		}
		time.Sleep(10 * time.Millisecond)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("RPC service shutdown: %v", err)
	}
	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Core shutdown: %v", err)
	}
}

func awaitProviderCalls(t *testing.T, entered <-chan struct{}, count int) {
	t.Helper()
	for range count {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("model provider did not start the expected request")
		}
	}
}

func decodeResult(t *testing.T, reply testRPCReply, target any) {
	t.Helper()
	if reply.Error != nil {
		t.Fatalf("RPC error: code=%d message=%q data=%s", reply.Error.Code, reply.Error.Message, reply.Error.Data)
	}
	if err := json.Unmarshal(reply.Result, target); err != nil {
		t.Fatalf("decode RPC result %s: %v", reply.Result, err)
	}
}

func readReplayComplete(t *testing.T, client *testRPCClient, executionID string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(client.ctx, 5*time.Second)
	defer cancel()
	finished := false
	for {
		notification := client.nextNotification(t, ctx)
		var envelope struct {
			Params struct {
				Event json.RawMessage `json:"event"`
			} `json:"params"`
		}
		if err := json.Unmarshal(notification, &envelope); err != nil {
			t.Fatal(err)
		}
		var event struct {
			Kind        string `json:"kind"`
			ExecutionID string `json:"executionId"`
		}
		if err := json.Unmarshal(envelope.Params.Event, &event); err != nil {
			t.Fatal(err)
		}
		if event.Kind == "task_finished" && event.ExecutionID == executionID {
			finished = true
		}
		if event.Kind == "runtime_replay_complete" {
			return finished
		}
	}
}

func (c *testRPCClient) nextNotification(t *testing.T, ctx context.Context) json.RawMessage {
	t.Helper()
	for {
		c.mu.Lock()
		if len(c.notifications) > 0 {
			data := c.notifications[0]
			c.notifications = c.notifications[1:]
			c.mu.Unlock()
			return data
		}
		c.mu.Unlock()
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			t.Fatalf("read runtime notification: %v", err)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Fatal(err)
		}
		if _, ok := envelope["method"]; !ok {
			t.Fatalf("unexpected RPC response while waiting for notification: %s", data)
		}
		return append(json.RawMessage(nil), data...)
	}
}

func certificatePool(certificate *x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return pool
}
