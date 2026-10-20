//go:build linux && (amd64 || arm64)

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	commandruntime "github.com/afterlune/luneCore/internal/command"
	publictool "github.com/afterlune/luneCore/tool"
)

type backgroundCommandStore struct {
	mu      sync.Mutex
	records map[string]commandruntime.Record
}

func (s *backgroundCommandStore) SaveCommand(_ context.Context, record commandruntime.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.Output = append([]byte(nil), record.Output...)
	s.records[record.Info.SessionID+"/"+record.Info.ID] = record
	return nil
}

func (s *backgroundCommandStore) GetCommand(_ context.Context, sessionID, id string) (commandruntime.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[sessionID+"/"+id]
	if !ok {
		return commandruntime.Record{}, os.ErrNotExist
	}
	record.Output = append([]byte(nil), record.Output...)
	return record, nil
}

func (s *backgroundCommandStore) ListCommands(_ context.Context, sessionID, executionID string) ([]commandruntime.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var records []commandruntime.Record
	for _, record := range s.records {
		if (sessionID == "" || record.Info.SessionID == sessionID) && (executionID == "" || record.Info.ExecutionID == executionID) {
			record.Output = append([]byte(nil), record.Output...)
			records = append(records, record)
		}
	}
	return records, nil
}

func (s *backgroundCommandStore) InterruptCommands(context.Context) error { return nil }

func TestBackgroundSandboxServiceSurvivesTaskAndIsReachable(t *testing.T) {
	workspace := t.TempDir()
	serverSource := `package main
import (
	"net/http"
	"os"
)
func main() {
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	_ = http.ListenAndServe("127.0.0.1:"+os.Args[1], nil)
}
`
	if err := os.WriteFile(filepath.Join(workspace, "server.go"), []byte(serverSource), 0600); err != nil {
		t.Fatal(err)
	}
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("Go toolchain is unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	store := &backgroundCommandStore{records: map[string]commandruntime.Record{}}
	manager, err := commandruntime.New(context.Background(), store, SandboxedCommandExecutor{HelperPath: sandboxHelperForTest(t)}, publictool.CommandOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.OpenScope("first-execution", context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("close process manager: %v", err)
		}
	}()

	identity := publictool.CallIdentity{SessionID: "service-session", ExecutionID: "first-execution", AgentID: "lead", InvocationID: "start-service"}
	ctx := WithPermissionMode(publictool.WithCallIdentity(context.Background(), identity), string(PermissionModeWorkspace))
	command := fmt.Sprintf("%s run server.go %d", shellQuote(goPath), port)
	arguments, err := json.Marshal(map[string]any{"command": command, "background": true, "yield_time_ms": 1000})
	if err != nil {
		t.Fatal(err)
	}
	result := (CommandTool{Workspace: workspace, Manager: manager, CommandToolchainPaths: []string{filepath.Dir(goPath)}}).Run(ctx, arguments)
	if result.Err != nil || result.Details == nil || result.Details.Command == nil || !result.Details.Command.Command.Background {
		t.Fatalf("start service result = %#v", result)
	}
	commandID := result.Details.Command.Command.ID
	if commandID == "" || !result.Details.Command.Command.Status.Active() {
		t.Fatalf("start receipt = %#v", result.Details.Command.Command)
	}

	if err := manager.StopScope(context.Background(), "first-execution", ""); err != nil {
		t.Fatal(err)
	}
	if manager.Active("first-execution", "lead") {
		t.Fatal("session service blocked completion of its task")
	}
	endpoint := "http://127.0.0.1:" + strconv.Itoa(port) + "/health"
	if body := awaitHTTPBody(t, endpoint); body != "ok" {
		t.Fatalf("health response = %q, want ok", body)
	}

	if err := manager.OpenScope("next-execution", context.Background()); err != nil {
		t.Fatal(err)
	}
	identity.ExecutionID = "next-execution"
	identity.InvocationID = "stop-service"
	nextContext := publictool.WithCallIdentity(context.Background(), identity)
	status, err := manager.Read(nextContext, identity.SessionID, commandID, identity.ExecutionID, identity.AgentID, publictool.CommandReadOptions{})
	if err != nil || status.Command.Status != publictool.CommandRunning {
		t.Fatalf("cross-task service status = %#v, error = %v", status, err)
	}
	stopped, err := manager.Stop(nextContext, identity.SessionID, commandID, identity.ExecutionID, identity.AgentID)
	if err != nil || stopped.Command.Status != publictool.CommandCancelled {
		t.Fatalf("cross-task stop = %#v, error = %v", stopped, err)
	}
	awaitHTTPFailure(t, endpoint)
}

func awaitHTTPBody(t *testing.T, endpoint string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	client := http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK {
				return string(body)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("service did not answer at %s", endpoint)
	return ""
}

func awaitHTTPFailure(t *testing.T, endpoint string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		client := http.Client{Timeout: 100 * time.Millisecond}
		response, err := client.Get(endpoint)
		if err != nil {
			return
		}
		_ = response.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("service is still reachable after stop: %s", endpoint)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
