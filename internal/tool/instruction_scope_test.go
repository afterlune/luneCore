package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstructionDirectoriesForCall(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	subDir := filepath.Join(workspace, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	testFile := filepath.Join(subDir, "file.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := NewDefaultRegistry(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	// Read file
	args, _ := json.Marshal(map[string]string{"path": "sub/file.txt"})
	dirs, relevant, err := reg.InstructionDirectoriesForCall(ctx, "read_file", args)
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}
	if !relevant || len(dirs) == 0 {
		t.Fatalf("expected relevant directories for read_file, got %v, %v", dirs, relevant)
	}

	// List dir
	args, _ = json.Marshal(map[string]string{"path": "sub"})
	dirs, relevant, err = reg.InstructionDirectoriesForCall(ctx, "list_dir", args)
	if err != nil {
		t.Fatalf("list_dir failed: %v", err)
	}
	if !relevant || len(dirs) == 0 {
		t.Fatalf("expected relevant directories for list_dir, got %v, %v", dirs, relevant)
	}

	// Unknown or unscoped tool
	dirs, relevant, err = reg.InstructionDirectoriesForCall(ctx, "unknown_tool", json.RawMessage(`{}`))
	if err != nil || relevant || len(dirs) != 0 {
		t.Fatalf("expected unknown tool to be irrelevant, got %v, %v, %v", dirs, relevant, err)
	}
}

func TestCleanProjectInstructionPath(t *testing.T) {
	for _, path := range []string{"", ".", "../AGENTS.md", "src/../../AGENTS.md"} {
		if _, err := CleanProjectInstructionPath(path, false); err == nil {
			t.Errorf("CleanProjectInstructionPath(%q) unexpectedly succeeded", path)
		}
	}
	if got, err := CleanProjectInstructionPath(`src\nested`, true); err != nil || !strings.Contains(got, "src") {
		t.Fatalf("CleanProjectInstructionPath returned %q, %v", got, err)
	}
}

func TestProjectInstructionPathEscapeGuidance(t *testing.T) {
	workspace := t.TempDir()
	traversal := filepath.Join("..", "outside")
	_, _, err := ProjectInstructionPath(workspace, traversal, true)
	if err == nil {
		t.Fatal("expected traversal error, got nil")
	}
	expectedAbs := filepath.Clean(filepath.Join(workspace, traversal))
	if !strings.Contains(err.Error(), "escapes the workspace root") {
		t.Fatalf("error %q does not contain 'escapes the workspace root'", err.Error())
	}
	if !strings.Contains(err.Error(), filepath.Base(expectedAbs)) {
		t.Fatalf("error %q does not contain expected derived abs %q", err.Error(), expectedAbs)
	}
	if !strings.Contains(err.Error(), "session directory grant") {
		t.Fatalf("error %q does not contain guidance 'session directory grant'", err.Error())
	}
}

func TestProjectInstructionPathCrossVolumeAndOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()

	var crossTarget string
	if filepath.Separator == '\\' {
		vol := strings.ToUpper(filepath.VolumeName(workspace))
		targetVol := "D:"
		if vol == "D:" {
			targetVol = "C:"
		}
		crossTarget = targetVol + `\test\dir`
	} else {
		crossTarget = "/tmp/outside_workspace_test"
	}

	clean, inWorkspace, err := ProjectInstructionPath(workspace, crossTarget, true)
	if err != nil {
		t.Fatalf("ProjectInstructionPath with cross-target %q failed: %v", crossTarget, err)
	}
	if inWorkspace {
		t.Fatalf("expected inWorkspace=false for %q in workspace %q, got clean=%q", crossTarget, workspace, clean)
	}

	outsideSameDrive := filepath.Dir(workspace)
	clean, inWorkspace, err = ProjectInstructionPath(workspace, outsideSameDrive, true)
	if err != nil {
		t.Fatalf("ProjectInstructionPath with outside directory failed: %v", err)
	}
	if inWorkspace {
		t.Fatalf("expected inWorkspace=false for outside directory %q, got clean=%q", outsideSameDrive, clean)
	}

	reg, err := NewDefaultRegistry(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	args, _ := json.Marshal(map[string]string{"path": crossTarget})
	dirs, relevant, err := reg.InstructionDirectoriesForCall(context.Background(), "list_dir", args)
	if err != nil {
		t.Fatalf("InstructionDirectoriesForCall failed for crossTarget %q: %v", crossTarget, err)
	}
	if !relevant {
		t.Fatal("expected list_dir to still be relevant")
	}
	if len(dirs) != 0 {
		t.Fatalf("expected no workspace instruction directories for external path, got %v", dirs)
	}
}
