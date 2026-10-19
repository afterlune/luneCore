package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func BenchmarkRegistryDefinitions(b *testing.B) {
	registry, err := NewDefaultRegistry(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer registry.Close()
	definitions := registry.Definitions()
	request := model.ModelRequest{Tools: definitions}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = registry.Definitions()
	}
	b.StopTimer()
	b.ReportMetric(float64(model.EstimateInputTokens(request)), "tokens/request")
	b.ReportMetric(float64(len(mustJSON(b, definitions))), "schema-bytes")
}

func BenchmarkRegistryDefinitionsForCategory(b *testing.B) {
	registry, err := NewDefaultRegistry(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer registry.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = registry.DefinitionsForCategory("files")
	}
}

func BenchmarkBuiltinFileTools(b *testing.B) {
	workspace := makeToolBenchmarkWorkspace(b)
	root, err := openWorkspaceRoot(workspace)
	if err != nil {
		b.Fatal(err)
	}
	identity := &workspaceIdentity{root: root}
	b.Cleanup(func() { _ = identity.closeRoot() })
	ctx := context.Background()
	readTool := ReadFileTool{Workspace: workspace, workspace: identity}
	listTool := ListDirTool{Workspace: workspace, workspace: identity}
	globTool := GlobTool{Workspace: workspace, workspace: identity}
	grepTool := GrepTool{Workspace: workspace, workspace: identity}
	writeTool := WriteFileTool{Workspace: workspace, workspace: identity}
	editTool := EditFileTool{Workspace: workspace, workspace: identity}

	bench := func(name string, run func() Result) {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if result := run(); result.Err != nil {
					b.Fatal(result.Err)
				}
			}
		})
	}
	bench("read_file", func() Result {
		return readTool.Run(ctx, json.RawMessage(`{"path":"000.go","start_line":1,"limit":100}`))
	})
	bench("list_dir", func() Result {
		return listTool.Run(ctx, json.RawMessage(`{"path":".","offset":0,"limit":100}`))
	})
	bench("glob", func() Result {
		return globTool.Run(ctx, json.RawMessage(`{"pattern":"**/*.go","offset":0,"limit":100}`))
	})
	bench("grep", func() Result {
		return grepTool.Run(ctx, json.RawMessage(`{"pattern":"needle","path":".","offset":0,"limit":100}`))
	})
	bench("write_file", func() Result {
		return writeTool.Run(ctx, json.RawMessage(`{"path":"written.txt","content":"benchmark content"}`))
	})
	b.Run("edit_file", func(b *testing.B) {
		if err := os.WriteFile(filepath.Join(workspace, "edit.txt"), []byte("before\n"), 0o600); err != nil {
			b.Fatal(err)
		}
		editArgs := [2]json.RawMessage{
			json.RawMessage(`{"path":"edit.txt","old_text":"before","new_text":"after"}`),
			json.RawMessage(`{"path":"edit.txt","old_text":"after","new_text":"before"}`),
		}
		editIndex := 0
		b.ReportAllocs()
		for range b.N {
			result := editTool.Run(ctx, editArgs[editIndex])
			editIndex ^= 1
			if result.Err != nil {
				b.Fatal(result.Err)
			}
		}
	})
}

func BenchmarkBuiltinCommandTool(b *testing.B) {
	workspace := b.TempDir()
	root, err := openWorkspaceRoot(workspace)
	if err != nil {
		b.Fatal(err)
	}
	identity := &workspaceIdentity{root: root}
	b.Cleanup(func() { _ = identity.closeRoot() })
	command := CommandTool{Workspace: workspace, workspace: identity, Executor: benchmarkCommandExecutor{}}
	args := json.RawMessage(`{"command":"echo benchmark"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if result := command.Run(context.Background(), args); result.Err != nil {
			b.Fatal(result.Err)
		}
	}
}

func BenchmarkBuiltinWebTools(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/lite/") {
			writer.Header().Set("Content-Type", "text/html")
			_, _ = writer.Write([]byte("<!doctype html><table><tr><td><a class='result-link' href='https://example.com/guide'>A guide</a></td></tr><tr><td class='result-snippet'>Useful text.</td></tr></table>"))
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte("<!doctype html><html><head><title>Guide</title></head><body><h1>Guide</h1><p>Useful text.</p></body></html>"))
	}))
	defer server.Close()
	search := WebSearchTool{httpClient: server.Client(), endpoint: server.URL + "/lite/"}
	fetch := WebFetchTool{httpClient: server.Client()}
	b.Run("web_search", func(b *testing.B) {
		args := json.RawMessage(`{"query":"benchmark","max_results":5}`)
		b.ReportAllocs()
		for range b.N {
			if result := search.Run(context.Background(), args); result.Err != nil {
				b.Fatal(result.Err)
			}
		}
	})
	b.Run("web_fetch", func(b *testing.B) {
		args, _ := json.Marshal(map[string]string{"url": server.URL + "/page"})
		b.ReportAllocs()
		for range b.N {
			if result := fetch.Run(context.Background(), args); result.Err != nil {
				b.Fatal(result.Err)
			}
		}
	})
}

type benchmarkCommandExecutor struct{}

func (benchmarkCommandExecutor) Execute(_ context.Context, request publictool.CommandRequest) (publictool.CommandResult, error) {
	code := 0
	return publictool.CommandResult{ExitCode: &code, Output: request.Command}, nil
}

func makeToolBenchmarkWorkspace(b *testing.B) string {
	b.Helper()
	workspace := b.TempDir()
	var contents strings.Builder
	for line := range 100 {
		fmt.Fprintf(&contents, "line %03d needle\n", line)
	}
	for file := range 100 {
		name := filepath.Join(workspace, fmt.Sprintf("%03d.go", file))
		if err := os.WriteFile(name, []byte(contents.String()), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "edit.txt"), []byte("before\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	return workspace
}

func mustJSON(b *testing.B, value any) []byte {
	b.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		b.Fatal(err)
	}
	return encoded
}
