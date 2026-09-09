//go:build windows

package broker

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func TestCredentialDeniesSecondUnprivilegedPrincipal(t *testing.T) {
	username, password := os.Getenv("CODEX_ACL_TEST_USER"), os.Getenv("CODEX_ACL_TEST_PASSWORD")
	if username == "" || password == "" {
		t.Skip("requires dedicated local test principal; native CI provisions one")
	}
	dir := t.TempDir()
	file, err := createCredentialTemp(dir, "protected-")
	if err != nil {
		t.Fatal(err)
	}
	protected := file.Name()
	if _, err := file.WriteString("credential"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(dir, "readable-control")
	if err := os.WriteFile(control, []byte("control"), 0600); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:(A;;GR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(control, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	user16, _ := windows.UTF16PtrFromString(username)
	domain16, _ := windows.UTF16PtrFromString(".")
	password16, _ := windows.UTF16PtrFromString(password)
	advapi := windows.NewLazySystemDLL("advapi32.dll")
	var token windows.Token
	ok, _, callErr := advapi.NewProc("LogonUserW").Call(uintptr(unsafe.Pointer(user16)), uintptr(unsafe.Pointer(domain16)), uintptr(unsafe.Pointer(password16)), 2, 0, uintptr(unsafe.Pointer(&token)))
	if ok == 0 {
		t.Fatalf("log on test principal: %v", callErr)
	}
	defer token.Close()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ok, _, callErr = advapi.NewProc("ImpersonateLoggedOnUser").Call(uintptr(token))
	if ok == 0 {
		t.Fatalf("impersonate test principal: %v", callErr)
	}
	defer func() {
		if err := windows.RevertToSelf(); err != nil {
			t.Errorf("revert impersonation: %v", err)
		}
	}()
	if got, err := os.ReadFile(control); err != nil || string(got) != "control" {
		t.Fatalf("control read failed; ACL test inconclusive: %v", err)
	}
	if _, err := os.ReadFile(protected); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unprivileged credential read did not fail with permission denied: %v", err)
	}
}
