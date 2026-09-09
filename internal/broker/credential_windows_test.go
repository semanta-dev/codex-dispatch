//go:build windows

package broker

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestCredentialCreationHasProtectedCurrentUserOnlyDACL(t *testing.T) {
	file, err := createCredentialTemp(t.TempDir(), "credential-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// Inspect the original open handle: permissions must exist at creation.
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("unprotected DACL: %v %v", control, err)
	}
	acl, defaulted, err := sd.DACL()
	if err != nil || acl == nil || defaulted || acl.AceCount != 1 {
		t.Fatalf("unexpected DACL: %v %v", defaulted, err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(user.User.Sid) {
		t.Fatalf("credential readable by unexpected principal: %s", sid)
	}
}
