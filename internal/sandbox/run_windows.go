//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	publictool "github.com/afterlune/luneCore/tool"
	"golang.org/x/sys/windows"
)

func runPlatform(ctx context.Context, request publictool.CommandRequest, _ io.Writer) (runErr error) {
	workspace, err := filepath.EvalSymlinks(request.Workspace)
	if err != nil {
		return fmt.Errorf("resolve sandbox workspace: %w", err)
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return err
	}
	tempDir, err := filepath.EvalSymlinks(request.Sandbox.TempDir)
	if err != nil {
		return fmt.Errorf("resolve sandbox temporary directory: %w", err)
	}
	command := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(command); err != nil {
		return fmt.Errorf("PowerShell is unavailable: %w", err)
	}
	script := "try { Set-Location -LiteralPath '" + strings.ReplaceAll(workspace, "'", "''") + "' -ErrorAction Stop; [System.Environment]::CurrentDirectory = '" + strings.ReplaceAll(workspace, "'", "''") + "' } catch { Write-Error $_; exit 1 }; " + request.Command
	args := []string{command, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script}
	if request.Sandbox.PermissionMode == "full-access" {
		if !request.Sandbox.Network {
			return errors.New("full-access sandbox requires network permission on Windows")
		}
		homeDir := request.Sandbox.HomeDir
		if homeDir == "" {
			homeDir = os.Getenv("USERPROFILE")
		}
		if homeDir == "" {
			return errors.New("current-user profile is unavailable for full-access sandbox")
		}
		for name, value := range map[string]string{"TEMP": tempDir, "TMP": tempDir, "HOME": homeDir} {
			if err := os.Setenv(name, value); err != nil {
				return fmt.Errorf("set sandbox %s: %w", name, err)
			}
		}
		return runRestrictedUserProcess(ctx, args, workspace)
	}
	if request.Sandbox.PermissionMode != "workspace" {
		return fmt.Errorf("unsupported sandbox permission mode %q", request.Sandbox.PermissionMode)
	}

	profileName := windowsProfileName(workspace, tempDir)
	containerSID, err := createAppContainerProfile(profileName)
	if err != nil {
		return err
	}
	defer func() {
		if deleteErr := deleteAppContainerProfile(profileName); deleteErr != nil {
			runErr = errors.Join(runErr, deleteErr)
		}
	}()
	defer windows.FreeSid(containerSID)

	var revert []func() error
	defer func() {
		for index := len(revert) - 1; index >= 0; index-- {
			if restoreErr := revert[index](); restoreErr != nil {
				runErr = errors.Join(runErr, fmt.Errorf("restore AppContainer ACL: %w", restoreErr))
			}
		}
	}()
	grant := func(path string, access publictool.PathAccess, required bool) error {
		resolved, resolveErr := filepath.EvalSymlinks(path)
		if resolveErr != nil {
			if !required && errors.Is(resolveErr, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("resolve AppContainer path %q: %w", path, resolveErr)
		}
		readWrite := access == publictool.PathReadWrite
		restore, grantErr := grantAppContainerPath(resolved, containerSID, readWrite)
		if grantErr != nil {
			if !required && errors.Is(grantErr, windows.ERROR_ACCESS_DENIED) {
				return nil
			}
			return grantErr
		}
		revert = append(revert, restore)
		return nil
	}
	if err := grant(workspace, publictool.PathReadWrite, true); err != nil {
		return err
	}
	if err := grant(tempDir, publictool.PathReadWrite, true); err != nil {
		return err
	}
	for _, path := range request.Sandbox.Grants {
		if err := grant(path.Path, path.Access, true); err != nil {
			return err
		}
	}
	for _, path := range request.Sandbox.CommandToolchainPaths {
		if err := grant(path, publictool.PathRead, false); err != nil {
			return err
		}
	}
	// A workspace-rooted provider drive avoids PowerShell inspecting private
	// ancestors merely to resolve a location. Native programs use the real cwd.
	escapedWorkspace := strings.ReplaceAll(workspace, "'", "''")
	args[len(args)-1] = "try { New-PSDrive -Name LuneWorkspace -PSProvider FileSystem -Root '" + escapedWorkspace + "' -ErrorAction Stop | Out-Null; Set-Location -LiteralPath 'LuneWorkspace:\\' -ErrorAction Stop; [System.Environment]::CurrentDirectory = '" + escapedWorkspace + "' } catch { Write-Error $_; exit 1 }; " + request.Command
	return runAppContainerProcess(ctx, containerSID, args, workspace, tempDir, request.Sandbox.Network, request.Sandbox.CommandToolchainPaths)
}

func runChild(publictool.CommandRequest) error {
	return errors.New("sandbox child mode is supported only on Linux")
}
