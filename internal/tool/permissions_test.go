package tool

import (
	"context"
	"errors"
	"encoding/json"
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
)

type permissionTestTool struct {
	name  string
	calls int
}

func (t *permissionTestTool) Definition() model.FunctionTool {
	return model.FunctionTool{
		Name:       t.name,
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`),
	}
}

func (t *permissionTestTool) Run(context.Context, json.RawMessage) Result {
	t.calls++
	return Result{Output: "ran"}
}

func TestPermissionModesAuthorizeToolsAsDesigned(t *testing.T) {
	metadata := map[string]model.ToolMetadata{
		"read_file":   {PermissionClass: model.ToolPermissionWorkspaceRead, ReadOnly: true},
		"write_file":  {PermissionClass: model.ToolPermissionWorkspaceWrite},
		"command":     {PermissionClass: model.ToolPermissionCommand},
		"web_fetch":   {PermissionClass: model.ToolPermissionNetwork, ReadOnly: true},
		"custom_tool": {},
	}
	tests := []struct {
		name          string
		mode          PermissionMode
		allowed       map[string]bool
		needsApproval map[string]bool
	}{
		{
			name:          "read-only",
			mode:          PermissionModeReadOnly,
			allowed:       map[string]bool{"read_file": true, "web_fetch": true, "custom_tool": true},
			needsApproval: map[string]bool{"web_fetch": true, "custom_tool": true},
		},
		{
			name:          "workspace",
			mode:          PermissionModeWorkspace,
			allowed:       map[string]bool{"read_file": true, "write_file": true, "command": true, "web_fetch": true, "custom_tool": true},
			needsApproval: map[string]bool{"command": true, "web_fetch": true, "custom_tool": true},
		},
		{
			name:          "auto",
			mode:          PermissionModeAuto,
			allowed:       map[string]bool{"read_file": true, "write_file": true, "command": true, "web_fetch": true, "custom_tool": true},
			needsApproval: map[string]bool{"command": true, "web_fetch": true, "custom_tool": true},
		},
		{
			name:          "ask-alias",
			mode:          PermissionModeAsk,
			allowed:       map[string]bool{"read_file": true, "write_file": true, "command": true, "web_fetch": true, "custom_tool": true},
			needsApproval: map[string]bool{"command": true, "web_fetch": true, "custom_tool": true},
		},
		{
			name:    "full-access",
			mode:    PermissionModeFullAccess,
			allowed: map[string]bool{"read_file": true, "write_file": true, "command": true, "web_fetch": true, "custom_tool": true},
		},
	}

	toolNames := []string{"read_file", "write_file", "command", "web_fetch", "custom_tool"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tools := make(map[string]*permissionTestTool, len(toolNames))
			registrations := make([]ToolRegistration, 0, len(toolNames))
			for _, name := range toolNames {
				registeredTool := &permissionTestTool{name: name}
				tools[name] = registeredTool
				registrations = append(registrations, ToolRegistration{Tool: registeredTool, Metadata: metadata[name]})
			}
			registry, err := NewRegistry(registrations...)
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.SetPermissionMode(test.mode); err != nil {
				t.Fatal(err)
			}
			approvals := make(map[string]int)
			registry.SetPermissionApprovalHandler(func(_ context.Context, request PermissionRequest) bool {
				approvals[request.Tool]++
				return true
			})

			for _, name := range toolNames {
				if visible := registry.ToolVisible(name); visible != test.allowed[name] {
					t.Errorf("%s visible = %v, want %v for %s mode", name, visible, test.allowed[name], test.name)
				}
				result := registry.Run(context.Background(), name, json.RawMessage(`{}`))
				wantAllowed := test.allowed[name]
				if wantAllowed && result.Err != nil {
					t.Errorf("%s was rejected: %v", name, result.Err)
				}
				if !wantAllowed && result.Err == nil {
					t.Errorf("%s was allowed", name)
				}
				wantCalls := 0
				if wantAllowed {
					wantCalls = 1
				}
				if tools[name].calls != wantCalls {
					t.Errorf("%s calls = %d, want %d", name, tools[name].calls, wantCalls)
				}
				wantApprovals := 0
				if test.needsApproval[name] {
					wantApprovals = 1
				}
				if approvals[name] != wantApprovals {
					t.Errorf("%s approvals = %d, want %d", name, approvals[name], wantApprovals)
				}
			}
		})
	}
}

func TestPermissionChangeIsRecheckedBeforeExecution(t *testing.T) {
	registeredTool := &permissionTestTool{name: "write_file"}
	registry, err := NewRegistry(ToolRegistration{
		Tool:     registeredTool,
		Metadata: model.ToolMetadata{PermissionClass: model.ToolPermissionWorkspaceWrite},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(PermissionModeWorkspace); err != nil {
		t.Fatal(err)
	}
	if !registry.ToolVisible("write_file") {
		t.Fatal("workspace mode hid workspace writes")
	}
	if err := registry.SetPermissionMode(PermissionModeReadOnly); err != nil {
		t.Fatal(err)
	}
	if registry.ToolVisible("write_file") {
		t.Fatal("read-only mode exposed workspace writes")
	}
	if result := registry.Run(context.Background(), "write_file", json.RawMessage(`{}`)); result.Err == nil {
		t.Fatal("write succeeded after switching to read-only mode")
	}
	if registeredTool.calls != 0 {
		t.Fatalf("write tool ran %d times after permission change", registeredTool.calls)
	}
}

func TestTaskPermissionOverrideIsScopedToOneCall(t *testing.T) {
	registeredTool := &permissionTestTool{name: "write_file"}
	registry, err := NewRegistry(ToolRegistration{
		Tool:     registeredTool,
		Metadata: model.ToolMetadata{PermissionClass: model.ToolPermissionWorkspaceWrite},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(PermissionModeReadOnly); err != nil {
		t.Fatal(err)
	}
	if registry.ToolVisible("write_file") || registry.ToolVisibleForMode("write_file", "read-only") {
		t.Fatal("read-only mode exposed a workspace write")
	}
	if !registry.ToolVisibleForMode("write_file", "workspace") {
		t.Fatal("workspace override hid a workspace write")
	}
	ctx := WithPermissionMode(context.Background(), "workspace")
	if result := registry.Run(ctx, "write_file", json.RawMessage(`{}`)); result.Err != nil {
		t.Fatalf("scoped workspace call failed: %v", result.Err)
	}
	if registry.PermissionMode() != PermissionModeReadOnly || registry.ToolVisible("write_file") {
		t.Fatalf("task override changed global permission mode to %q", registry.PermissionMode())
	}
	if result := registry.Run(context.Background(), "write_file", json.RawMessage(`{}`)); result.Err == nil {
		t.Fatal("workspace write succeeded without the scoped override")
	}
	if registeredTool.calls != 1 {
		t.Fatalf("write tool ran %d times, want exactly once", registeredTool.calls)
	}
}

func TestUnclassifiedToolRequestsApprovalInWorkspaceAndReadOnlyModes(t *testing.T) {
	for _, mode := range []PermissionMode{PermissionModeWorkspace, PermissionModeReadOnly} {
		t.Run(string(mode), func(t *testing.T) {
			registeredTool := &permissionTestTool{name: "custom_tool"}
			registry, err := NewRegistry(ToolRegistration{Tool: registeredTool})
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.SetPermissionMode(mode); err != nil {
				t.Fatal(err)
			}
			approvals := 0
			registry.SetPermissionApprovalHandler(func(context.Context, PermissionRequest) bool {
				approvals++
				return true
			})

			result := registry.Run(context.Background(), "custom_tool", json.RawMessage(`{}`))
			if result.Err != nil {
				t.Fatalf("error = %v, want successful approved execution", result.Err)
			}
			if approvals != 1 || registeredTool.calls != 1 {
				t.Fatalf("approvals = %d, calls = %d, want both 1", approvals, registeredTool.calls)
			}
		})
	}
}

func TestAutoModeDenialAndMissingApprovalHandlerBlockTool(t *testing.T) {
	for _, test := range []struct {
		name     string
		handler  PermissionApprovalHandler
		wantText string
	}{
		{name: "denied", handler: func(context.Context, PermissionRequest) bool { return false }, wantText: "permission denied"},
		{name: "no handler", wantText: "requires approval"},
	} {
		t.Run(test.name, func(t *testing.T) {
			registeredTool := &permissionTestTool{name: "command"}
			registry, err := NewRegistry(ToolRegistration{
				Tool:     registeredTool,
				Metadata: model.ToolMetadata{PermissionClass: model.ToolPermissionCommand},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.SetPermissionMode(PermissionModeAuto); err != nil {
				t.Fatal(err)
			}
			registry.SetPermissionApprovalHandler(test.handler)

			result := registry.Run(context.Background(), "command", json.RawMessage(`{}`))
			if result.Err == nil || !strings.Contains(result.Err.Error(), test.wantText) {
				t.Fatalf("error = %v, want text %q", result.Err, test.wantText)
			}
			if registeredTool.calls != 0 {
				t.Fatalf("tool ran %d times after approval was unavailable", registeredTool.calls)
			}
		})
	}
}

func TestSessionApprovalStoreRemembersApprovals(t *testing.T) {
	registeredTool := &permissionTestTool{name: "command"}
	registry, err := NewRegistry(ToolRegistration{
		Tool:     registeredTool,
		Metadata: model.ToolMetadata{PermissionClass: model.ToolPermissionCommand},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(PermissionModeWorkspace); err != nil {
		t.Fatal(err)
	}
	approvals := 0
	registry.SetPermissionApprovalHandler(func(_ context.Context, _ PermissionRequest) bool {
		approvals++
		return true
	})

	store := NewSessionApprovalStore(nil)
	ctx := WithSessionApprovalStore(context.Background(), store)

	// First call requires approval
	res1 := registry.Run(ctx, "command", json.RawMessage(`{"cmd": "git status"}`))
	if res1.Err != nil {
		t.Fatalf("first run failed: %v", res1.Err)
	}
	if approvals != 1 {
		t.Fatalf("expected 1 approval, got %d", approvals)
	}

	// Second call with identical arguments (different whitespace/formatting) should NOT require approval
	res2 := registry.Run(ctx, "command", json.RawMessage(`{  "cmd":   "git status"  }`))
	if res2.Err != nil {
		t.Fatalf("second run failed: %v", res2.Err)
	}
	if approvals != 1 {
		t.Fatalf("expected approvals to stay 1, got %d", approvals)
	}

	// Third call with different argument requires fresh approval
	res3 := registry.Run(ctx, "command", json.RawMessage(`{"cmd": "git log"}`))
	if res3.Err != nil {
		t.Fatalf("third run failed: %v", res3.Err)
	}
	if approvals != 2 {
		t.Fatalf("expected 2 approvals for new argument, got %d", approvals)
	}
}

func TestAutoModeModelReview(t *testing.T) {
	registeredTool := &permissionTestTool{name: "command"}
	registry, err := NewRegistry(ToolRegistration{
		Tool:     registeredTool,
		Metadata: model.ToolMetadata{PermissionClass: model.ToolPermissionCommand},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPermissionMode(PermissionModeAuto); err != nil {
		t.Fatal(err)
	}

	interactiveApprovals := 0
	registry.SetPermissionApprovalHandler(func(_ context.Context, _ PermissionRequest) bool {
		interactiveApprovals++
		return true
	})

	store := NewSessionApprovalStore(nil)
	ctx := WithSessionApprovalStore(context.Background(), store)

	// 1. Safe review -> auto approved without interactive prompt
	registry.SetPermissionAutoReviewHandler(func(_ context.Context, req PermissionRequest) (bool, error) {
		return true, nil
	})
	res1 := registry.Run(ctx, "command", json.RawMessage(`{"cmd": "go test"}`))
	if res1.Err != nil {
		t.Fatalf("safe run failed: %v", res1.Err)
	}
	if interactiveApprovals != 0 {
		t.Fatalf("expected 0 interactive approvals for safe review, got %d", interactiveApprovals)
	}

	// Subsequent identical run is remembered in SessionApprovalStore
	res1Repeat := registry.Run(ctx, "command", json.RawMessage(`{"cmd": "go test"}`))
	if res1Repeat.Err != nil {
		t.Fatalf("repeated run failed: %v", res1Repeat.Err)
	}
	if interactiveApprovals != 0 {
		t.Fatalf("expected 0 interactive approvals for repeated run, got %d", interactiveApprovals)
	}

	// 2. Unsafe review -> falls back to interactive approval
	registry.SetPermissionAutoReviewHandler(func(_ context.Context, req PermissionRequest) (bool, error) {
		return false, nil
	})
	res2 := registry.Run(ctx, "command", json.RawMessage(`{"cmd": "rm -rf /"}`))
	if res2.Err != nil {
		t.Fatalf("unsafe run failed: %v", res2.Err)
	}
	if interactiveApprovals != 1 {
		t.Fatalf("expected 1 interactive approval for unsafe review fallback, got %d", interactiveApprovals)
	}

	// 3. Error in review -> falls back to interactive approval
	registry.SetPermissionAutoReviewHandler(func(_ context.Context, req PermissionRequest) (bool, error) {
		return false, errors.New("timeout")
	})
	res3 := registry.Run(ctx, "command", json.RawMessage(`{"cmd": "git push --force"}`))
	if res3.Err != nil {
		t.Fatalf("error review run failed: %v", res3.Err)
	}
	if interactiveApprovals != 2 {
		t.Fatalf("expected 2 interactive approvals after review error fallback, got %d", interactiveApprovals)
	}
}
