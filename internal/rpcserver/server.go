package rpcserver

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/internal/session"
	"github.com/coder/websocket"
)

const MaxMessageBytes = 32 << 20
const maxConcurrentRequests = 8
const maxServerRequests = 16
const maxSubscriptionsPerConnection = 16
const maxConnections = 256

const WebSocketSubprotocol = "lune-core.jsonrpc.v1"
const browserAuthSubprotocolPrefix = "lune-core-auth."

type ServerOptions struct {
	// AllowedOrigins adds browser origins allowed to connect cross-origin.
	// The WebSocket request host is always allowed.
	AllowedOrigins []string
}

type Server struct {
	core          *core.Core
	workspaceInfo os.FileInfo
	token         string
	origins       []string
	// writeTimeout is set only by package tests; production uses the protocol
	// default below.
	writeTimeout time.Duration

	mu                  sync.Mutex
	closed              bool
	clients             map[*websocket.Conn]context.CancelFunc
	interactiveSessions map[string]int
	requests            chan struct{}
	wg                  sync.WaitGroup
}

func New(client *core.Core, token string) (*Server, error) {
	return NewWithOptions(client, token, ServerOptions{})
}

func NewWithOptions(client *core.Core, token string, options ServerOptions) (*Server, error) {
	if client == nil {
		return nil, errors.New("core is required")
	}
	workspaceInfo, err := os.Stat(client.Workspace())
	if err != nil {
		return nil, err
	}
	return &Server{
		core: client, workspaceInfo: workspaceInfo, token: strings.TrimSpace(token), origins: append([]string(nil), options.AllowedOrigins...),
		clients:             make(map[*websocket.Conn]context.CancelFunc),
		interactiveSessions: make(map[string]int),
		requests:            make(chan struct{}, maxServerRequests),
	}, nil
}

func (s *Server) retainInteractiveSession(sessionID string, runtime *core.SessionRuntime) {
	if s == nil || sessionID == "" || runtime == nil {
		return
	}
	s.mu.Lock()
	if s.interactiveSessions == nil {
		s.interactiveSessions = make(map[string]int)
	}
	s.interactiveSessions[sessionID]++
	first := s.interactiveSessions[sessionID] == 1
	s.mu.Unlock()
	if first {
		runtime.SetUserInteractionAvailable(true)
	}
}

func (s *Server) releaseInteractiveSession(sessionID string, runtime *core.SessionRuntime) {
	if s == nil || sessionID == "" || runtime == nil {
		return
	}
	s.mu.Lock()
	count := s.interactiveSessions[sessionID]
	last := count <= 1
	if last {
		delete(s.interactiveSessions, sessionID)
	} else {
		s.interactiveSessions[sessionID] = count - 1
	}
	s.mu.Unlock()
	if last {
		runtime.SetUserInteractionAvailable(false)
	}
}

// ServeStdio owns the pipe endpoints until the connection is closed.
func (s *Server) ServeStdio(ctx context.Context, input io.ReadCloser, output io.WriteCloser) error {
	if ctx == nil || input == nil || output == nil {
		return errors.New("context, input, and output are required")
	}
	client := newConnection(s, ctx)
	pipesClosed := make(chan struct{})
	context.AfterFunc(client.ctx, func() {
		defer close(pipesClosed)
		_ = input.Close()
		_ = output.Close()
	})
	writerDone := client.startWriter(func(ctx context.Context, data []byte) error {
		// Closing the owned endpoints also interrupts a pipe without native
		// deadline support.
		stopTimeout := context.AfterFunc(ctx, client.cancel)
		defer stopTimeout()
		if _, err := output.Write(data); err != nil {
			return err
		}
		_, err := io.WriteString(output, "\n")
		return err
	})
	defer func() {
		_ = s.core.BeginShutdown()
		client.cancel()
		client.join()
		<-writerDone
		<-pipesClosed
	}()
	reader := bufio.NewReaderSize(input, 64*1024)
	for {
		line, err := readLineLimited(reader, MaxMessageBytes)
		if err != nil {
			if client.ctx.Err() != nil || errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if !client.handleAsync(line) {
			return nil
		}
	}
}

func readLineLimited(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		part, prefix, err := reader.ReadLine()
		if len(line)+len(part) > limit {
			return nil, errors.New("IPC message exceeds maximum size")
		}
		line = append(line, part...)
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
		if !prefix {
			return line, nil
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path != "/v1/runtime" {
		http.NotFound(w, r)
		return
	}
	if s.token == "" {
		http.Error(w, "remote authentication is not configured", http.StatusServiceUnavailable)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="lune-core"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	connectionLimitReached := len(s.clients) >= maxConnections
	s.mu.Unlock()
	if connectionLimitReached {
		http.Error(w, "connection limit reached", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{WebSocketSubprotocol}, OriginPatterns: s.origins,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(MaxMessageBytes)
	client := newConnection(s, r.Context())
	defer client.cancel()
	s.mu.Lock()
	if s.closed || len(s.clients) >= maxConnections {
		s.mu.Unlock()
		return
	}
	s.clients[conn] = client.cancel
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		s.mu.Unlock()
		s.wg.Done()
	}()
	writerDone := client.startWriter(func(ctx context.Context, data []byte) error {
		return conn.Write(ctx, websocket.MessageText, data)
	})
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-client.ctx.Done():
				return
			case <-ticker.C:
				pingCtx, cancelPing := context.WithTimeout(client.ctx, s.writeDeadline())
				err := conn.Ping(pingCtx)
				cancelPing()
				if err != nil {
					client.cancel()
					return
				}
			}
		}
	}()
	for {
		messageType, message, err := conn.Read(client.ctx)
		if err != nil {
			break
		}
		if messageType == websocket.MessageText && !client.handleAsync(message) {
			break
		}
	}
	client.cancel()
	_ = conn.CloseNow()
	client.join()
	<-writerDone
	<-pingDone
}

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	provided := ""
	const bearerPrefix = "Bearer "
	if header := r.Header.Get("Authorization"); strings.HasPrefix(header, bearerPrefix) {
		provided = strings.TrimSpace(strings.TrimPrefix(header, bearerPrefix))
	}
	if len(provided) == len(s.token) && subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) == 1 {
		return true
	}
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, protocol := range strings.Split(header, ",") {
			protocol = strings.TrimSpace(protocol)
			if !strings.HasPrefix(protocol, browserAuthSubprotocolPrefix) {
				continue
			}
			encoded := strings.TrimPrefix(protocol, browserAuthSubprotocolPrefix)
			decoded, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil || len(decoded) != len(s.token) {
				continue
			}
			if subtle.ConstantTimeCompare(decoded, []byte(s.token)) == 1 {
				return true
			}
		}
	}
	return false
}

// Shutdown stops admission and joins the handlers for upgraded connections.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	s.mu.Lock()
	s.closed = true
	beginErr := s.core.BeginShutdown()
	for _, cancel := range s.clients {
		cancel()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return beginErr
	case <-ctx.Done():
		return errors.Join(beginErr, ctx.Err())
	}
}

type subscription struct {
	cancel  context.CancelFunc
	runtime *core.SessionRuntime
	ready   chan struct{}
	done    chan struct{}
}

type subscriptionReady struct {
	result any
	ready  chan struct{}
}

type connection struct {
	server *Server
	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.Mutex
	subscriptions map[string]*subscription
	requests      chan struct{}
	requestWG     sync.WaitGroup
	eventWG       sync.WaitGroup

	sendMu      sync.Mutex
	sendQueue   chan []byte
	sending     int
	outstanding int
}

const maxSendMessages = 64
const writeTimeout = 5 * time.Second

func (s *Server) writeDeadline() time.Duration {
	if s != nil && s.writeTimeout > 0 {
		return s.writeTimeout
	}
	return writeTimeout
}

func newConnection(server *Server, parent context.Context) *connection {
	ctx, cancel := context.WithCancel(parent)
	return &connection{
		server: server, ctx: ctx, cancel: cancel,
		subscriptions: make(map[string]*subscription),
		requests:      make(chan struct{}, maxConcurrentRequests),
		sendQueue:     make(chan []byte, maxSendMessages),
	}
}

func (c *connection) startWriter(write func(context.Context, []byte) error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-c.ctx.Done():
				return
			case data := <-c.sendQueue:
				if c.ctx.Err() != nil {
					return
				}
				ctx, cancel := context.WithTimeout(c.ctx, c.server.writeDeadline())
				err := write(ctx, data)
				cancel()
				c.sendMu.Lock()
				c.sending--
				c.outstanding -= len(data)
				c.sendMu.Unlock()
				if err != nil {
					c.cancel()
					return
				}
			}
		}
	}()
	return done
}

// send includes the in-flight message in both limits. Producers never block
// the reader or Core waiting for a client to consume its output.
func (c *connection) send(data []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if c.sending == maxSendMessages || len(data) > MaxMessageBytes-c.outstanding {
		c.cancel()
		return errors.New("runtime connection send queue is full")
	}
	c.sending++
	c.outstanding += len(data)
	c.sendQueue <- data
	return nil
}

func (c *connection) handleAsync(message []byte) bool {
	if c.ctx.Err() != nil {
		return false
	}
	select {
	case c.requests <- struct{}{}:
	default:
		return c.rejectOverload(message)
	}
	select {
	case c.server.requests <- struct{}{}:
	default:
		<-c.requests
		return c.rejectOverload(message)
	}
	c.requestWG.Add(1)
	go func() {
		defer c.requestWG.Done()
		defer func() { <-c.requests }()
		defer func() { <-c.server.requests }()
		c.handle(message)
	}()
	return true
}

func (c *connection) rejectOverload(message []byte) bool {
	var req request
	if json.Unmarshal(message, &req) == nil && len(req.ID) != 0 && validRequestID(req.ID) {
		c.respond(req.ID, nil, &rpcError{Code: -32003, Message: "runtime request limit reached"})
	}
	return c.ctx.Err() == nil
}

func (c *connection) join() {
	c.requestWG.Wait()
	c.closeSubscriptions()
}

func (c *connection) closeSubscriptions() {
	c.mu.Lock()
	subscriptions := make([]*subscription, 0, len(c.subscriptions))
	for _, sub := range c.subscriptions {
		subscriptions = append(subscriptions, sub)
		sub.cancel()
	}
	c.subscriptions = make(map[string]*subscription)
	c.mu.Unlock()
	for _, sub := range subscriptions {
		<-sub.done
		_ = sub.runtime.Close(context.Background())
	}
	c.eventWG.Wait()
}

func (c *connection) handle(data []byte) {
	var req request
	if !json.Valid(data) {
		c.respond(rawID(nil), nil, &rpcError{Code: -32700, Message: "parse error"})
		return
	}
	if err := json.Unmarshal(data, &req); err != nil {
		c.respond(rawID(nil), nil, &rpcError{Code: -32600, Message: "invalid request"})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" || !validRequestID(req.ID) {
		c.respond(rawID(req.ID), nil, &rpcError{Code: -32600, Message: "invalid request"})
		return
	}
	if len(data) > MaxMessageBytes {
		c.respond(req.ID, nil, &rpcError{Code: -32600, Message: "request exceeds maximum size"})
		return
	}
	result, rpcErr := c.dispatch(req.Method, req.Params)
	ready, hasReady := result.(subscriptionReady)
	if len(req.ID) == 0 {
		if hasReady {
			close(ready.ready)
		}
		return
	}
	if hasReady {
		c.respond(req.ID, ready.result, rpcErr)
		close(ready.ready)
		return
	}
	c.respond(req.ID, result, rpcErr)
}

func validRequestID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(id)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	switch value.(type) {
	case nil, string, json.Number:
		return true
	default:
		return false
	}
}

func (c *connection) respond(id json.RawMessage, result any, rpcErr *rpcError) {
	reply := response{JSONRPC: "2.0", ID: id}
	if rpcErr != nil {
		reply.Error = rpcErr
	} else {
		encoded, err := json.Marshal(result)
		if err != nil {
			reply.Error = &rpcError{Code: -32603, Message: "internal error"}
		} else {
			reply.Result = encoded
		}
	}
	data, err := json.Marshal(reply)
	if err == nil && len(data) > MaxMessageBytes {
		reply = response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: -32002, Message: "response exceeds maximum size", Data: map[string]string{"code": "response_too_large"}}}
		data, err = json.Marshal(reply)
	}
	if err == nil {
		_ = c.send(data)
	}
}

func (c *connection) notify(method string, params any) error {
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	return c.send(data)
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func rawID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func decodeParams(raw json.RawMessage, target any) *rpcError {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return &rpcError{Code: -32602, Message: "invalid params", Data: err.Error()}
	}
	return nil
}

func (c *connection) withRuntime(ctx context.Context, sessionID string, action func(*core.SessionRuntime) (any, error)) (any, *rpcError) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, &rpcError{Code: -32602, Message: "sessionId is required"}
	}
	runtime, err := c.server.core.OpenSessionRuntime(ctx, sessionID)
	if err != nil {
		return nil, applicationError(err)
	}
	defer runtime.Close(context.Background())
	result, err := action(runtime)
	if err != nil {
		return nil, applicationError(err)
	}
	return result, nil
}

func applicationError(err error) *rpcError {
	if err == nil {
		return nil
	}
	code := "runtime_error"
	switch {
	case errors.Is(err, session.ErrSubmissionConflict):
		code = "submission_conflict"
	case errors.Is(err, core.ErrSessionBusy):
		code = "session_busy"
	case errors.Is(err, core.ErrTaskPaused):
		code = "task_paused"
	case errors.Is(err, core.ErrTurnNotFound):
		code = "turn_not_found"
	case errors.Is(err, core.ErrSessionInUse):
		code = "session_in_use"
	case errors.Is(err, core.ErrQueuedTurnStarted):
		code = "queued_turn_started"
	case errors.Is(err, core.ErrPlanUnavailable):
		code = "plan_unavailable"
	case errors.Is(err, core.ErrPlanOutdated):
		code = "plan_outdated"
	case errors.Is(err, core.ErrMCPServerLifecycleChanged):
		code = "mcp_lifecycle_changed"
	case errors.Is(err, core.ErrModelRetryNotReady):
		code = "model_retry_not_ready"
	case errors.Is(err, core.ErrToolInvocationPersistence):
		code = "tool_invocation_persistence"
	case errors.Is(err, core.ErrUserInteractionUnsupported):
		code = "user_interaction_unsupported"
	case errors.Is(err, core.ErrAuthorizationRequired):
		code = "authorization_required"
	case errors.Is(err, core.ErrCredentialNotFound):
		code = "credential_not_found"
	}
	return &rpcError{Code: -32000, Message: err.Error(), Data: map[string]string{"code": code}}
}

func (c *connection) readParams(raw json.RawMessage, value any) *rpcError {
	if err := decodeParams(raw, value); err != nil {
		return err
	}
	return nil
}

var _ http.Handler = (*Server)(nil)
