package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	projectconfig "github.com/afterlune/luneCore/internal/config"
	publicmodel "github.com/afterlune/luneCore/model"
)

type stdioChild struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Reader
	stderr  bytes.Buffer
	waited  bool
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

	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("state directory after process restart: %v", err)
	}
	t.Run("Python stdio example", func(t *testing.T) { runPythonStdioExample(t, binary) })
}

func runPythonStdioExample(t *testing.T, binary string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
	}
	if err != nil {
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
