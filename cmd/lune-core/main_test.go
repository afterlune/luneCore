package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/afterlune/luneCore"
	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/rpcserver"
	publicmodel "github.com/afterlune/luneCore/model"
	"github.com/coder/websocket"
)

type stdioChild struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Reader
	stderr  bytes.Buffer
	waited  bool
}

func TestParseWebOrigins(t *testing.T) {
	origins, err := parseWebOrigins(" https://app.example.test, http://localhost:3000 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(origins) != 2 || origins[0] != "https://app.example.test" || origins[1] != "http://localhost:3000" {
		t.Fatalf("parsed origins = %#v", origins)
	}
	for _, invalid := range []string{"*", "https://*.example.test", "https://app.example.test/path", "ftp://app.example.test", "https://user@app.example.test", "https://app.example.test,"} {
		if _, err := parseWebOrigins(invalid); err == nil {
			t.Errorf("parseWebOrigins(%q) succeeded", invalid)
		}
	}
}

func TestServeHTTPSContextCancellationClosesWSSListener(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	client, err := core.New(core.Config{
		Workspace: workspace, StateDir: stateDir,
		ModelConfig: &publicmodel.Config{
			Providers:      map[string]publicmodel.ProviderConfig{"fixture": {Protocol: "responses", BaseURL: "https://fixture.invalid", APIKey: "fixture-key"}},
			Profiles:       map[string]publicmodel.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.Shutdown(shutdownCtx); err != nil {
			t.Errorf("Core shutdown: %v", err)
		}
	})
	service, err := rpcserver.New(client, "https-token")
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePath := filepath.Join(t.TempDir(), "server.crt")
	keyPath := filepath.Join(t.TempDir(), "server.key")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(context.Background())
	defer cancelServe()
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveHTTPSListener(serveCtx, service, listener, certificatePath, keyPath) }()
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	defer transport.CloseIdleConnections()
	header := make(http.Header)
	header.Set("Authorization", "Bearer https-token")
	url := "wss://" + listener.Addr().String() + "/v1/runtime"
	var conn *websocket.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		dialCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		conn, _, err = websocket.Dial(dialCtx, url, &websocket.DialOptions{
			HTTPHeader: header, HTTPClient: &http.Client{Transport: transport},
		})
		cancel()
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial WSS service: %v", err)
	}
	defer conn.CloseNow()
	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelRequest()
	if err := conn.Write(requestCtx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":1,"method":"runtime.hello","params":{}}`)); err != nil {
		t.Fatalf("write runtime.hello: %v", err)
	}
	_, response, err := conn.Read(requestCtx)
	if err != nil {
		t.Fatalf("read runtime.hello: %v", err)
	}
	var reply struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &reply); err != nil || reply.Result.ProtocolVersion != "1" {
		t.Fatalf("runtime.hello response = %s, error=%v", response, err)
	}
	cancelServe()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serveHTTPSListener after cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTPS listener did not stop after context cancellation")
	}
}

func buildStandalone(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lune-core")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", path, ".")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build lune-core: %v\n%s", err, output)
	}
	return path
}

func startStdioChild(t *testing.T, binary, workspace, stateDir string) *stdioChild {
	t.Helper()
	command := exec.Command(binary, "stdio", "--workspace", workspace, "--state-dir", stateDir)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child := &stdioChild{command: command, input: input, output: bufio.NewReader(output)}
	command.Stderr = &child.stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !child.waited && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	return child
}

func (c *stdioChild) call(t *testing.T, id int, method string, params any, result any) {
	t.Helper()
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(c.input, "%s\n", request); err != nil {
		t.Fatalf("write %s request: %v", method, err)
	}
	line, err := c.output.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read %s response: %v (stderr: %s)", method, err, c.stderr.String())
	}
	var reply struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatalf("decode %s response %q: %v", method, line, err)
	}
	if reply.ID != id || len(reply.Error) != 0 {
		t.Fatalf("%s response = %s", method, line)
	}
	if result != nil {
		if err := json.Unmarshal(reply.Result, result); err != nil {
			t.Fatalf("decode %s result %s: %v", method, reply.Result, err)
		}
	}
}

func (c *stdioChild) stop(t *testing.T) {
	t.Helper()
	if err := c.input.Close(); err != nil && !strings.Contains(err.Error(), "file already closed") {
		t.Fatalf("close stdio input: %v", err)
	}
	drainDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, c.output)
		close(drainDone)
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- c.command.Wait() }()
	select {
	case err := <-waitDone:
		c.waited = true
		if err != nil {
			t.Fatalf("lune-core process exited with error: %v (stderr: %s)", err, c.stderr.String())
		}
	case <-time.After(15 * time.Second):
		_ = c.command.Process.Kill()
		t.Fatal("lune-core did not stop after stdio EOF")
	}
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("did not drain stdio output after shutdown")
	}
}

func (c *stdioChild) kill(t *testing.T) {
	t.Helper()
	_ = c.input.Close()
	drainDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, c.output)
		close(drainDone)
	}()
	if err := c.command.Process.Kill(); err != nil {
		t.Fatalf("kill lune-core process: %v", err)
	}
	if err := c.command.Wait(); err == nil {
		t.Fatal("forcibly terminated Core exited successfully")
	}
	c.waited = true
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("did not drain output from terminated process")
	}
}

func TestStandaloneStdioChildLocksAndReopensState(t *testing.T) {
	binary := buildStandalone(t)
	workspace, stateDir := t.TempDir(), t.TempDir()
	project := projectconfig.ProjectConfig{
		Models: publicmodel.Config{
			Providers:      map[string]publicmodel.ProviderConfig{"fixture": {Protocol: "responses", BaseURL: "https://fixture.invalid", APIKey: "fixture"}},
			Profiles:       map[string]publicmodel.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
		Runtime:    projectconfig.RuntimeConfig{AutoCompact: true},
		MCPServers: map[string]projectconfig.MCPServerConfig{},
	}
	configData, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, projectconfig.FileName), configData, 0600); err != nil {
		t.Fatal(err)
	}
	first := startStdioChild(t, binary, workspace, stateDir)
	var hello struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	first.call(t, 1, "runtime.hello", map[string]any{"clientName": "stdio-integration"}, &hello)
	if hello.ProtocolVersion != "1" {
		t.Fatalf("protocol version = %q", hello.ProtocolVersion)
	}
	var saved struct {
		ID string `json:"id"`
	}
	first.call(t, 2, "session.create", map[string]any{}, &saved)
	if saved.ID == "" {
		t.Fatal("session.create returned an empty ID")
	}

	second := exec.Command(binary, "stdio", "--workspace", workspace, "--state-dir", stateDir)
	second.Stdin = bytes.NewReader(nil)
	var secondStdout, secondStderr bytes.Buffer
	second.Stdout, second.Stderr = &secondStdout, &secondStderr
	if err := second.Run(); err == nil || !strings.Contains(secondStderr.String(), "already locked") {
		t.Fatalf("second Core process error=%v stderr=%q", err, secondStderr.String())
	}

	first.stop(t)
	restarted := startStdioChild(t, binary, workspace, stateDir)
	var sessions []struct {
		ID string `json:"id"`
	}
	restarted.call(t, 1, "session.list", map[string]any{"limit": 10}, &sessions)
	if len(sessions) != 1 || sessions[0].ID != saved.ID {
		t.Fatalf("sessions after process restart = %#v, want ID %q", sessions, saved.ID)
	}
	restarted.stop(t)

	forced := startStdioChild(t, binary, workspace, stateDir)
	var secondSaved struct {
		ID string `json:"id"`
	}
	forced.call(t, 1, "session.create", map[string]any{}, &secondSaved)
	forced.kill(t)
	afterKill := startStdioChild(t, binary, workspace, stateDir)
	var afterKillSessions []struct {
		ID string `json:"id"`
	}
	afterKill.call(t, 1, "session.list", map[string]any{"limit": 10}, &afterKillSessions)
	if len(afterKillSessions) != 2 {
		t.Fatalf("sessions after forced process exit = %#v", afterKillSessions)
	}
	afterKill.stop(t)

	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("state directory after process restart: %v", err)
	}
	t.Run("Python stdio example", func(t *testing.T) { runPythonStdioExample(t, binary) })
}

func TestStdioEOFPreservesActiveTurnBeforeJoiningRequests(t *testing.T) {
	binary := buildStandalone(t)
	entered := make(chan struct{}, 1)
	requestCanceled := make(chan struct{}, 1)
	var requests atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				_, _ = fmt.Fprint(w, ": waiting for shutdown\n\n")
				flusher.Flush()
			}
			entered <- struct{}{}
			select {
			case <-r.Context().Done():
				requestCanceled <- struct{}{}
			case <-time.After(6 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		payload := `{"type":"response.completed","sequence_number":2,"response":{"id":"resumed","object":"response","status":"completed","model":"fixture-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"resumed after restart"}]}]}}`
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", payload)
	}))
	defer modelServer.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	project := projectconfig.ProjectConfig{
		Models: publicmodel.Config{
			Providers:      map[string]publicmodel.ProviderConfig{"fixture": {Protocol: "responses", BaseURL: modelServer.URL, APIKey: "fixture"}},
			Profiles:       map[string]publicmodel.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
		Runtime:    projectconfig.RuntimeConfig{AutoCompact: true},
		MCPServers: map[string]projectconfig.MCPServerConfig{},
	}
	configData, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, projectconfig.FileName), configData, 0600); err != nil {
		t.Fatal(err)
	}

	first := startStdioChild(t, binary, workspace, stateDir)
	var saved struct {
		ID string `json:"id"`
	}
	first.call(t, 1, "session.create", map[string]any{}, &saved)
	var submitted struct {
		ExecutionID string `json:"executionId"`
	}
	first.call(t, 2, "turn.submit", map[string]any{
		"sessionId": saved.ID, "submissionId": "shutdown-submit", "input": "hold while stopping",
	}, &submitted)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first model request did not start")
	}
	waitRequest, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "turn.wait",
		"params": map[string]any{"sessionId": saved.ID, "executionId": submitted.ExecutionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(first.input, "%s\n", waitRequest); err != nil {
		t.Fatal(err)
	}
	var activeState struct {
		Task struct {
			ExecutionID string `json:"executionId"`
		} `json:"task"`
	}
	first.call(t, 4, "runtime.state", map[string]any{"sessionId": saved.ID}, &activeState)
	if activeState.Task.ExecutionID != submitted.ExecutionID {
		t.Fatalf("active task before EOF = %#v", activeState.Task)
	}
	var queued struct {
		ExecutionID string `json:"executionId"`
	}
	first.call(t, 5, "turn.submit", map[string]any{
		"sessionId": saved.ID, "submissionId": "shutdown-queued", "input": "wait behind active turn", "mode": "queue_input",
	}, &queued)
	time.Sleep(50 * time.Millisecond)
	first.stop(t)
	select {
	case <-requestCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("stdio EOF did not cancel the active model request")
	}

	reopened := startStdioChild(t, binary, workspace, stateDir)
	var state struct {
		Task struct {
			Status      string `json:"status"`
			ExecutionID string `json:"executionId"`
		} `json:"task"`
		Queue []struct {
			ExecutionID string `json:"executionId"`
			Status      string `json:"status"`
		} `json:"queue"`
	}
	reopened.call(t, 1, "runtime.state", map[string]any{"sessionId": saved.ID}, &state)
	if state.Task.Status != "paused" || state.Task.ExecutionID != submitted.ExecutionID {
		t.Fatalf("task after EOF restart = %#v", state.Task)
	}
	if len(state.Queue) != 2 || state.Queue[0].ExecutionID != submitted.ExecutionID || state.Queue[0].Status != "paused" ||
		state.Queue[1].ExecutionID != queued.ExecutionID || state.Queue[1].Status != "queued" {
		t.Fatalf("queue after EOF restart = %#v", state.Queue)
	}
	var recovered struct {
		ExecutionID string `json:"executionId"`
	}
	reopened.call(t, 2, "turn.recover", map[string]any{"sessionId": saved.ID}, &recovered)
	if recovered.ExecutionID != submitted.ExecutionID {
		t.Fatalf("recovered execution ID = %q, want %q", recovered.ExecutionID, submitted.ExecutionID)
	}
	var result struct {
		Status string `json:"status"`
	}
	reopened.call(t, 3, "turn.wait", map[string]any{"sessionId": saved.ID, "executionId": submitted.ExecutionID}, &result)
	if result.Status != "completed" {
		t.Fatalf("recovered turn status = %q", result.Status)
	}
	var queuedResult struct {
		Status string `json:"status"`
	}
	reopened.call(t, 4, "turn.wait", map[string]any{"sessionId": saved.ID, "executionId": queued.ExecutionID}, &queuedResult)
	if queuedResult.Status != "completed" {
		t.Fatalf("queued turn after EOF restart status = %q", queuedResult.Status)
	}
	reopened.stop(t)
}

func runPythonStdioExample(t *testing.T, binary string) {
	t.Helper()
	var python string
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil && exec.Command(path, "--version").Run() == nil {
			python = path
			break
		}
	}
	if python == "" {
		t.Skip("Python is not installed")
	}
	var requests atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		call := requests.Add(1)
		var output any
		if call == 1 {
			output = map[string]any{
				"type": "function_call", "id": "ask-item", "call_id": "ask-call", "name": "ask_user",
				"arguments": `{"questions":[{"id":"target","question":"Which target?","options":[{"label":"local","description":"Use the local environment"}]}]}`,
			}
		} else {
			output = map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "Answer accepted."}},
			}
		}
		payload, marshalErr := json.Marshal(map[string]any{
			"type": "response.completed", "sequence_number": 2,
			"response": map[string]any{
				"id": fmt.Sprintf("fixture-%d", call), "object": "response", "status": "completed", "model": "fixture-model",
				"output": []any{output},
			},
		})
		if marshalErr != nil {
			http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", payload)
	}))
	defer modelServer.Close()

	workspace := t.TempDir()
	project := projectconfig.ProjectConfig{
		Models: publicmodel.Config{
			Providers:      map[string]publicmodel.ProviderConfig{"fixture": {Protocol: "responses", BaseURL: modelServer.URL, APIKey: "fixture-key"}},
			Profiles:       map[string]publicmodel.ModelProfile{"fixture": {Provider: "fixture", Model: "fixture-model"}},
			DefaultProfile: "fixture",
		},
		Runtime:    projectconfig.RuntimeConfig{AutoCompact: true},
		MCPServers: map[string]projectconfig.MCPServerConfig{},
	}
	configData, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, projectconfig.FileName), configData, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	script := filepath.Join("..", "..", "examples", "runtime_stdio.py")
	command := exec.CommandContext(ctx, python, script, binary, workspace,
		"Ask which environment to use before responding.", "--answer", "local")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("run Python stdio example: %v\n%s", err, output)
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode Python example output %q: %v", output, err)
	}
	if result.Status != "completed" || requests.Load() < 2 {
		t.Fatalf("Python example result=%#v provider requests=%d", result, requests.Load())
	}
}
