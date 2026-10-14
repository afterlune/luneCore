package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/agentgraph"
	"github.com/afterlune/luneCore/internal/model"
	runtimeTool "github.com/afterlune/luneCore/internal/tool"
)

func TestProjectInstructionsLoadForBuiltInFileToolScopeAndCache(t *testing.T) {
	workspace := t.TempDir()
	for directory, contents := range map[string]string{
		".":             "root guidance",
		"src":           "source guidance",
		"src/nested":    "nested guidance",
		"src/unrelated": "unrelated guidance",
	} {
		path := filepath.Join(workspace, filepath.FromSlash(directory), "AGENTS.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "src", "nested", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := runtimeTool.NewDefaultRegistry(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	client := &Core{registry: registry}
	ctx := context.Background()

	root, err := client.initialProjectInstructions(ctx)
	if err != nil {
		t.Fatalf("load root instructions: %v", err)
	}
	if len(root) != 1 || root[0].Path != "AGENTS.md" || root[0].Content != "root guidance" {
		t.Fatalf("initial root snapshot = %#v", root)
	}

	additions, err := client.projectInstructionsForTool(ctx, agentgraph.State{ProjectInstructions: root}, model.OutputFunctionCall{
		Name: "read_file", Arguments: `{"path":"src/nested/main.go"}`,
	})
	if err != nil {
		t.Fatalf("load nested instructions: %v", err)
	}
	if len(additions) != 2 || additions[0].Path != "src/AGENTS.md" || additions[0].Content != "source guidance" ||
		additions[1].Path != "src/nested/AGENTS.md" || additions[1].Content != "nested guidance" {
		t.Fatalf("nested instruction snapshot = %#v", additions)
	}
	if err := os.Symlink(filepath.Join(workspace, "src", "nested"), filepath.Join(workspace, "alias")); err == nil {
		aliasInstructions, err := client.projectInstructionsForTool(ctx, agentgraph.State{}, model.OutputFunctionCall{
			Name: "read_file", Arguments: `{"path":"alias/main.go"}`,
		})
		if err != nil {
			t.Fatalf("load instructions through workspace symlink: %v", err)
		}
		foundTargetGuidance := false
		for _, instruction := range aliasInstructions {
			foundTargetGuidance = foundTargetGuidance || instruction.Path == "src/nested/AGENTS.md" && instruction.Content == "nested guidance"
		}
		if !foundTargetGuidance {
			t.Fatalf("resolved symlink target guidance missing: %#v", aliasInstructions)
		}
	} else {
		t.Logf("symlink support unavailable; skipped alias-path assertion: %v", err)
	}

	state := agentgraph.State{
		ProjectInstructionsReady: true,
		ProjectInstructions:      append(append([]agentgraph.ProjectInstruction(nil), root...), additions...),
	}
	checkpoint, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var resumed agentgraph.State
	if err := json.Unmarshal(checkpoint, &resumed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "src", "nested", "AGENTS.md"), []byte("changed after checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	replayed, err := client.projectInstructionsForTool(ctx, resumed, model.OutputFunctionCall{
		Name: "read_file", Arguments: `{"path":"src/nested/main.go"}`,
	})
	if err != nil || len(replayed) != 0 {
		t.Fatalf("cached instruction lookup = %#v, error=%v; want no reload", replayed, err)
	}
}

func TestProjectInstructionsUseFileToolTargetsAndIgnoreShell(t *testing.T) {
	workspace := t.TempDir()
	for _, directory := range []string{"src", "src/nested"} {
		if err := os.MkdirAll(filepath.Join(workspace, filepath.FromSlash(directory)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workspace, filepath.FromSlash(directory), "AGENTS.md"), []byte(directory+" rules"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "src", "nested", "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := runtimeTool.NewDefaultRegistry(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	client := &Core{registry: registry}
	ctx := context.Background()
	state := agentgraph.State{}

	tests := []struct {
		name      string
		call      model.OutputFunctionCall
		wantPaths []string
	}{
		{name: "directory listing", call: model.OutputFunctionCall{Name: "list_dir", Arguments: `{"path":"src"}`}, wantPaths: []string{"src/AGENTS.md"}},
		{name: "recursive grep", call: model.OutputFunctionCall{Name: "grep", Arguments: `{"path":"src","pattern":"main"}`}, wantPaths: []string{"src/AGENTS.md", "src/nested/AGENTS.md"}},
		{name: "glob result", call: model.OutputFunctionCall{Name: "glob", Arguments: `{"pattern":"src/**/*.go"}`}, wantPaths: []string{"src/AGENTS.md", "src/nested/AGENTS.md"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := client.projectInstructionsForTool(ctx, state, test.call)
			if err != nil {
				t.Fatalf("projectInstructionsForTool: %v", err)
			}
			for _, path := range test.wantPaths {
				found := false
				for _, instruction := range got {
					found = found || instruction.Path == path
				}
				if !found {
					t.Errorf("instruction %q missing from %#v", path, got)
				}
			}
		})
	}

	got, err := client.projectInstructionsForTool(ctx, state, model.OutputFunctionCall{Name: "command", Arguments: `{"command":"cat src/main.go"}`})
	if err != nil || len(got) != 0 {
		t.Fatalf("shell instruction lookup = %#v, error=%v; want no nested instructions", got, err)
	}
	_, err = client.projectInstructionsForTool(ctx, state, model.OutputFunctionCall{Name: "write_file", Arguments: `{"path":"../outside","content":"x"}`})
	if err == nil {
		t.Fatal("path traversal passed instruction scope validation")
	}

	if _, err := client.projectInstructionsForTool(ctx, state, model.OutputFunctionCall{Name: "glob", Arguments: `{"pattern":"../**/*.go"}`}); err == nil {
		t.Fatal("glob traversal passed instruction scope validation")
	}
}

func TestProjectInstructionsRecordMissingRootFile(t *testing.T) {
	registry, err := runtimeTool.NewDefaultRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	client := &Core{registry: registry}
	instructions, err := client.initialProjectInstructions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(instructions) != 1 || instructions[0].Path != "AGENTS.md" || instructions[0].Content != "" {
		t.Fatalf("missing root instruction marker = %#v", instructions)
	}
}

func TestProjectInstructionsRejectOversizedFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), make([]byte, maxProjectInstructionBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := runtimeTool.NewDefaultRegistry(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	client := &Core{registry: registry}
	if _, err := client.initialProjectInstructions(context.Background()); err == nil {
		t.Fatal("oversized root instructions were accepted")
	}
}

func TestProjectInstructionPathValidation(t *testing.T) {
	for _, path := range []string{"", ".", "../AGENTS.md", "src/../../AGENTS.md"} {
		if _, err := cleanProjectInstructionPath(path, false); err == nil {
			t.Errorf("cleanProjectInstructionPath(%q) unexpectedly succeeded", path)
		}
	}
	if got, err := cleanProjectInstructionPath(`src\nested`, true); err != nil || !strings.Contains(got, "src") {
		t.Fatalf("cleanProjectInstructionPath returned %q, %v", got, err)
	}
}
