//go:build windows

// This is a native feasibility experiment, never a production verification backend.
// It executes only fixed probe commands and uses only synthetic credentials.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	securityCapabilities = 0x00020009
	allPackagesPolicy    = 0x0002000f
	tokenIsAppContainer  = 29
	tokenCapabilities    = 30
	tokenAppContainerSID = 31
	tokenIsLPAC          = 46
)

var userenv = windows.NewLazySystemDLL("userenv.dll")
var querySecurityAttributes = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQuerySecurityAttributesToken")
var machineInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsWow64Process2")
var inJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

type capabilities struct {
	SID          *windows.SID
	Capabilities *windows.SIDAndAttributes
	Count        uint32
	Reserved     uint32
}
type identity struct {
	ProcessMachine  uint16 `json:"process_machine"`
	NativeMachine   uint16 `json:"native_machine"`
	AppContainer    uint32 `json:"app_container"`
	LPAC            uint32 `json:"lpac"`
	LPACSource      string `json:"lpac_source"`
	CapabilityCount uint32 `json:"capability_count"`
	SID             string `json:"sid"`
	InJob           bool   `json:"in_job"`
	Arch            string `json:"observer_goarch"`
}
type childResult struct {
	Prerequisites               map[string]string `json:"prerequisite_access"`
	GoStandardLibraryCompatible bool              `json:"go_standard_library_compatible"`
	GoStandardLibraryError      string            `json:"go_standard_library_error,omitempty"`
	DenialErrors                map[string]string `json:"denial_errors"`
	Identity                    identity          `json:"identity"`
	ReadDenied                  bool              `json:"outside_read_denied"`
	WriteDenied                 bool              `json:"outside_write_denied"`
	NetworkDenied               bool              `json:"network_denied"`
	EnvironmentClean            bool              `json:"environment_clean"`
	Error                       string            `json:"error,omitempty"`
}
type phase struct {
	Stdout   string   `json:"stdout_tail"`
	Stderr   string   `json:"stderr_tail"`
	Name     string   `json:"name"`
	Exit     uint32   `json:"exit_code"`
	Identity identity `json:"identity"`
	Error    string   `json:"error,omitempty"`
}
type lifecycleResult struct {
	PIDs                 []int           `json:"observed_pids"`
	Identities           []identity      `json:"observed_identities"`
	ChildDiagnostic      json.RawMessage `json:"child_diagnostic,omitempty"`
	SupervisorDiagnostic json.RawMessage `json:"supervisor_diagnostic,omitempty"`
	SupervisorTerminated bool            `json:"supervisor_terminated"`
	DescendantsSignaled  int             `json:"descendants_signaled"`
	DelayedWritesAbsent  bool            `json:"delayed_writes_absent"`
	Error                string          `json:"error,omitempty"`
}
type report struct {
	GoStandardLibraryCompatible bool                   `json:"go_standard_library_compatible"`
	Breakaway                   json.RawMessage        `json:"breakaway_evidence,omitempty"`
	Lifecycle                   lifecycleResult        `json:"lifecycle"`
	Prototype                   bool                   `json:"prototype"`
	Status                      string                 `json:"status"`
	Arch                        string                 `json:"goarch"`
	Phases                      []phase                `json:"phases"`
	Children                    map[string]childResult `json:"children"`
	Errors                      []string               `json:"errors"`
	Cleanup                     []string               `json:"cleanup_errors"`
}

func tokenInfo(token windows.Token, class uint32) ([]byte, error) {
	var n uint32
	queryErr := windows.GetTokenInformation(token, class, nil, 0, &n)
	if n == 0 || n > 65536 {
		return nil, fmt.Errorf("GetTokenInformation class %d size %d: %v", class, n, queryErr)
	}
	b := make([]byte, n)
	if err := windows.GetTokenInformation(token, class, &b[0], n, &n); err != nil {
		return nil, err
	}
	return b, nil
}
func inspect(process windows.Handle) (identity, error) {
	result := identity{Arch: runtime.GOARCH}
	ok, _, machineErr := machineInfo.Call(uintptr(process), uintptr(unsafe.Pointer(&result.ProcessMachine)), uintptr(unsafe.Pointer(&result.NativeMachine)))
	if ok == 0 {
		return result, machineErr
	}
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return result, err
	}
	defer token.Close()
	// Boolean/DWORD token classes are fixed-size queries. Some Win32 token
	// classes do not support the null-buffer size-discovery pattern.
	for _, pair := range []struct {
		class uint32
		dst   *uint32
	}{{tokenIsAppContainer, &result.AppContainer}} {
		var returned uint32
		err := windows.GetTokenInformation(token, pair.class, (*byte)(unsafe.Pointer(pair.dst)), 4, &returned)
		if err != nil {
			return result, fmt.Errorf("GetTokenInformation DWORD class %d: %w", pair.class, err)
		}
	}
	var returned uint32
	lpacErr := windows.GetTokenInformation(token, tokenIsLPAC, (*byte)(unsafe.Pointer(&result.LPAC)), 4, &returned)
	if lpacErr == nil {
		result.LPACSource = "TokenIsLessPrivilegedAppContainer"
	} else {
		// Some supported Windows versions expose the enum but not this query.
		// Query the kernel-owned LPAC security attribute by exact name, as used by
		// System Informer's PhDoesTokenSecurityAttributeExist. BUFFER_TOO_SMALL on
		// this single-name null-buffer query proves the attribute exists; absence
		// or an unsupported native API fails closed. Never trust requested flags.
		name, nameErr := windows.NewNTUnicodeString("WIN://NOALLAPPPKG")
		if nameErr != nil {
			return result, nameErr
		}
		var required uint32
		status, _, _ := querySecurityAttributes.Call(uintptr(token), uintptr(unsafe.Pointer(name)), 1, 0, 0, uintptr(unsafe.Pointer(&required)))
		runtime.KeepAlive(name)
		if uint32(status) != 0xc0000023 || required == 0 || required > 65536 {
			return result, fmt.Errorf("LPAC query failed: DWORD=%v; security attribute NTSTATUS=0x%x size=%d", lpacErr, uint32(status), required)
		}
		result.LPAC = 1
		result.LPACSource = "WIN://NOALLAPPPKG"
	}
	capabilityData, capabilityErr := tokenInfo(token, tokenCapabilities)
	if capabilityErr != nil {
		return result, capabilityErr
	}
	if len(capabilityData) < 4 {
		return result, fmt.Errorf("TokenCapabilities count missing")
	}
	result.CapabilityCount = *(*uint32)(unsafe.Pointer(&capabilityData[0]))
	data, err := tokenInfo(token, tokenAppContainerSID)
	if err != nil {
		return result, err
	}
	sid := *(**windows.SID)(unsafe.Pointer(&data[0]))
	if sid != nil {
		result.SID = sid.String()
	}
	runtime.KeepAlive(data)
	var member int32
	ok, _, err = inJob.Call(uintptr(process), 0, uintptr(unsafe.Pointer(&member)))
	if ok == 0 {
		return result, err
	}
	result.InJob = member != 0
	return result, nil
}

// x/sys/windows imports net in this pinned dependency, so WSAStartup can
// poison Go poll I/O inside LPAC before main. Fixed child probes deliberately
// use synchronous Win32 I/O to retain diagnostic evidence. This does not qualify
// arbitrary Go applications; their runtime incompatibility remains a blocker.
func rawWrite(path string, data []byte) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, nil, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	for len(data) > 0 {
		var n uint32
		if err = windows.WriteFile(handle, data, &n, nil); err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
func rawExists(path string) bool {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	_, err = windows.GetFileAttributes(name)
	return err == nil
}
func socketConnect(address string) error {
	parts := strings.Split(address, ":")
	if len(parts) != 2 || parts[0] != "127.0.0.1" {
		return fmt.Errorf("invalid fixed loopback address")
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid loopback port")
	}
	var data syscall.WSAData
	if err = syscall.WSAStartup(0x202, &data); err != nil {
		return fmt.Errorf("WSAStartup error=%d: %w", err, err)
	}
	defer syscall.WSACleanup()
	socket, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer syscall.Closesocket(socket)
	return syscall.Connect(socket, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}})
}
func loopbackListener() (syscall.Handle, string, error) {
	var data syscall.WSAData
	if err := syscall.WSAStartup(0x202, &data); err != nil {
		return syscall.InvalidHandle, "", err
	}
	socket, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		syscall.WSACleanup()
		return syscall.InvalidHandle, "", err
	}
	fail := func(err error) (syscall.Handle, string, error) {
		syscall.Closesocket(socket)
		syscall.WSACleanup()
		return syscall.InvalidHandle, "", err
	}
	if err = syscall.Bind(socket, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		return fail(err)
	}
	if err = syscall.Listen(socket, 16); err != nil {
		return fail(err)
	}
	address, err := syscall.Getsockname(socket)
	if err != nil {
		return fail(err)
	}
	tcp, ok := address.(*syscall.SockaddrInet4)
	if !ok {
		return fail(fmt.Errorf("unexpected listener address"))
	}
	return socket, fmt.Sprintf("127.0.0.1:%d", tcp.Port), nil
}

// Read-only prerequisite diagnostics. Never change existing registry/file ACLs
// or dump their contents; report only API status for fixed public OS resources.
func prerequisiteAccess() map[string]string {
	result := map[string]string{}
	openKey := windows.NewLazySystemDLL("advapi32.dll").NewProc("RegOpenKeyExW")
	closeKey := windows.NewLazySystemDLL("advapi32.dll").NewProc("RegCloseKey")
	for _, path := range []string{`SYSTEM\CurrentControlSet\Services\WinSock2\Parameters`, `SYSTEM\CurrentControlSet\Services\WinSock2\Parameters\Protocol_Catalog9`, `SYSTEM\CurrentControlSet\Services\WinSock2\Parameters\NameSpace_Catalog5`, `SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`} {
		name, _ := windows.UTF16PtrFromString(path)
		var key windows.Handle
		status, _, _ := openKey.Call(uintptr(syscall.HKEY_LOCAL_MACHINE), uintptr(unsafe.Pointer(name)), 0, 0x20019, uintptr(unsafe.Pointer(&key)))
		result["HKLM\\"+path] = fmt.Sprintf("RegOpenKeyEx KEY_READ status=%d", status)
		if status == 0 {
			closeKey.Call(uintptr(key))
		}
	}
	for _, name := range []string{"ws2_32.dll", "mswsock.dll", "nsi.dll"} {
		handle, err := windows.LoadLibraryEx(name, 0, windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
		result["System32/"+name] = fmt.Sprint(err)
		if err == nil {
			windows.FreeLibrary(handle)
		}
	}
	return result
}
func checkGoStandardLibrary(scratch string) (compatible bool, diagnostic string) {
	defer func() {
		if failure := recover(); failure != nil {
			compatible = false
			diagnostic = fmt.Sprintf("Go os.WriteFile panic: %v", failure)
		}
	}()
	if err := os.WriteFile(filepath.Join(scratch, "go-standard-library.txt"), []byte("standard-library-positive"), 0600); err != nil {
		return false, err.Error()
	}
	return true, ""
}
func child(name string) int {
	if name != "native" && name != "bash" {
		return 2
	}
	result := childResult{DenialErrors: map[string]string{}}
	facts, err := inspect(windows.CurrentProcess())
	result.Identity = facts
	if err != nil || facts.AppContainer != 1 || facts.LPAC != 1 || facts.CapabilityCount != 0 || !facts.InJob || facts.SID != os.Getenv("PROBE_EXPECTED_SID") {
		fmt.Fprintln(os.Stderr, "child probe refused: confinement identity is unqualified")
		return 2
	}
	credential, _ := windows.UTF16PtrFromString(os.Getenv("PROBE_CREDENTIAL"))
	readHandle, readErr := windows.CreateFile(credential, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	result.ReadDenied = readErr != nil
	if readErr != nil {
		result.DenialErrors["read"] = readErr.Error()
	} else {
		windows.CloseHandle(readHandle)
	}
	sentinelName, _ := windows.UTF16PtrFromString(os.Getenv("PROBE_SENTINEL"))
	sentinel, writeErr := windows.CreateFile(sentinelName, windows.GENERIC_WRITE, 0, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	result.WriteDenied = writeErr != nil
	if writeErr != nil {
		result.DenialErrors["write"] = writeErr.Error()
	} else {
		windows.CloseHandle(sentinel)
	}
	err = socketConnect(os.Getenv("PROBE_LISTENER"))
	result.NetworkDenied = err != nil
	if err != nil {
		result.DenialErrors["network"] = err.Error()
	}
	result.Prerequisites = prerequisiteAccess()
	result.GoStandardLibraryCompatible, result.GoStandardLibraryError = checkGoStandardLibrary(os.Getenv("PROBE_SCRATCH"))
	result.EnvironmentClean = os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("PROBE_FAKE_PARENT_CREDENTIAL") == ""
	raw, _ := json.MarshalIndent(result, "", "  ")
	if err = rawWrite(filepath.Join(os.Getenv("PROBE_SCRATCH"), name+".json"), raw); err != nil {
		return 3
	}
	if result.Error != "" || facts.ProcessMachine != 0 || facts.AppContainer != 1 || facts.LPAC != 1 || facts.CapabilityCount != 0 || !facts.InJob || !result.ReadDenied || !result.WriteDenied || !result.NetworkDenied || !result.EnvironmentClean {
		return 1
	}
	return 0
}
func setACL(path, owner, sid string, writable bool) error {
	rights := "GRGX"
	if writable {
		rights = "FA"
	}
	ace := ""
	if sid != "" {
		ace = "(A;OICI;" + rights + ";;;" + sid + ")"
	}
	sddl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + owner + ")" + ace
	if writable {
		sddl += "S:(ML;OICI;NW;;;LW)"
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return err
	}
	if writable {
		sacl, _, e := sd.SACL()
		if e != nil {
			return e
		}
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.LABEL_SECURITY_INFORMATION, nil, nil, nil, sacl)
	}
	return nil
}
func profile(name string) (*windows.SID, error) {
	value, _ := windows.UTF16PtrFromString(name)
	var sid *windows.SID
	hr, _, _ := userenv.NewProc("CreateAppContainerProfile").Call(uintptr(unsafe.Pointer(value)), uintptr(unsafe.Pointer(value)), uintptr(unsafe.Pointer(value)), 0, 0, uintptr(unsafe.Pointer(&sid)))
	if int32(hr) < 0 {
		return nil, fmt.Errorf("CreateAppContainerProfile HRESULT 0x%x", uint32(hr))
	}
	return sid, nil
}
func envBlock(values map[string]string) []uint16 {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	block := []uint16{}
	for _, key := range keys {
		value, _ := windows.UTF16FromString(key + "=" + values[key])
		block = append(block, value...)
	}
	return append(block, 0)
}
func launch(name, exe string, args []string, cwd string, env map[string]string, sid *windows.SID) (out phase) {
	out = phase{Name: name, Exit: 0xffffffff}
	fail := func(err error) phase { out.Error = err.Error(); return out }
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fail(err)
	}
	defer windows.CloseHandle(job)
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fail(err)
	}
	handles := []windows.Handle{}
	for _, filename := range []string{name + ".stdin", name + ".stdout", name + ".stderr"} {
		path, _ := windows.UTF16PtrFromString(filepath.Join(cwd, filename))
		sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
		handle, e := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &sa, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if e != nil {
			for _, h := range handles {
				windows.CloseHandle(h)
			}
			return fail(e)
		}
		handles = append(handles, handle)
	}
	defer func() {
		for _, h := range handles {
			windows.CloseHandle(h)
		}
		for _, item := range []struct {
			filename string
			target   *string
		}{{name + ".stdout", &out.Stdout}, {name + ".stderr", &out.Stderr}} {
			file, e := os.Open(filepath.Join(cwd, item.filename))
			if e == nil {
				info, _ := file.Stat()
				if info != nil && info.Size() > 8192 {
					file.Seek(-8192, io.SeekEnd)
				}
				raw, _ := io.ReadAll(io.LimitReader(file, 8192))
				*item.target = string(raw)
				file.Close()
			}
		}
	}()
	attributes, err := windows.NewProcThreadAttributeList(3)
	if err != nil {
		return fail(err)
	}
	defer attributes.Delete()
	caps := capabilities{SID: sid}
	optOut := uint32(1)
	if err = attributes.Update(securityCapabilities, unsafe.Pointer(&caps), unsafe.Sizeof(caps)); err != nil {
		return fail(err)
	}
	if err = attributes.Update(allPackagesPolicy, unsafe.Pointer(&optOut), unsafe.Sizeof(optOut)); err != nil {
		return fail(err)
	}
	if err = attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return fail(err)
	}
	si := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{}))}, ProcThreadAttributeList: attributes.List()}
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = handles[0], handles[1], handles[2]
	app, _ := windows.UTF16PtrFromString(exe)
	line, _ := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{exe}, args...)))
	dir, _ := windows.UTF16PtrFromString(cwd)
	block := envBlock(env)
	pi := windows.ProcessInformation{}
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW)
	if err = windows.CreateProcess(app, line, nil, nil, true, flags, &block[0], dir, &si.StartupInfo, &pi); err != nil {
		return fail(fmt.Errorf("CreateProcess suspended LPAC: %w", err))
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	// The child has executed no instructions. Every failure below terminates it.
	defer func() {
		_ = windows.TerminateProcess(pi.Process, 1)
		_, _ = windows.WaitForSingleObject(pi.Process, 5000)
	}()
	if err = windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		return fail(fmt.Errorf("AssignProcessToJobObject before resume: %w", err))
	}
	defer func() { _ = windows.TerminateJobObject(job, 1) }()
	out.Identity, err = inspect(pi.Process)
	if err != nil {
		return fail(err)
	}
	if out.Identity.AppContainer != 1 || out.Identity.LPAC != 1 || out.Identity.CapabilityCount != 0 || out.Identity.SID != sid.String() || !out.Identity.InJob {
		return fail(fmt.Errorf("pre-execution token/job contract mismatch"))
	}
	if _, err = windows.ResumeThread(pi.Thread); err != nil {
		return fail(err)
	}
	state, err := windows.WaitForSingleObject(pi.Process, 15000)
	if err != nil {
		return fail(err)
	}
	if state != windows.WAIT_OBJECT_0 {
		return fail(fmt.Errorf("probe deadline exceeded"))
	}
	if err = windows.GetExitCodeProcess(pi.Process, &out.Exit); err != nil {
		return fail(err)
	}
	runtime.KeepAlive(caps)
	runtime.KeepAlive(block)
	return out
}

// All child lifecycle modes refuse to act unless already inside the expected
// LPAC and an owned job. They execute only this binary and fixed sentinel writes.
func lifecycleChild(mode string) int {
	facts, err := inspect(windows.CurrentProcess())
	if err != nil || facts.AppContainer != 1 || facts.LPAC != 1 || facts.CapabilityCount != 0 || !facts.InJob || facts.SID != os.Getenv("PROBE_EXPECTED_SID") {
		return 2
	}
	scratch := os.Getenv("PROBE_SCRATCH")
	fail := func(stage string, err error) int {
		raw, _ := json.Marshal(map[string]string{"stage": stage, "error": fmt.Sprint(err)})
		if !rawExists(filepath.Join(scratch, "lifecycle-child-error.json")) {
			rawWrite(filepath.Join(scratch, "lifecycle-child-error.json"), raw)
		}
		if handle, e := windows.GetStdHandle(windows.STD_ERROR_HANDLE); e == nil {
			var n uint32
			windows.WriteFile(handle, append(raw, '\n'), &n, nil)
		}
		return 3
	}
	if mode == "delayed" {
		raw, _ := json.Marshal(map[string]int{"pid": os.Getpid()})
		if err := rawWrite(filepath.Join(scratch, "grandchild-ready.json"), raw); err != nil {
			return fail("grandchild ready write", err)
		}
		time.Sleep(10 * time.Second)
		if rawWrite(filepath.Join(scratch, "grandchild-late.txt"), []byte("late")) != nil {
			return 3
		}
		return 0
	}
	if mode == "tree" {
		executable := os.Getenv("PROBE_HELPER")
		app, _ := windows.UTF16PtrFromString(executable)
		line, _ := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{executable, "--lifecycle-child", "delayed"}))
		si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
		pi := windows.ProcessInformation{}
		if err := windows.CreateProcess(app, line, nil, nil, false, windows.CREATE_NO_WINDOW, nil, nil, &si, &pi); err != nil {
			return fail("CreateProcess inherited LPAC child", err)
		}
		defer windows.CloseHandle(pi.Process)
		defer windows.CloseHandle(pi.Thread)
		kill := func() { windows.TerminateProcess(pi.Process, 1); windows.WaitForSingleObject(pi.Process, 5000) }
		deadline := time.Now().Add(4 * time.Second)
		for {
			if rawExists(filepath.Join(scratch, "grandchild-ready.json")) {
				break
			}
			if time.Now().After(deadline) {
				kill()
				return fail("grandchild readiness timeout", nil)
			}
			time.Sleep(20 * time.Millisecond)
		}
		raw, _ := json.Marshal([]int{os.Getpid(), int(pi.ProcessId)})
		if err := rawWrite(filepath.Join(scratch, "lifecycle-ready.json"), raw); err != nil {
			kill()
			return fail("parent ready write", err)
		}
		time.Sleep(10 * time.Second)
		rawWrite(filepath.Join(scratch, "parent-late.txt"), []byte("late"))
		windows.WaitForSingleObject(pi.Process, 5000)
		return 0
	}
	if mode == "breakaway" {
		executable := os.Getenv("PROBE_HELPER")
		app, _ := windows.UTF16PtrFromString(executable)
		line, _ := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{executable, "--lifecycle-child", "delayed"}))
		si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
		pi := windows.ProcessInformation{}
		err := windows.CreateProcess(app, line, nil, nil, false, windows.CREATE_SUSPENDED|windows.CREATE_BREAKAWAY_FROM_JOB, nil, nil, &si, &pi)
		if err == nil {
			windows.TerminateProcess(pi.Process, 1)
			windows.WaitForSingleObject(pi.Process, 5000)
			windows.CloseHandle(pi.Thread)
			windows.CloseHandle(pi.Process)
		}
		denied := err == windows.ERROR_ACCESS_DENIED
		raw, _ := json.Marshal(map[string]any{"denied": denied, "error": fmt.Sprint(err), "identity": facts})
		if rawWrite(filepath.Join(scratch, "breakaway.json"), raw) != nil {
			return 3
		}
		if !denied {
			return 1
		}
		return 0
	}
	return 2
}

// The trusted supervisor owns the job handle. The outside observer force-kills
// it after both descendants prove readiness, exercising kernel kill-on-close.
func supervisor() int {
	sid, err := windows.StringToSid(os.Getenv("PROBE_EXPECTED_SID"))
	if err != nil {
		return 2
	}
	env := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	result := launch("lifecycle-tree", os.Getenv("PROBE_HELPER"), []string{"--lifecycle-child", "tree"}, os.Getenv("PROBE_SCRATCH"), env, sid)
	raw, _ := json.Marshal(result)
	os.WriteFile(filepath.Join(os.Getenv("PROBE_SCRATCH"), "supervisor-result.json"), raw, 0600)
	if result.Error != "" || result.Exit != 0 {
		return 1
	}
	return 0
}
func crashProbe(helper, scratch string, env map[string]string, sid *windows.SID) (result lifecycleResult) {
	defer func() {
		childRaw, childErr := os.ReadFile(filepath.Join(scratch, "lifecycle-child-error.json"))
		if childErr == nil && json.Valid(childRaw) {
			result.ChildDiagnostic = childRaw
		}
		raw, err := os.ReadFile(filepath.Join(scratch, "supervisor-result.json"))
		if err == nil && json.Valid(raw) {
			result.SupervisorDiagnostic = raw
		}
	}()
	command := exec.Command(helper, "--supervisor")
	for key, value := range env {
		command.Env = append(command.Env, key+"="+value)
	}
	command.Dir = scratch
	if err := command.Start(); err != nil {
		result.Error = err.Error()
		return
	}
	reaped := false
	defer func() {
		if !reaped {
			command.Process.Kill()
			command.Wait()
		}
	}()
	deadline := time.Now().Add(7 * time.Second)
	var pids []int
	for {
		raw, err := os.ReadFile(filepath.Join(scratch, "lifecycle-ready.json"))
		if err == nil && json.Unmarshal(raw, &pids) == nil && len(pids) == 2 && pids[0] > 0 && pids[1] > 0 && pids[0] != pids[1] {
			break
		}
		if time.Now().After(deadline) {
			result.Error = "descendant readiness timeout"
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	handles := []windows.Handle{}
	defer func() {
		for _, h := range handles {
			windows.TerminateProcess(h, 1)
			windows.WaitForSingleObject(h, 5000)
			windows.CloseHandle(h)
		}
	}()
	for _, pid := range pids {
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			result.Error = "open owned descendant: " + err.Error()
			return
		}
		facts, err := inspect(handle)
		if err != nil || facts.SID != sid.String() || facts.AppContainer != 1 || facts.LPAC != 1 || !facts.InJob {
			windows.CloseHandle(handle)
			result.Error = "descendant identity mismatch"
			return
		}
		handles = append(handles, handle)
		result.PIDs = append(result.PIDs, pid)
		result.Identities = append(result.Identities, facts)
	}
	if err := command.Process.Kill(); err != nil {
		result.Error = err.Error()
		return
	}
	_ = command.Wait()
	reaped = true
	result.SupervisorTerminated = true
	for _, handle := range handles {
		state, err := windows.WaitForSingleObject(handle, 5000)
		if err != nil || state != windows.WAIT_OBJECT_0 {
			result.Error = "descendant survived supervisor termination"
			return
		}
		result.DescendantsSignaled++
	}
	result.DelayedWritesAbsent = true
	for _, name := range []string{"parent-late.txt", "grandchild-late.txt"} {
		if _, err := os.Stat(filepath.Join(scratch, name)); !os.IsNotExist(err) {
			result.DelayedWritesAbsent = false
			result.Error = "delayed write present or unauditable"
		}
	}
	return
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	copied, copyErr := io.Copy(out, io.LimitReader(in, 128*1024*1024+1))
	err = copyErr
	if copied > 128*1024*1024 {
		err = fmt.Errorf("runtime file grew beyond quota")
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func copyRuntime(source, target string) error {
	deadline := time.Now().Add(90 * time.Second)
	var total int64
	count := 0
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("runtime copy deadline")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		// Reject all reparse points, including junctions, rather than following them.
		path16, _ := windows.UTF16PtrFromString(path)
		attrs, err := windows.GetFileAttributes(path16)
		if err != nil {
			return err
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("runtime contains reparse point: %s", path)
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		if !info.Mode().IsRegular() || info.Size() > 128*1024*1024 {
			return fmt.Errorf("unsupported runtime file")
		}
		total += info.Size()
		count++
		if total > 1024*1024*1024 || count > 50000 {
			return fmt.Errorf("runtime copy quota")
		}
		return copyFile(path, dest)
	})
}
func run(gitRoot string) (r *report) {
	os.Setenv("PROBE_FAKE_PARENT_CREDENTIAL", "FAKE_PARENT_ONLY")
	r = &report{Prototype: true, Status: "NO-GO", Arch: runtime.GOARCH, Children: map[string]childResult{}, Errors: []string{}, Cleanup: []string{}}
	fail := func(err error) *report { r.Errors = append(r.Errors, err.Error()); return r }
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return fail(err)
	}
	name := "codex.probe." + hex.EncodeToString(random)
	sid, err := profile(name)
	if err != nil {
		return fail(err)
	}
	defer windows.FreeSid(sid)
	defer func() {
		value, _ := windows.UTF16PtrFromString(name)
		hr, _, _ := userenv.NewProc("DeleteAppContainerProfile").Call(uintptr(unsafe.Pointer(value)))
		if int32(hr) < 0 {
			r.Cleanup = append(r.Cleanup, fmt.Sprintf("DeleteAppContainerProfile HRESULT 0x%x", uint32(hr)))
			r.Status = "NO-GO"
		}
	}()
	root, err := os.MkdirTemp("", "codex-lpac-probe-")
	if err != nil {
		return fail(err)
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil {
			r.Cleanup = append(r.Cleanup, err.Error())
			r.Status = "NO-GO"
		}
	}()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fail(err)
	}
	owner := user.User.Sid.String()
	if err = setACL(root, owner, sid.String(), false); err != nil {
		return fail(err)
	}
	scratch, outside := filepath.Join(root, "scratch"), filepath.Join(root, "outside")
	for _, dir := range []string{scratch, outside} {
		if err = os.Mkdir(dir, 0700); err != nil {
			return fail(err)
		}
	}
	if err = setACL(scratch, owner, sid.String(), true); err != nil {
		return fail(err)
	}
	if err = setACL(outside, owner, "", false); err != nil {
		return fail(err)
	}
	credential, sentinel := filepath.Join(outside, "credential.txt"), filepath.Join(outside, "sentinel.txt")
	if err = os.WriteFile(credential, []byte("FAKE_REVIEW_CREDENTIAL_ONLY"), 0600); err != nil {
		return fail(err)
	}
	current, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	helper := filepath.Join(root, "probe.exe")
	if err = copyFile(current, helper); err != nil {
		return fail(err)
	}
	listener, listenerAddress, err := loopbackListener()
	if err != nil {
		return fail(err)
	}
	defer syscall.Closesocket(listener)
	defer syscall.WSACleanup()
	if err = socketConnect(listenerAddress); err != nil {
		return fail(fmt.Errorf("host loopback positive control: %w", err))
	}
	system := os.Getenv("SystemRoot")
	if system == "" {
		return fail(fmt.Errorf("missing trusted SystemRoot"))
	}
	env := map[string]string{"SystemRoot": system, "WINDIR": system, "PATH": filepath.Join(system, "System32"), "HOME": scratch, "USERPROFILE": scratch, "TEMP": scratch, "TMP": scratch, "LOCALAPPDATA": scratch, "PROBE_SCRATCH": scratch, "PROBE_CREDENTIAL": credential, "PROBE_SENTINEL": sentinel, "PROBE_LISTENER": listenerAddress, "PROBE_HELPER": helper, "PROBE_EXPECTED_SID": sid.String()}
	r.Phases = append(r.Phases, launch("cmd", filepath.Join(system, "System32", "cmd.exe"), []string{"/d", "/c", "echo native-cmd-ok>cmd-positive.txt"}, scratch, env, sid))
	r.Phases = append(r.Phases, launch("native-denials", helper, []string{"--child", "native"}, scratch, env, sid))
	r.Phases = append(r.Phases, launch("breakaway", helper, []string{"--lifecycle-child", "breakaway"}, scratch, env, sid))
	r.Lifecycle = crashProbe(helper, scratch, env, sid)
	if r.Lifecycle.Error != "" || !r.Lifecycle.SupervisorTerminated || r.Lifecycle.DescendantsSignaled != 2 || !r.Lifecycle.DelayedWritesAbsent {
		r.Errors = append(r.Errors, "parent-death lifecycle probe failed")
	}
	breakawayRaw, breakawayErr := os.ReadFile(filepath.Join(scratch, "breakaway.json"))
	if json.Valid(breakawayRaw) {
		r.Breakaway = breakawayRaw
	}
	var breakaway struct {
		Denied bool `json:"denied"`
	}
	if breakawayErr != nil || json.Unmarshal(breakawayRaw, &breakaway) != nil || !breakaway.Denied {
		r.Errors = append(r.Errors, "breakaway proof missing or failed")
	}
	if gitRoot == "" {
		r.Errors = append(r.Errors, "Git runtime source missing")
	} else {
		portable := filepath.Join(root, "git")
		if err = copyRuntime(gitRoot, portable); err != nil {
			r.Errors = append(r.Errors, "Git copy: "+err.Error())
		} else {
			env["PATH"] = filepath.Join(portable, "usr", "bin") + ";" + filepath.Join(system, "System32")
			// Fixed commands only. No repository content or user command is executed.
			command := "printf bash-ok > bash-positive.txt; (printf child-ok > bash-child-positive.txt); exec \"$PROBE_HELPER\" --child bash"
			r.Phases = append(r.Phases, launch("bash", filepath.Join(portable, "usr", "bin", "bash.exe"), []string{"--noprofile", "--norc", "-c", command}, scratch, env, sid))
		}
	}
	for _, name := range []string{"native", "bash"} {
		data, err := os.ReadFile(filepath.Join(scratch, name+".json"))
		if err != nil {
			r.Errors = append(r.Errors, "missing "+name+" child evidence: "+err.Error())
			continue
		}
		var result childResult
		if err = json.Unmarshal(data, &result); err != nil {
			r.Errors = append(r.Errors, err.Error())
			continue
		}
		r.Children[name] = result
		if result.Error != "" || result.Identity.ProcessMachine != 0 || result.Identity.AppContainer != 1 || result.Identity.LPAC != 1 || result.Identity.CapabilityCount != 0 || result.Identity.SID != sid.String() || !result.Identity.InJob || !result.ReadDenied || !result.WriteDenied || !result.NetworkDenied || !result.EnvironmentClean {
			r.Errors = append(r.Errors, name+" boundary checks failed")
		}
	}
	r.GoStandardLibraryCompatible = r.Children["native"].GoStandardLibraryCompatible && r.Children["bash"].GoStandardLibraryCompatible
	if !r.GoStandardLibraryCompatible {
		r.Errors = append(r.Errors, "Go standard-library compatibility unproven or failed; raw Win32 diagnostics do not qualify verification")
	}
	for _, name := range []string{"cmd-positive.txt", "bash-positive.txt", "bash-child-positive.txt"} {
		if _, err = os.Stat(filepath.Join(scratch, name)); err != nil {
			r.Errors = append(r.Errors, "missing positive control "+name)
		}
	}
	if _, err = os.Stat(sentinel); !os.IsNotExist(err) {
		r.Errors = append(r.Errors, "outside sentinel unexpectedly present or unauditable")
	}
	for _, p := range r.Phases {
		if p.Error != "" || p.Exit != 0 {
			r.Errors = append(r.Errors, p.Name+" failed")
		}
	}
	if len(r.Errors) == 0 && len(r.Phases) == 4 && len(r.Children) == 2 {
		r.Status = "PROBE-PASS"
	}
	return r
}
func main() {
	supervisorMode := flag.Bool("supervisor", false, "internal trusted lifecycle supervisor")
	lifecycleMode := flag.String("lifecycle-child", "", "internal fixed lifecycle probe")
	childName := flag.String("child", "", "internal fixed child probe")
	gitRoot := flag.String("git-root", "", "installed Git root, copied before execution")
	out := flag.String("out", "windows-sandbox-probe.json", "evidence output")
	flag.Parse()
	if *supervisorMode {
		os.Exit(supervisor())
	}
	if *lifecycleMode != "" {
		os.Exit(lifecycleChild(*lifecycleMode))
	}
	if *childName != "" {
		os.Exit(child(*childName))
	}
	r := run(*gitRoot)
	raw, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(*out, append(raw, '\n'), 0600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(raw))
	if !strings.EqualFold(r.Status, "PROBE-PASS") {
		os.Exit(1)
	}
}
