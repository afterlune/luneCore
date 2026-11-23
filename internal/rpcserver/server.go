package rpcserver

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

type Server struct {
	core  *core.Core
	token string

	mu       sync.Mutex
	closed   bool
	clients  map[*websocket.Conn]context.CancelFunc
	requests chan struct{}
	wg       sync.WaitGroup
}

func New(client *core.Core, token string) (*Server, error) {
	if client == nil {
		return nil, errors.New("core is required")
	}
	return &Server{
		core: client, token: strings.TrimSpace(token),
		clients:  make(map[*websocket.Conn]context.CancelFunc),
		requests: make(chan struct{}, maxServerRequests),
	}, nil
}

func (s *Server) ServeStdio(ctx context.Context, input io.Reader, output io.Writer) error {
	if ctx == nil || input == nil || output == nil {
		return errors.New("context, input, and output are required")
	}
	writeMu := sync.Mutex{}
	write := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := output.Write(data); err != nil {
			return err
		}
		_, err := io.WriteString(output, "\n")
		return err
	}
	readDone := make(chan struct{})
	defer close(readDone)
	if closer, ok := input.(io.Closer); ok {
		go func() {
			select {
			case <-ctx.Done():
				_ = closer.Close()
			case <-readDone:
			}
		}()
	}
	reader := bufio.NewReaderSize(input, 64*1024)
	conn := newConnection(s, ctx, write)
	var requests sync.WaitGroup
	for {
		line, err := readLineLimited(reader, MaxMessageBytes)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if errors.Is(err, io.EOF) {
				break
			}
			conn.cancel()
			requests.Wait()
			conn.closeSubscriptions()
			return err
		}
		if !conn.handleAsync(&requests, line) {
			break
		}
	}
	conn.cancel()
	requests.Wait()
	conn.closeSubscriptions()
	return nil
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
	if s.token != "" {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		provided := ""
		if strings.HasPrefix(header, prefix) {
			provided = strings.TrimSpace(strings.TrimPrefix(header, prefix))
		}
		if len(provided) != len(s.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="lune-core"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	s.mu.Lock()
	connectionLimitReached := len(s.clients) >= maxConnections
	s.mu.Unlock()
	if connectionLimitReached {
		http.Error(w, "connection limit reached", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(MaxMessageBytes)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close(websocket.StatusGoingAway, "server is shutting down")
		return
	}
	s.clients[conn] = cancel
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		s.mu.Unlock()
		s.wg.Done()
	}()
	writeMu := sync.Mutex{}
	write := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.Write(ctx, websocket.MessageText, data)
	}
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
				writeMu.Lock()
				err := conn.Ping(pingCtx)
				writeMu.Unlock()
				cancelPing()
				if err != nil {
					cancel()
					_ = conn.CloseNow()
					return
				}
			}
		}
	}()
	client := newConnection(s, ctx, write)
	var requests sync.WaitGroup
	for {
		messageType, message, err := conn.Read(ctx)
		if err != nil {
			break
		}
		if messageType != websocket.MessageText {
			continue
		}
		if !client.handleAsync(&requests, message) {
			break
		}
	}
	client.cancel()
	cancel()
	requests.Wait()
	client.closeSubscriptions()
	<-pingDone
}

// Shutdown rejects new WSS connections and closes connected clients so their
// event subscriptions release session handles before Core closes its stores.
func (s *Server) Shutdown() {
	s.mu.Lock()
	s.closed = true
	connections := make([]*websocket.Conn, 0, len(s.clients))
	for conn, cancel := range s.clients {
		cancel()
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.CloseNow()
	}
	s.wg.Wait()
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
	write  func([]byte) error

	mu            sync.Mutex
	subscriptions map[string]*subscription
	requests      chan struct{}
	eventWG       sync.WaitGroup
}

func newConnection(server *Server, parent context.Context, write func([]byte) error) *connection {
	ctx, cancel := context.WithCancel(parent)
	return &connection{server: server, ctx: ctx, cancel: cancel, write: write, subscriptions: make(map[string]*subscription), requests: make(chan struct{}, maxConcurrentRequests)}
}

func (c *connection) handleAsync(requests *sync.WaitGroup, message []byte) bool {
	select {
	case c.requests <- struct{}{}:
	case <-c.ctx.Done():
		return false
	}
	select {
	case c.server.requests <- struct{}{}:
	case <-c.ctx.Done():
		<-c.requests
		return false
	}
	requests.Add(1)
	go func() {
		defer requests.Done()
		defer func() { <-c.requests }()
		defer func() { <-c.server.requests }()
		c.handle(message)
	}()
	return true
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
		_ = c.write(data)
	}
}

func (c *connection) notify(method string, params any) error {
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	if len(data) > MaxMessageBytes {
		return errors.New("notification exceeds maximum message size")
	}
	return c.write(data)
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
