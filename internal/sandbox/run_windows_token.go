//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

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
