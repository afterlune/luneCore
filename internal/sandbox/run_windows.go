//go:build windows

package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	publictool "github.com/afterlune/luneCore/tool"
	"golang.org/x/sys/windows"
)

const procThreadAttributeSecurityCapabilities = 0x00020009
const fileDeleteChild = 0x00000040

type securityCapabilities struct {
	AppContainerSID *windows.SID
	Capabilities    *windows.SIDAndAttributes
	CapabilityCount uint32
	Reserved        uint32
}

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
	// A workspace-rooted provider drive avoids PowerShell inspecting private
	// ancestors merely to resolve a location. Native programs use the real cwd.
	escapedWorkspace := strings.ReplaceAll(workspace, "'", "''")
	args[len(args)-1] = "try { New-PSDrive -Name LuneWorkspace -PSProvider FileSystem -Root '" + escapedWorkspace + "' -ErrorAction Stop | Out-Null; Set-Location -LiteralPath 'LuneWorkspace:\\' -ErrorAction Stop; [System.Environment]::CurrentDirectory = '" + escapedWorkspace + "' } catch { Write-Error $_; exit 1 }; " + request.Command
	return runAppContainerProcess(ctx, containerSID, args, workspace, tempDir, request.Sandbox.Network)
}

func restrictedCurrentUserToken() (windows.Token, error) {
	const disableMaxPrivilege = 0x1
	var current windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT, &current); err != nil {
		return 0, fmt.Errorf("open current-user token: %w", err)
	}
	defer current.Close()
	user, err := current.GetTokenUser()
	if err != nil {
		return 0, fmt.Errorf("read current-user SID: %w", err)
	}
	if user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return 0, errors.New("full-access sandbox cannot run from the LocalSystem account")
	}

	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return 0, fmt.Errorf("create administrators SID: %w", err)
	}
	groups, err := current.GetTokenGroups()
	if err != nil {
		return 0, fmt.Errorf("read current-user token groups: %w", err)
	}
	var disabled []windows.SIDAndAttributes
	for _, group := range groups.AllGroups() {
		if group.Sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) && group.Attributes&windows.SE_GROUP_USE_FOR_DENY_ONLY == 0 {
			disabled = append(disabled, windows.SIDAndAttributes{Sid: group.Sid})
			break
		}
	}

	var restricted windows.Token
	var disabledPointer uintptr
	if len(disabled) > 0 {
		disabledPointer = uintptr(unsafe.Pointer(&disabled[0]))
	}
	var pinner runtime.Pinner
	pinner.Pin(adminSID)
	for index := range disabled {
		pinner.Pin(disabled[index].Sid)
	}
	hr, _, callErr := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken").Call(
		uintptr(current), disableMaxPrivilege, uintptr(len(disabled)), disabledPointer,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&restricted)),
	)
	runtime.KeepAlive(groups)
	runtime.KeepAlive(disabled)
	runtime.KeepAlive(adminSID)
	pinner.Unpin()
	if hr == 0 {
		return 0, fmt.Errorf("create restricted current-user token: %w", callErr)
	}

	integrity, err := tokenIntegrityRID(current)
	if err != nil {
		_ = restricted.Close()
		return 0, err
	}
	const mediumIntegrityRID = 0x2000
	if integrity > mediumIntegrityRID {
		mediumSID, sidErr := windows.CreateWellKnownSid(windows.WinMediumLabelSid)
		if sidErr != nil {
			_ = restricted.Close()
			return 0, fmt.Errorf("create medium-integrity SID: %w", sidErr)
		}
		label := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{
			Sid: mediumSID, Attributes: windows.SE_GROUP_INTEGRITY | windows.SE_GROUP_INTEGRITY_ENABLED,
		}}
		var pinner runtime.Pinner
		pinner.Pin(mediumSID)
		pinner.Pin(&label)
		err = windows.SetTokenInformation(restricted, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&label)), label.Size())
		runtime.KeepAlive(mediumSID)
		pinner.Unpin()
		if err != nil {
			_ = restricted.Close()
			return 0, fmt.Errorf("lower restricted token integrity: %w", err)
		}
	}
	return restricted, nil
}

func tokenIntegrityRID(token windows.Token) (uint32, error) {
	var size uint32
	err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, nil, 0, &size)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || size < uint32(unsafe.Sizeof(windows.Tokenmandatorylabel{})) {
		return 0, fmt.Errorf("query token integrity size: %w", err)
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buffer[0], uint32(len(buffer)), &size); err != nil {
		return 0, fmt.Errorf("read token integrity: %w", err)
	}
	label := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buffer[0]))
	count := label.Label.Sid.SubAuthorityCount()
	if count == 0 {
		return 0, errors.New("current-user token has an invalid integrity SID")
	}
	return label.Label.Sid.SubAuthority(uint32(count - 1)), nil
}

func runRestrictedUserProcess(ctx context.Context, args []string, directory string) error {
	token, err := restrictedCurrentUserToken()
	if err != nil {
		return err
	}
	defer token.Close()
	return runWindowsChild(ctx, args, directory, token, nil)
}

func runWindowsChild(ctx context.Context, args []string, directory string, token windows.Token, security *securityCapabilities) error {
	attributeCount := uint32(1)
	if security != nil {
		attributeCount++
	}
	attributeList, err := windows.NewProcThreadAttributeList(attributeCount)
	if err != nil {
		return fmt.Errorf("create sandbox process attributes: %w", err)
	}
	defer attributeList.Delete()
	if security != nil {
		if err := attributeList.Update(procThreadAttributeSecurityCapabilities, unsafe.Pointer(security), unsafe.Sizeof(*security)); err != nil {
			return fmt.Errorf("set AppContainer process attribute: %w", err)
		}
	}
	stdin, err := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("open sandbox process stdin: %w", err)
	}
	defer windows.CloseHandle(stdin)
	stdOut, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return err
	}
	stdErr, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil {
		return err
	}
	process, err := windows.GetCurrentProcess()
	if err != nil {
		return err
	}
	childHandles := make([]windows.Handle, 3)
	for index, source := range []windows.Handle{stdin, stdOut, stdErr} {
		if err := windows.DuplicateHandle(process, source, process, &childHandles[index], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return fmt.Errorf("duplicate sandbox process standard handle: %w", err)
		}
		defer windows.CloseHandle(childHandles[index])
	}
	if err := attributeList.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&childHandles[0]), unsafe.Sizeof(childHandles[0])*uintptr(len(childHandles))); err != nil {
		return fmt.Errorf("set sandbox process handle list: %w", err)
	}
	startup := windows.StartupInfoEx{}
	startup.StartupInfo.Cb = uint32(unsafe.Sizeof(startup))
	startup.StartupInfo.Flags = windows.STARTF_USESTDHANDLES
	startup.StartupInfo.StdInput, startup.StartupInfo.StdOutput, startup.StartupInfo.StdErr = childHandles[0], childHandles[1], childHandles[2]
	startup.ProcThreadAttributeList = attributeList.List()
	application, err := windows.UTF16PtrFromString(args[0])
	if err != nil {
		return err
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return err
	}
	directory16, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create sandbox process job: %w", err)
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return fmt.Errorf("configure sandbox process job: %w", err)
	}
	processInfo := windows.ProcessInformation{}
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW)
	if token != 0 {
		err = windows.CreateProcessAsUser(token, application, commandLine, nil, nil, true, flags, nil, directory16, &startup.StartupInfo, &processInfo)
	} else {
		err = windows.CreateProcess(application, commandLine, nil, nil, true, flags, nil, directory16, &startup.StartupInfo, &processInfo)
	}
	if err != nil {
		return fmt.Errorf("create sandbox process: %w", err)
	}
	defer windows.CloseHandle(processInfo.Process)
	defer windows.CloseHandle(processInfo.Thread)
	if err := windows.AssignProcessToJobObject(job, processInfo.Process); err != nil {
		_ = windows.TerminateProcess(processInfo.Process, 1)
		return fmt.Errorf("assign sandbox process to job: %w", err)
	}
	if _, err := windows.ResumeThread(processInfo.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		return fmt.Errorf("resume sandbox process: %w", err)
	}
	waitResult := make(chan error, 1)
	go func() {
		_, waitErr := windows.WaitForSingleObject(processInfo.Process, windows.INFINITE)
		waitResult <- waitErr
	}()
	select {
	case err := <-waitResult:
		if err != nil {
			return fmt.Errorf("wait for sandbox process: %w", err)
		}
	case <-ctx.Done():
		_ = windows.TerminateJobObject(job, 1)
		if err := <-waitResult; err != nil {
			return fmt.Errorf("stop canceled sandbox process: %w", err)
		}
		return ctx.Err()
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(processInfo.Process, &exitCode); err != nil {
		return fmt.Errorf("read restricted process exit code: %w", err)
	}
	if exitCode != 0 {
		return &ExitError{Code: int(exitCode)}
	}
	return nil
}

func windowsProfileName(workspace, tempDir string) string {
	hash := sha256.Sum256([]byte(strings.ToLower(workspace) + "\x00" + tempDir + "\x00" + time.Now().UTC().Format(time.RFC3339Nano)))
	return fmt.Sprintf("LuneCore_%x", hash[:12])
}

func deleteAppContainerProfile(name string) error {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	hr, _, _ := windows.NewLazySystemDLL("userenv.dll").NewProc("DeleteAppContainerProfile").Call(uintptr(unsafe.Pointer(name16)))
	if hr != 0 {
		return fmt.Errorf("delete AppContainer profile failed: HRESULT 0x%08x", uint32(hr))
	}
	return nil
}

func createAppContainerProfile(name string) (*windows.SID, error) {
	userenv := windows.NewLazySystemDLL("userenv.dll")
	create := userenv.NewProc("CreateAppContainerProfile")
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	display, _ := windows.UTF16PtrFromString("LuneCore command")
	description, _ := windows.UTF16PtrFromString("Per-invocation command sandbox")
	var sid *windows.SID
	hr, _, _ := create.Call(uintptr(unsafe.Pointer(name16)), uintptr(unsafe.Pointer(display)), uintptr(unsafe.Pointer(description)), 0, 0, uintptr(unsafe.Pointer(&sid)))
	if hr == 0 && sid != nil {
		return sid, nil
	}
	if uint32(hr) != 0x800700b7 { // HRESULT_FROM_WIN32(ERROR_ALREADY_EXISTS)
		return nil, fmt.Errorf("create AppContainer profile failed: HRESULT 0x%08x", uint32(hr))
	}
	derive := userenv.NewProc("DeriveAppContainerSidFromAppContainerName")
	hr, _, _ = derive.Call(uintptr(unsafe.Pointer(name16)), uintptr(unsafe.Pointer(&sid)))
	if hr != 0 || sid == nil {
		return nil, fmt.Errorf("derive AppContainer SID failed: HRESULT 0x%08x", uint32(hr))
	}
	return sid, nil
}

func grantAppContainerPath(path string, sid *windows.SID, readWrite bool) (func() error, error) {
	release, err := lockPathACL(path)
	if err != nil {
		return nil, err
	}
	defer release()
	permissions := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE | windows.READ_CONTROL)
	if readWrite {
		permissions |= windows.FILE_GENERIC_WRITE | windows.DELETE | fileDeleteChild
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("read ACL for %q: %w", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, err
	}
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(permissions),
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	var pinner runtime.Pinner
	pinner.Pin(sid)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, dacl)
	if err == nil {
		err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
	}
	pinner.Unpin()
	if err != nil {
		return nil, fmt.Errorf("grant AppContainer access to %q: %w", path, err)
	}
	return func() error {
		release, err := lockPathACL(path)
		if err != nil {
			return err
		}
		defer release()
		current, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		currentDACL, _, err := current.DACL()
		if err != nil {
			return err
		}
		revoke := entry
		revoke.AccessMode = windows.REVOKE_ACCESS
		var pinner runtime.Pinner
		pinner.Pin(sid)
		updated, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{revoke}, currentDACL)
		if err == nil {
			err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, updated, nil)
		}
		pinner.Unpin()
		return err
	}, nil
}

func runAppContainerProcess(ctx context.Context, sid *windows.SID, args []string, directory, tempDir string, network bool) error {
	if err := os.Setenv("TEMP", tempDir); err != nil {
		return fmt.Errorf("set sandbox TEMP: %w", err)
	}
	if err := os.Setenv("TMP", tempDir); err != nil {
		return fmt.Errorf("set sandbox TMP: %w", err)
	}
	if err := os.Setenv("HOME", tempDir); err != nil {
		return fmt.Errorf("set sandbox HOME: %w", err)
	}
	capabilities, cleanupCapabilities, err := appContainerCapabilities(network)
	if err != nil {
		return err
	}
	defer cleanupCapabilities()
	security := securityCapabilities{AppContainerSID: sid}
	if len(capabilities) > 0 {
		security.Capabilities = &capabilities[0]
		security.CapabilityCount = uint32(len(capabilities))
	}

	attributeList, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return fmt.Errorf("create AppContainer process attributes: %w", err)
	}
	defer attributeList.Delete()
	if err := attributeList.Update(procThreadAttributeSecurityCapabilities, unsafe.Pointer(&security), unsafe.Sizeof(security)); err != nil {
		return fmt.Errorf("set AppContainer process attribute: %w", err)
	}

	stdin, err := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("open sandbox stdin: %w", err)
	}
	defer windows.CloseHandle(stdin)
	stdOut, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return err
	}
	stdErr, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if err != nil {
		return err
	}
	proc, err := windows.GetCurrentProcess()
	if err != nil {
		return err
	}
	childHandles := make([]windows.Handle, 3)
	for index, source := range []windows.Handle{stdin, stdOut, stdErr} {
		if err := windows.DuplicateHandle(proc, source, proc, &childHandles[index], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return fmt.Errorf("duplicate sandbox standard handle: %w", err)
		}
		defer windows.CloseHandle(childHandles[index])
	}
	if err := attributeList.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&childHandles[0]), unsafe.Sizeof(childHandles[0])*uintptr(len(childHandles))); err != nil {
		return fmt.Errorf("set sandbox handle list: %w", err)
	}

	startup := windows.StartupInfoEx{}
	startup.StartupInfo.Cb = uint32(unsafe.Sizeof(startup))
	startup.StartupInfo.Flags = windows.STARTF_USESTDHANDLES
	startup.StartupInfo.StdInput, startup.StartupInfo.StdOutput, startup.StartupInfo.StdErr = childHandles[0], childHandles[1], childHandles[2]
	startup.ProcThreadAttributeList = attributeList.List()
	application, err := windows.UTF16PtrFromString(args[0])
	if err != nil {
		return err
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return err
	}
	directory16, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create sandbox job: %w", err)
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return fmt.Errorf("configure sandbox job: %w", err)
	}
	processInfo := windows.ProcessInformation{}
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW)
	if err := windows.CreateProcess(application, commandLine, nil, nil, true, flags, nil, directory16, &startup.StartupInfo, &processInfo); err != nil {
		return fmt.Errorf("create AppContainer command: %w", err)
	}
	defer windows.CloseHandle(processInfo.Process)
	defer windows.CloseHandle(processInfo.Thread)
	if err := windows.AssignProcessToJobObject(job, processInfo.Process); err != nil {
		_ = windows.TerminateProcess(processInfo.Process, 1)
		return fmt.Errorf("assign command to sandbox job: %w", err)
	}
	if _, err := windows.ResumeThread(processInfo.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		return fmt.Errorf("resume AppContainer command: %w", err)
	}
	waitResult := make(chan error, 1)
	go func() {
		_, waitErr := windows.WaitForSingleObject(processInfo.Process, windows.INFINITE)
		waitResult <- waitErr
	}()
	select {
	case err := <-waitResult:
		if err != nil {
			_ = windows.TerminateJobObject(job, 1)
			return fmt.Errorf("wait for AppContainer command: %w", err)
		}
	case <-ctx.Done():
		_ = windows.TerminateJobObject(job, 1)
		if err := <-waitResult; err != nil {
			return fmt.Errorf("stop canceled AppContainer command: %w", err)
		}
		return ctx.Err()
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(processInfo.Process, &exitCode); err != nil {
		return fmt.Errorf("read AppContainer command exit code: %w", err)
	}
	if exitCode != 0 {
		return &ExitError{Code: int(exitCode)}
	}
	return nil
}

func appContainerCapabilities(network bool) ([]windows.SIDAndAttributes, func(), error) {
	if !network {
		return nil, func() {}, nil
	}
	capabilities := make([]windows.SIDAndAttributes, 0, 2)
	cleanup := func() {
	}
	for _, name := range []string{"internetClient", "privateNetworkClientServer"} {
		sid, err := deriveCapabilitySID(name)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		capabilities = append(capabilities, windows.SIDAndAttributes{Sid: sid})
	}
	return capabilities, cleanup, nil
}

func deriveCapabilitySID(name string) (*windows.SID, error) {
	proc := windows.NewLazySystemDLL("advapi32.dll").NewProc("DeriveCapabilitySidsFromName")
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var groups *windows.SIDAndAttributes
	var groupCount uint32
	var capabilities *windows.SIDAndAttributes
	var capabilityCount uint32
	hr, _, _ := proc.Call(uintptr(unsafe.Pointer(name16)), uintptr(unsafe.Pointer(&groups)), uintptr(unsafe.Pointer(&groupCount)), uintptr(unsafe.Pointer(&capabilities)), uintptr(unsafe.Pointer(&capabilityCount)))
	if hr != 0 || capabilities == nil || capabilityCount == 0 {
		return nil, fmt.Errorf("derive AppContainer capability %q failed: HRESULT 0x%08x", name, uint32(hr))
	}
	items := unsafe.Slice(capabilities, capabilityCount)
	copy, err := items[0].Sid.Copy()
	for _, item := range items {
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(item.Sid)))
	}
	if groups != nil {
		for _, group := range unsafe.Slice(groups, groupCount) {
			_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(group.Sid)))
		}
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(groups)))
	}
	_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(capabilities)))
	if err != nil {
		return nil, err
	}
	return copy, nil
}

// Helpers run in separate processes. Serialize each ACL read/modify/write so
// concurrent grants and revocations preserve the other helpers' SIDs.
func lockPathACL(path string) (func(), error) {
	runtime.LockOSThread()
	absolute, err := filepath.Abs(path)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	hash := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(absolute))))
	name, err := windows.UTF16PtrFromString(fmt.Sprintf("Global\\LuneCoreACL_%x", hash))
	if err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	mutex, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		runtime.UnlockOSThread()
		return nil, err
	}
	status, err := windows.WaitForSingleObject(mutex, 5000)
	if err != nil || (status != windows.WAIT_OBJECT_0 && status != windows.WAIT_ABANDONED) {
		_ = windows.CloseHandle(mutex)
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("lock sandbox path ACL: status=%d error=%v", status, err)
	}
	return func() { _ = windows.ReleaseMutex(mutex); _ = windows.CloseHandle(mutex); runtime.UnlockOSThread() }, nil
}
