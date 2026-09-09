//go:build windows

package appserver

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestJobProcessHelper(t *testing.T) {
	switch os.Getenv("CODEX_JOB_HELPER") {
	case "leaf":
		time.Sleep(time.Hour)
	case "parent":
		// Match app-server behavior: descendants start only after the handshake,
		// when the broker has assigned the child to its Job Object.
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			os.Exit(2)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestJobProcessHelper$")
		child.Env = append(os.Environ(), "CODEX_JOB_HELPER=leaf")
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(os.Getenv("CODEX_JOB_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0600); err != nil {
			os.Exit(4)
		}
		_ = child.Wait()
	case "owner":
		command := exec.Command(os.Args[0], "-test.run=^TestJobProcessHelper$")
		command.Env = append(os.Environ(), "CODEX_JOB_HELPER=parent")
		input, _ := command.StdinPipe()
		if err := command.Start(); err != nil {
			os.Exit(5)
		}
		control := newChildController()
		if err := control.arm(command); err != nil {
			os.Exit(6)
		}
		if err := os.WriteFile(os.Getenv("CODEX_JOB_PARENT_FILE"), []byte(strconv.Itoa(command.Process.Pid)), 0600); err != nil {
			os.Exit(7)
		}
		_, _ = fmt.Fprintln(input, "go")
		time.Sleep(time.Hour)
	}
}

func waitJobPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child PID evidence not written: %s", path)
	return 0
}

func TestWindowsJobTerminatesDescendants(t *testing.T) {
	for _, mode := range []string{"terminate", "close", "owner-crash"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			leafFile := filepath.Join(dir, "leaf.pid")
			parentFile := filepath.Join(dir, "parent.pid")
			role := "parent"
			if mode == "owner-crash" {
				role = "owner"
			}
			command := exec.Command(os.Args[0], "-test.run=^TestJobProcessHelper$")
			command.Env = append(os.Environ(), "CODEX_JOB_HELPER="+role, "CODEX_JOB_PID_FILE="+leafFile, "CODEX_JOB_PARENT_FILE="+parentFile)
			input, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer command.Process.Kill()
			control := newChildController()
			defer control.close()
			if mode != "owner-crash" {
				if err := control.arm(command); err != nil {
					t.Fatal(err)
				}
				_, _ = fmt.Fprintln(input, "go")
			}
			leaf := waitJobPID(t, leafFile)
			parent := command.Process.Pid
			if mode == "owner-crash" {
				parent = waitJobPID(t, parentFile)
			}
			handles := []windows.Handle{}
			for _, pid := range []int{parent, leaf} {
				h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
				if err != nil {
					t.Fatal(err)
				}
				defer windows.CloseHandle(h)
				handles = append(handles, h)
			}
			switch mode {
			case "terminate":
				if err := control.signal(parent, childSIGKILL); err != nil {
					t.Fatal(err)
				}
			case "close":
				control.close()
			case "owner-crash":
				if err := command.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			}
			_ = command.Wait()
			for _, handle := range handles {
				result, err := windows.WaitForSingleObject(handle, 5000)
				if err != nil || result != windows.WAIT_OBJECT_0 {
					t.Fatalf("descendant survived %s: %d %v", mode, result, err)
				}
			}
		})
	}
}
