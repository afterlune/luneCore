package tool

import (
	"encoding/json"
	"runtime"
	"testing"

	publictool "github.com/afterlune/luneCore/tool"
)

func TestSessionApprovalStoreKeepsFingerprintAcrossLoad(t *testing.T) {
	args := json.RawMessage(`{"command":"git status"}`)
	store := NewSessionApprovalStore([]publictool.SessionApproval{
		{Tool: "command", Arguments: string(args), Fingerprint: "ws=/a;sandbox=true"},
		// Legacy row persisted before boundaries existed: it cannot be bound to any
		// execution context, so it must not be honored.
		{Tool: "command", Arguments: string(args)},
	})

	if !store.Allows("command", args, "ws=/a;sandbox=true") {
		t.Fatal("an approval from the same boundary must be honored")
	}
	if store.Allows("command", args, "ws=/a;sandbox=false") {
		t.Fatal("an approval must not survive a sandbox change")
	}
	if store.Allows("command", args, "") {
		t.Fatal("an empty fingerprint must never match")
	}
	if got := store.Snapshot(); len(got) != 1 || got[0].Fingerprint != "ws=/a;sandbox=true" {
		t.Fatalf("legacy approvals without a fingerprint must be dropped, got %#v", got)
	}
}

func TestSessionApprovalStoreRejectsUnboundAdd(t *testing.T) {
	store := NewSessionApprovalStore(nil)
	store.Add(publictool.SessionApproval{Tool: "command", Arguments: `{"command":"ls"}`})
	if got := store.Snapshot(); len(got) != 0 {
		t.Fatalf("an approval without a fingerprint was stored: %#v", got)
	}
}

func TestExecutionFingerprintTracksTheBoundary(t *testing.T) {
	base := ComputeExecutionFingerprint("/work", "workspace", "/bin/sh", nil, false)
	if base == "" {
		t.Fatal("fingerprint must not be empty")
	}
	if base != ComputeExecutionFingerprint("/work", "workspace", "/bin/sh", nil, false) {
		t.Fatal("fingerprint must be stable")
	}
	for name, other := range map[string]string{
		"sandbox":    ComputeExecutionFingerprint("/work", "workspace", "/bin/sh", nil, true),
		"workspace":  ComputeExecutionFingerprint("/other", "workspace", "/bin/sh", nil, false),
		"mode":       ComputeExecutionFingerprint("/work", "auto", "/bin/sh", nil, false),
		"shell":      ComputeExecutionFingerprint("/work", "workspace", "cmd.exe", nil, false),
		"toolchains": ComputeExecutionFingerprint("/work", "workspace", "/bin/sh", []string{"/go"}, false),
	} {
		if base == other {
			t.Fatalf("changing the %s must change the fingerprint", name)
		}
	}
	if got := ComputeExecutionFingerprint("", "workspace", executionShell(), nil, false); got == "" {
		t.Fatal("a registry without a workspace root still needs a fingerprint")
	}
}

func TestExecutionShellMatchesTheHostPlatform(t *testing.T) {
	want := "/bin/sh"
	if runtime.GOOS == "windows" {
		want = "cmd.exe"
	}
	if got := executionShell(); got != want {
		t.Fatalf("executionShell() = %q, want %q", got, want)
	}
}
