//go:build windows

package sandbox

import (
	"testing"

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
