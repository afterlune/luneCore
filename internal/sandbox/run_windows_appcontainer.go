//go:build windows

package sandbox

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const fileDeleteChild = 0x00000040

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
	proc := windows.NewLazySystemDLL("kernelbase.dll").NewProc("DeriveCapabilitySidsFromName")
	if err := proc.Find(); err != nil {
		return nil, fmt.Errorf("load capability API: %w", err)
	}
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	// The API returns arrays of SID pointers, not SID_AND_ATTRIBUTES structs,
	// and its return value is BOOL, not HRESULT.
	var groups, capabilities **windows.SID
	var groupCount, capabilityCount uint32
	ok, _, callErr := proc.Call(uintptr(unsafe.Pointer(name16)), uintptr(unsafe.Pointer(&groups)), uintptr(unsafe.Pointer(&groupCount)), uintptr(unsafe.Pointer(&capabilities)), uintptr(unsafe.Pointer(&capabilityCount)))
	free := func(sids **windows.SID, count uint32) {
		if sids == nil {
			return
		}
		for _, sid := range unsafe.Slice(sids, count) {
			if sid != nil {
				_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(sid)))
			}
		}
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(sids)))
	}
	defer free(groups, groupCount)
	defer free(capabilities, capabilityCount)
	if ok == 0 {
		return nil, fmt.Errorf("derive AppContainer capability %q: %w", name, callErr)
	}
	if capabilities == nil || capabilityCount == 0 || *capabilities == nil {
		return nil, fmt.Errorf("derive AppContainer capability %q returned no SID", name)
	}
	return (*capabilities).Copy()
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
