//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const procThreadAttributeSecurityCapabilities = 0x00020009

type securityCapabilities struct {
	AppContainerSID *windows.SID
	Capabilities    *windows.SIDAndAttributes
	CapabilityCount uint32
	Reserved        uint32
}

func runRestrictedUserProcess(ctx context.Context, args []string, directory string, envOverrides map[string]string) error {
	token, err := restrictedCurrentUserToken()
	if err != nil {
		return err
	}
	defer token.Close()
	return runWindowsChild(ctx, args, directory, token, nil, envOverrides)
}

func runWindowsChild(ctx context.Context, args []string, directory string, token windows.Token, security *securityCapabilities, envOverrides map[string]string) error {
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
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED)
	var envBlockPtr *uint16
	if len(envOverrides) > 0 {
		block, blockErr := windowsEnvironmentBlock(envOverrides)
		if blockErr != nil {
			return blockErr
		}
		envBlockPtr = &block[0]
		flags |= windows.CREATE_UNICODE_ENVIRONMENT
	}
	if token != 0 {
		err = windows.CreateProcessAsUser(token, application, commandLine, nil, nil, true, flags, envBlockPtr, directory16, &startup.StartupInfo, &processInfo)
	} else {
		err = windows.CreateProcess(application, commandLine, nil, nil, true, flags, envBlockPtr, directory16, &startup.StartupInfo, &processInfo)
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

func runAppContainerProcess(ctx context.Context, sid *windows.SID, args []string, directory, tempDir string, network bool, toolchainRoots []string) error {
	for _, name := range []string{"go-cache", "go-tmp", "go-mod-cache", "go-path", "cache"} {
		if err := os.MkdirAll(filepath.Join(tempDir, name), 0700); err != nil {
			return fmt.Errorf("create sandbox cache directory: %w", err)
		}
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
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED)
	environment, err := windowsEnvironmentBlock(map[string]string{
		"TEMP": tempDir, "TMP": tempDir, "HOME": tempDir,
		"PATH":    appendSearchPaths(os.Getenv("PATH"), commandToolchainSearchPaths(toolchainRoots), ";"),
		"GOCACHE": filepath.Join(tempDir, "go-cache"), "GOTMPDIR": filepath.Join(tempDir, "go-tmp"),
		"GOMODCACHE": filepath.Join(tempDir, "go-mod-cache"), "GOPATH": filepath.Join(tempDir, "go-path"),
		"GOENV": "off", "GOTOOLCHAIN": "local", "XDG_CACHE_HOME": filepath.Join(tempDir, "cache"),
	})
	if err != nil {
		return err
	}
	flags |= windows.CREATE_UNICODE_ENVIRONMENT
	if err := windows.CreateProcess(application, commandLine, nil, nil, true, flags, &environment[0], directory16, &startup.StartupInfo, &processInfo); err != nil {
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

func windowsEnvironmentBlock(overrides map[string]string) ([]uint16, error) {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		separator := strings.IndexByte(entry, '=')
		if strings.HasPrefix(entry, "=") {
			separator = strings.IndexByte(entry[1:], '=') + 1
		}
		if separator <= 0 || separator >= len(entry) {
			continue
		}
		name := entry[:separator]
		values[strings.ToUpper(name)] = entry
	}
	for name, value := range overrides {
		if strings.ContainsAny(name+value, "\x00") {
			return nil, fmt.Errorf("sandbox environment variable %q contains a null byte", name)
		}
		values[strings.ToUpper(name)] = name + "=" + value
	}
	entries := make([]string, 0, len(values))
	for _, entry := range values {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return strings.ToUpper(entries[i]) < strings.ToUpper(entries[j]) })
	block := make([]uint16, 0, len(entries)*64+1)
	for _, entry := range entries {
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, fmt.Errorf("encode sandbox environment: %w", err)
		}
		block = append(block, encoded[:len(encoded)-1]...)
		block = append(block, 0)
	}
	block = append(block, 0)
	if len(entries) == 0 {
		block = append(block, 0)
	}
	return block, nil
}
