//go:build windows

package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	publictool "github.com/afterlune/luneCore/tool"
	"golang.org/x/sys/windows"
)

func TestRestrictedCurrentUserTokenRemovesElevation(t *testing.T) {
	current, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	user, err := current.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		if _, err := restrictedCurrentUserToken(); err == nil {
			t.Fatal("LocalSystem token was accepted for full access")
		}
		return
	}

	token, err := restrictedCurrentUserToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()

	groups, err := token.GetTokenGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups.AllGroups() {
		if group.Sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) && group.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0 {
			t.Fatal("restricted token retained enabled Administrators membership")
		}
	}
	integrity, err := tokenIntegrityRID(token)
	if err != nil {
		t.Fatal(err)
	}
	if integrity > 0x2000 {
		t.Fatalf("restricted token integrity = %d, want medium or lower", integrity)
	}
}

func TestWindowsEnvironmentBlockAppliesSandboxOverrides(t *testing.T) {
	block, err := windowsEnvironmentBlock(map[string]string{
		"PATH": "C:\\toolchain\\bin", "GOCACHE": "C:\\temp\\go-cache",
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := strings.Split(string(utf16.Decode(block)), "\x00")
	var pathCount, cacheCount int
	for _, entry := range entries {
		if strings.EqualFold(entry, `PATH=C:\toolchain\bin`) {
			pathCount++
		}
		if strings.EqualFold(entry, `GOCACHE=C:\temp\go-cache`) {
			cacheCount++
		}
	}
	if pathCount != 1 || cacheCount != 1 {
		t.Fatalf("environment block overrides PATH=%d GOCACHE=%d: %q", pathCount, cacheCount, entries)
	}
}

func TestAppContainerRestoresPathACLsAfterChildOutcomes(t *testing.T) {
	toolchainRoot := t.TempDir()
	toolchainBin := filepath.Join(toolchainRoot, "bin")
	if err := os.MkdirAll(toolchainBin, 0700); err != nil {
		t.Fatal(err)
	}
	toolchainFile := filepath.Join(toolchainBin, "read-only.txt")
	if err := os.WriteFile(toolchainFile, []byte("toolchain data"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	tempDir := filepath.Join(t.TempDir(), "command-temp")
	for _, path := range []string{workspace, tempDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(workspace, "toolchain-ran.txt")
	toolchainScript := filepath.Join(toolchainBin, "toolchain-check.cmd")
	if err := os.WriteFile(toolchainScript, []byte("@echo off\r\necho toolchain-ok>\""+marker+"\"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	paths := []string{workspace, tempDir, toolchainRoot, toolchainFile, toolchainScript}
	snapshots := make(map[string]string, len(paths))
	for _, path := range paths {
		snapshot, err := snapshotPathDACL(path)
		if err != nil {
			t.Skipf("AppContainer ACL inspection is unavailable: %v", err)
		}
		snapshots[path] = snapshot
	}

	for _, scenario := range []struct {
		name    string
		command string
		cancel  bool
		wantErr bool
		marker  bool
	}{
		{name: "success", command: "exit 0"},
		{name: "failed", command: "exit 17", wantErr: true},
		{name: "granted-toolchain-is-readable", command: "Get-Content -LiteralPath '" + toolchainFile + "' -ErrorAction Stop | Out-Null; exit 0"},
		{name: "granted-toolchain-is-not-writable", command: "try { Set-Content -LiteralPath '" + toolchainFile + "' -Value 'changed' -ErrorAction Stop; exit 0 } catch { exit 17 }", wantErr: true},
		{name: "granted-toolchain-is-on-path-and-executable", command: "toolchain-check.cmd", marker: true},
		{name: "cancelled", command: "Start-Sleep -Seconds 30", cancel: true, wantErr: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			request := publictool.CommandRequest{
				Command: scenario.command, Workspace: workspace,
				Sandbox: publictool.SandboxPolicy{
					PermissionMode: "workspace", Workspace: workspace, TempDir: tempDir,
					Grants:                []publictool.PathGrant{{Path: toolchainRoot, Access: publictool.PathRead}},
					CommandToolchainPaths: []string{toolchainRoot},
				},
			}
			result := make(chan error, 1)
			go func() { result <- RunContext(ctx, request, io.Discard) }()
			if scenario.cancel {
				time.Sleep(750 * time.Millisecond)
				cancel()
			}
			err := <-result
			if scenario.wantErr && err == nil {
				t.Fatal("sandbox command succeeded; want a child error")
			}
			if !scenario.wantErr && err != nil {
				t.Fatalf("sandbox command failed: %v", err)
			}
			if scenario.marker {
				if data, readErr := os.ReadFile(marker); readErr != nil || string(data) != "toolchain-ok\r\n" {
					t.Fatalf("toolchain execution marker = %q, error=%v", data, readErr)
				}
			} else if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("toolchain unexpectedly wrote marker; stat error=%v", statErr)
			}
			if scenario.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled command error = %v, want context.Canceled", err)
			}
			for _, path := range paths {
				current, err := snapshotPathDACL(path)
				if err != nil {
					t.Fatalf("snapshot ACL for %q: %v", path, err)
				}
				if current != snapshots[path] {
					t.Errorf("sandbox ACL entries remained changed on %q after %s child: before=%s after=%s", path, scenario.name, snapshots[path], current)
				}
			}
			if scenario.marker {
				if err := os.Remove(marker); err != nil {
					t.Fatalf("remove toolchain marker: %v", err)
				}
			}
		})
	}
}

func snapshotPathDACL(path string) (string, error) {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	sddl := descriptor.String()
	start := strings.Index(sddl, "D:")
	if start < 0 {
		return "", errors.New("security descriptor has no DACL")
	}
	dacl := sddl[start+2:]
	firstACE := strings.IndexByte(dacl, '(')
	if firstACE < 0 {
		return "", errors.New("security descriptor DACL has no ACE list")
	}
	flags := strings.ReplaceAll(dacl[:firstACE], "AI", "")
	dacl = "D:" + flags + dacl[firstACE:]
	var normalized strings.Builder
	for len(dacl) > 0 {
		start := strings.IndexByte(dacl, '(')
		if start < 0 {
			normalized.WriteString(dacl)
			break
		}
		normalized.WriteString(dacl[:start])
		end := strings.IndexByte(dacl[start:], ')')
		if end < 0 {
			normalized.WriteString(dacl[start:])
			break
		}
		end += start
		ace := strings.Split(dacl[start+1:end], ";")
		if len(ace) > 1 {
			ace[1] = strings.ReplaceAll(ace[1], "ID", "")
		}
		normalized.WriteByte('(')
		normalized.WriteString(strings.Join(ace, ";"))
		normalized.WriteByte(')')
		dacl = dacl[end+1:]
	}
	return normalized.String(), nil
}
