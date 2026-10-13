package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	projectconfig "github.com/afterlune/luneCore/internal/config"
)

const (
	stdioDrainTimeout = time.Second
	stdioExitGrace    = 1500 * time.Millisecond
	stdioCloseTimeout = 3 * time.Second
)

type stdioResult struct {
	data json.RawMessage
	err  error
}

type stdioWrite struct {
	id   uint64
	body []byte
	ctx  context.Context
}

type stdioTransport struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	mu          sync.Mutex
	pending     map[uint64]chan stdioResult
	terminalErr error
	sequence    atomic.Uint64
	writes      chan stdioWrite
	closed      chan struct{}
	readerDone  chan struct{}
	writerDone  chan struct{}
	processDone chan struct{}
	waitErr     error
	closeOnce   sync.Once
	closeDone   chan struct{}
	closeErr    error
}

func startStdio(config projectconfig.MCPServerConfig, workspace string) (*stdioTransport, error) {
	cmd := exec.Command(config.Command, config.Args...)
	cmd.Dir = workspace
	cmd.Env = mergeEnvironment(os.Environ(), config.Env)
	cmd.Stderr = io.Discard
	configureMCPProcess(cmd)
	stdout, output, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("open MCP stdio output: %w", err)
	}
	cmd.Stdout = output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = stdout.Close()
		_ = output.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = output.Close()
		return nil, fmt.Errorf("start MCP stdio server: %w", err)
	}
	_ = output.Close()
	client := &stdioTransport{
		cmd: cmd, stdin: stdin, stdout: stdout,
		pending: make(map[uint64]chan stdioResult), writes: make(chan stdioWrite),
		closed: make(chan struct{}), readerDone: make(chan struct{}), writerDone: make(chan struct{}),
		processDone: make(chan struct{}), closeDone: make(chan struct{}),
	}
	go client.readLoop()
	go client.writeLoop()
	go client.waitProcess()
	return client, nil
}

func (client *stdioTransport) Alive() bool {
	if client == nil {
		return false
	}
	select {
	case <-client.closed:
		return false
	default:
	}
	select {
	case <-client.processDone:
		return false
	default:
		return true
	}
}

// complete removes a request before publishing its one and only result.
func (client *stdioTransport) complete(id uint64, result stdioResult) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if pending := client.pending[id]; pending != nil {
		delete(client.pending, id)
		pending <- result
	}
}

func (client *stdioTransport) stop(err error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.terminalErr != nil {
		return
	}
	client.terminalErr = err
	close(client.closed)
	for id, pending := range client.pending {
		delete(client.pending, id)
		pending <- stdioResult{err: err}
	}
}

func (client *stdioTransport) readLoop() {
	defer close(client.readerDone)
	scanner := bufio.NewScanner(client.stdout)
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	for scanner.Scan() {
		var response rpcResponse
		if json.Unmarshal(scanner.Bytes(), &response) != nil || len(response.ID) == 0 {
			continue
		}
		var id uint64
		if json.Unmarshal(response.ID, &id) != nil {
			continue
		}
		data, err := responseResult(response)
		client.complete(id, stdioResult{data: data, err: err})
	}
	err := errors.New("MCP stdio server closed its output")
	if scanner.Err() != nil {
		err = fmt.Errorf("read MCP stdio output: %w", scanner.Err())
	}
	client.stop(err)
	go client.Close()
}

func (client *stdioTransport) writeLoop() {
	defer close(client.writerDone)
	for {
		select {
		case <-client.closed:
			return
		case job := <-client.writes:
			if err := job.ctx.Err(); err != nil {
				client.complete(job.id, stdioResult{err: err})
				continue
			}
			client.mu.Lock()
			active := client.pending[job.id] != nil && client.terminalErr == nil
			client.mu.Unlock()
			if !active {
				continue
			}
			if _, err := client.stdin.Write(job.body); err != nil {
				client.stop(fmt.Errorf("write MCP stdio request: %w", err))
				go client.Close()
				return
			}
		}
	}
}

func (client *stdioTransport) waitProcess() {
	client.waitErr = client.cmd.Wait()
	close(client.processDone)
	// Wait never owns stdout: let the reader deliver buffered final responses.
	timer := time.NewTimer(stdioDrainTimeout)
	defer timer.Stop()
	select {
	case <-client.readerDone:
	case <-timer.C:
		_ = client.stdout.Close()
	}
}

func (client *stdioTransport) Request(ctx context.Context, method string, params map[string]any, _ string, _ []headerBinding) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := client.sequence.Add(1)
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: requestParams(maps.Clone(params))})
	if err != nil {
		return nil, err
	}
	pending := make(chan stdioResult, 1)
	client.mu.Lock()
	if !client.Alive() {
		client.mu.Unlock()
		return nil, errors.New("MCP stdio server is not running")
	}
	client.pending[id] = pending
	client.mu.Unlock()
	select {
	case client.writes <- stdioWrite{id: id, body: append(body, '\n'), ctx: ctx}:
	case result := <-pending:
		return result.data, result.err
	case <-ctx.Done():
		client.complete(id, stdioResult{err: ctx.Err()})
	case <-client.closed:
	}
	select {
	case result := <-pending:
		return result.data, result.err
	case <-ctx.Done():
		client.complete(id, stdioResult{err: ctx.Err()})
		result := <-pending
		return result.data, result.err
	}
}

func (client *stdioTransport) Close() error {
	return client.closeContext(context.Background())
}

func (client *stdioTransport) closeContext(ctx context.Context) error {
	if client == nil {
		return nil
	}
	client.closeOnce.Do(func() { go client.shutdown() })
	select {
	case <-client.closeDone:
		return client.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitUntil(done <-chan struct{}, deadline time.Time) bool {
	select {
	case <-done:
		return true
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (client *stdioTransport) shutdown() {
	defer close(client.closeDone)
	deadline := time.Now().Add(stdioCloseTimeout)
	client.stop(errors.New("MCP stdio transport is closed"))
	_ = client.stdin.Close()
	_ = client.stdout.Close()
	forced := false
	if !waitUntil(client.processDone, time.Now().Add(stdioExitGrace)) {
		forced = true
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		err := terminateMCPProcess(ctx, client.cmd)
		cancel()
		if err != nil && !errors.Is(err, os.ErrProcessDone) {
			client.closeErr = err
		}
	}
	if !waitUntil(client.processDone, deadline) {
		client.closeErr = errors.Join(client.closeErr, errors.New("MCP stdio process did not exit within shutdown deadline"))
	} else if !forced {
		client.closeErr = errors.Join(client.closeErr, client.waitErr)
	}
	if !waitUntil(client.readerDone, deadline) || !waitUntil(client.writerDone, deadline) {
		client.closeErr = errors.Join(client.closeErr, errors.New("MCP stdio I/O did not stop within shutdown deadline"))
	}
}

func mergeEnvironment(parent []string, overrides map[string]string) []string {
	merged := make(map[string]string, len(parent)+len(overrides))
	for _, entry := range parent {
		if key, value, ok := strings.Cut(entry, "="); ok {
			merged[key] = value
		}
	}
	for key, value := range overrides {
		for existing := range merged {
			if strings.EqualFold(existing, key) {
				delete(merged, existing)
			}
		}
		merged[key] = value
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(merged))
	for _, key := range keys {
		result = append(result, key+"="+merged[key])
	}
	return result
}
