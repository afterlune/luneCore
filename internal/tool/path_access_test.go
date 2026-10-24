package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

func TestExternalFileToolsRequireScopedSessionGrants(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	file := filepath.Join(external, "notes.txt")
	if err := os.WriteFile(file, []byte("outside workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	readArgs := json.RawMessage(`{"path":` + quoteJSON(file) + `}`)
	reader := ReadFileTool{Workspace: workspace}
	grant, err := reader.PathAccessRequest(context.Background(), readArgs)
	if err != nil {
		t.Fatal(err)
	}
	if grant == nil || grant.Path != external || grant.Access != publictool.PathRead {
		t.Fatalf("read grant = %#v, want read access to %q", grant, external)
	}
	if result := reader.Run(context.Background(), readArgs); result.Err == nil {
		t.Fatal("external read succeeded without a grant")
	}
	readCtx := WithApprovedPathGrant(context.Background(), *grant)
	if result := reader.Run(readCtx, readArgs); result.Err != nil || !strings.Contains(result.Output, "outside workspace") {
		t.Fatalf("approved external read = %#v", result)
	}

	writeArgs := json.RawMessage(`{"path":` + quoteJSON(filepath.Join(external, "new.txt")) + `,"content":"new"}`)
	writer := WriteFileTool{Workspace: workspace}
	writeGrant, err := writer.PathAccessRequest(context.Background(), writeArgs)
	if err != nil {
		t.Fatal(err)
	}
	if writeGrant == nil || writeGrant.Access != publictool.PathReadWrite {
		t.Fatalf("write grant = %#v, want read_write", writeGrant)
	}
	if result := writer.Run(readCtx, writeArgs); result.Err == nil {
		t.Fatal("read grant allowed a write")
	}
	writeCtx := WithApprovedPathGrant(context.Background(), *writeGrant)
	if result := writer.Run(writeCtx, writeArgs); result.Err != nil {
		t.Fatalf("approved external write: %v", result.Err)
	}
}

func TestSessionPathGrantStoreCoversOnlyRequestedSubtreeAndAccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	nested := filepath.Join(root, "nested")
	store := NewSessionPathGrantStore([]publictool.PathGrant{{Path: root, Access: publictool.PathRead}})
	if !store.Allows(publictool.PathGrant{Path: filepath.Join(nested, "child"), Access: publictool.PathRead}) {
		t.Fatal("read grant did not cover a descendant")
	}
	if store.Allows(publictool.PathGrant{Path: nested, Access: publictool.PathReadWrite}) {
		t.Fatal("read grant allowed read-write access")
	}
	store.Add(publictool.PathGrant{Path: nested, Access: publictool.PathReadWrite})
	if !store.Allows(publictool.PathGrant{Path: filepath.Join(nested, "child"), Access: publictool.PathReadWrite}) ||
		!store.Allows(publictool.PathGrant{Path: filepath.Join(nested, "child"), Access: publictool.PathRead}) {
		t.Fatal("read-write grant did not cover descendant read and write")
	}
	if store.Allows(publictool.PathGrant{Path: filepath.Dir(root), Access: publictool.PathRead}) {
		t.Fatal("narrow directory grant covered its parent")
	}
}

func TestExternalPathApprovalsFollowPermissionModes(t *testing.T) {
	workspace, external := t.TempDir(), t.TempDir()
	readFile := filepath.Join(external, "read.txt")
	if err := os.WriteFile(readFile, []byte("readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeArgs := `{"path":` + quoteJSON(filepath.Join(external, "write.txt")) + `,"content":"written"}`
	tests := []struct {
		mode           PermissionMode
		readApprovals  int
		writeAllowed   bool
		writeApprovals int
	}{
		{mode: PermissionModeReadOnly, readApprovals: 1},
		{mode: PermissionModeAsk, readApprovals: 1, writeAllowed: true, writeApprovals: 1},
		{mode: PermissionModeWorkspace, readApprovals: 1, writeAllowed: true, writeApprovals: 1},
		{mode: PermissionModeFullAccess, writeAllowed: true},
	}
	for _, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			registry, err := NewDefaultRegistry(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			if err := registry.SetPermissionMode(test.mode); err != nil {
				t.Fatal(err)
			}
			var approvals []PermissionRequest
			registry.SetPermissionApprovalHandler(func(_ context.Context, request PermissionRequest) bool {
				approvals = append(approvals, request)
				return true
			})
			readResult := registry.Run(context.Background(), "read_file", json.RawMessage(`{"path":`+quoteJSON(readFile)+`}`))
			if readResult.Err != nil {
				t.Fatalf("external read: %v", readResult.Err)
			}
			if len(approvals) != test.readApprovals {
				t.Fatalf("read approvals = %d, want %d", len(approvals), test.readApprovals)
			}
			if test.readApprovals == 1 {
				request := approvals[0]
				if request.PathGrant == nil || request.PathGrant.Path != external || request.PathGrant.Access != publictool.PathRead {
					t.Fatalf("external read approval = %#v", request)
				}
			}
			approvals = nil
			writeResult := registry.Run(context.Background(), "write_file", json.RawMessage(writeArgs))
			if (writeResult.Err == nil) != test.writeAllowed {
				t.Fatalf("external write = %#v, want allowed=%v", writeResult, test.writeAllowed)
			}
			if len(approvals) != test.writeApprovals {
				t.Fatalf("write approvals = %d, want %d", len(approvals), test.writeApprovals)
			}
			if test.writeApprovals == 1 {
				request := approvals[0]
				if request.PathGrant == nil || request.PathGrant.Path != external || request.PathGrant.Access != publictool.PathReadWrite {
					t.Fatalf("external write approval = %#v", request)
				}
			}
		})
	}
}

func TestExternalDirectoryToolsUseApprovedDirectoryAndReturnScopedPaths(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "hit.txt"), []byte("needle here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args string
		tool PathAccessTool
		run  func(context.Context) Result
	}{
		{name: "list", args: `{"path":` + quoteJSON(external) + `}`, tool: ListDirTool{Workspace: workspace}, run: func(ctx context.Context) Result {
			return (ListDirTool{Workspace: workspace}).Run(ctx, json.RawMessage(`{"path":`+quoteJSON(external)+`}`))
		}},
		{name: "glob", args: `{"pattern":"**/*","path":` + quoteJSON(external) + `}`, tool: GlobTool{Workspace: workspace}, run: func(ctx context.Context) Result {
			return (GlobTool{Workspace: workspace}).Run(ctx, json.RawMessage(`{"pattern":"**/*","path":`+quoteJSON(external)+`}`))
		}},
		{name: "grep", args: `{"pattern":"needle","path":` + quoteJSON(external) + `}`, tool: GrepTool{Workspace: workspace}, run: func(ctx context.Context) Result {
			return (GrepTool{Workspace: workspace}).Run(ctx, json.RawMessage(`{"pattern":"needle","path":`+quoteJSON(external)+`}`))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			grant, err := test.tool.PathAccessRequest(context.Background(), json.RawMessage(test.args))
			if err != nil || grant == nil || grant.Path != external || grant.Access != publictool.PathRead {
				t.Fatalf("PathAccessRequest = %#v, %v", grant, err)
			}
			result := test.run(WithApprovedPathGrant(context.Background(), *grant))
			if result.Err != nil {
				t.Fatalf("Run: %v", result.Err)
			}
			if test.name != "list" && !strings.Contains(result.Output, filepath.Join(external, "hit.txt")) {
				t.Fatalf("output %q does not contain external result path", result.Output)
			}
		})
	}
}

func TestRelativeTraversalAndWorkspaceSymlinkEscapeAreRejected(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	secret := filepath.Join(external, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := ReadFileTool{Workspace: workspace}
	traversal := filepath.Join("..", filepath.Base(external), "secret.txt")
	if _, err := reader.PathAccessRequest(context.Background(), json.RawMessage(`{"path":`+quoteJSON(traversal)+`}`)); err == nil || !strings.Contains(err.Error(), "escapes the workspace") {
		t.Fatalf("relative traversal request error = %v", err)
	}
	if result := reader.Run(context.Background(), json.RawMessage(`{"path":`+quoteJSON(traversal)+`}`)); result.Err == nil || !strings.Contains(result.Err.Error(), "escapes the workspace") || !strings.Contains(result.Err.Error(), "session directory grant") {
		t.Fatalf("relative traversal result = %#v", result)
	}
	grant, err := reader.PathAccessRequest(context.Background(), json.RawMessage(`{"path":`+quoteJSON(secret)+`}`))
	if err != nil || grant == nil || grant.Path != external {
		t.Fatalf("absolute external path grant = %#v, error %v", grant, err)
	}
	link := filepath.Join(workspace, "outside-link")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	grant, err = (ListDirTool{Workspace: workspace}).PathAccessRequest(context.Background(), json.RawMessage(`{"path":"outside-link"}`))
	if err == nil || grant != nil {
		t.Fatalf("workspace symlink escape grant = %#v, error %v", grant, err)
	}
}

func TestReadImageToolReturnsBoundedMultimodalOutput(t *testing.T) {
	workspace := t.TempDir()
	imagePath := filepath.Join(workspace, "pixel.webp")
	data := []byte("RIFF\x04\x00\x00\x00WEBP")
	if err := os.WriteFile(imagePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := ReadImageTool{Workspace: workspace, MaxFileBytes: int64(len(data))}
	result := tool.Run(context.Background(), json.RawMessage(`{"path":"pixel.webp"}`))
	if result.Err != nil {
		t.Fatalf("read_image: %v", result.Err)
	}
	if result.ModelOutput == nil || len(result.ModelOutput.Parts) != 2 {
		t.Fatalf("ModelOutput = %#v", result.ModelOutput)
	}
	image, ok := result.ModelOutput.Parts[1].(model.InputImage)
	if !ok || !strings.HasPrefix(image.ImageURL, "data:image/webp;base64,") {
		t.Fatalf("image part = %#v", result.ModelOutput.Parts[1])
	}
	tool.MaxFileBytes = int64(len(data) - 1)
	if result := tool.Run(context.Background(), json.RawMessage(`{"path":"pixel.webp"}`)); result.Err == nil || !strings.Contains(result.Err.Error(), "attachment limit") {
		t.Fatalf("oversized image result = %#v", result)
	}
}

func quoteJSON(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
